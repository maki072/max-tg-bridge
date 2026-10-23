package store

import (
	"database/sql"
	"log"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo
)

// The delivered-messages registry: which MAX messages were already mirrored
// into Telegram. It lives in its own SQLite file next to the state file instead
// of in state.json, because it must (a) never forget — a bounded list lets old
// messages be posted a second time when a resync window reaches back past it —
// (b) cost O(1) per message instead of rewriting the whole state file, and
// (c) let two goroutines race for the same message with one winner.
//
// A message goes pending -> delivered: Claim reserves it, Done confirms it,
// Release gives it back so the next resync retries it. A pending claim that
// nobody confirmed for claimStale (a crash mid-delivery) can be claimed again.

const (
	claimStale     = 10 * time.Minute
	deliveredState = 1
	pendingState   = 0
)

const deliveredSchema = `
CREATE TABLE IF NOT EXISTS delivered (
	msg   TEXT PRIMARY KEY,
	chat  TEXT NOT NULL DEFAULT '',
	state INTEGER NOT NULL,
	ts    INTEGER NOT NULL
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS delivered_chat ON delivered(chat);`

// openDelivered opens (creating) the registry next to the state file and, the
// first time, imports the old bounded Seen list from state.json.
func (s *Store) openDelivered() error {
	path := filepath.Join(filepath.Dir(s.path), "delivered.db")
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1) // one writer; the queries are tiny
	if _, err := db.Exec(deliveredSchema); err != nil {
		db.Close()
		return err
	}
	s.db = db

	if len(s.Seen) > 0 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		for _, id := range s.Seen {
			id = trimSeenPrefix(id)
			if _, err := tx.Exec(`INSERT OR IGNORE INTO delivered(msg, chat, state, ts) VALUES(?, '', ?, ?)`, id, deliveredState, now); err != nil {
				tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		log.Printf("[store] imported %d seen message ids into delivered.db", len(s.Seen))
		s.Seen, s.seenSet = nil, make(map[string]bool)
		_ = s.save()
	}
	return nil
}

func trimSeenPrefix(id string) string {
	const p = "max:"
	if len(id) > len(p) && id[:len(p)] == p {
		return id[len(p):]
	}
	return id
}

// Claim reserves a MAX message for delivery. It reports true when the caller
// now owns the delivery (and must call Done or Release), false when the message
// is already delivered or another delivery is in flight.
func (s *Store) Claim(chat, msg string) bool {
	if msg == "" {
		return true // nothing to dedupe on
	}
	now := time.Now().Unix()
	res, err := s.db.Exec(`INSERT OR IGNORE INTO delivered(msg, chat, state, ts) VALUES(?, ?, ?, ?)`, msg, chat, pendingState, now)
	if err != nil {
		log.Printf("[store] claim %s failed: %v", msg, err)
		return false
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return true
	}
	// Already there: only a stale pending claim may be taken over.
	res, err = s.db.Exec(`UPDATE delivered SET ts = ?, chat = ? WHERE msg = ? AND state = ? AND ts < ?`,
		now, chat, msg, pendingState, now-int64(claimStale/time.Second))
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n == 1
}

// Done confirms a claimed message as delivered.
func (s *Store) Done(chat, msg string) {
	if msg == "" {
		return
	}
	if _, err := s.db.Exec(`UPDATE delivered SET state = ?, chat = ?, ts = ? WHERE msg = ?`, deliveredState, chat, time.Now().Unix(), msg); err != nil {
		log.Printf("[store] done %s failed: %v", msg, err)
	}
}

// Release drops a pending claim whose delivery failed, so a later resync tries
// the message again.
func (s *Store) Release(msg string) {
	if msg == "" {
		return
	}
	_, _ = s.db.Exec(`DELETE FROM delivered WHERE msg = ? AND state = ?`, msg, pendingState)
}

// MarkDelivered records a message that must never be mirrored (our own echo,
// or history we deliberately skip). Idempotent.
func (s *Store) MarkDelivered(chat, msg string) {
	if msg == "" {
		return
	}
	_, err := s.db.Exec(`INSERT INTO delivered(msg, chat, state, ts) VALUES(?, ?, ?, ?)
		ON CONFLICT(msg) DO UPDATE SET state = ?, chat = ?`, msg, chat, deliveredState, time.Now().Unix(), deliveredState, chat)
	if err != nil {
		log.Printf("[store] mark %s failed: %v", msg, err)
	}
}

// IsDelivered reports whether a message is recorded as delivered.
func (s *Store) IsDelivered(msg string) bool {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM delivered WHERE msg = ? AND state = ?`, msg, deliveredState).Scan(&one)
	return err == nil
}

// ChatTracked reports whether anything was ever recorded for a chat, either in
// the registry or as a pre-registry watermark. A mapped chat that is not
// tracked has unknown history, which a resync must not blindly re-post.
func (s *Store) ChatTracked(chat string) bool {
	s.mu.Lock()
	wm := s.LastSync[chat]
	s.mu.Unlock()
	if wm > 0 {
		return true
	}
	var one int
	return s.db.QueryRow(`SELECT 1 FROM delivered WHERE chat = ? LIMIT 1`, chat).Scan(&one) == nil
}

// forgetChat drops a chat's rows (its topic was removed; a recreated topic
// backfills its history afresh).
func (s *Store) forgetChat(chat string) {
	_, _ = s.db.Exec(`DELETE FROM delivered WHERE chat = ?`, chat)
}

// Close releases the registry's file handles.
func (s *Store) Close() {
	if s.db != nil {
		_ = s.db.Close()
	}
}
