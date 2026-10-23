package bridge

import (
	"log"
	"sort"
	"strconv"
	"sync"
	"time"

	"maxtg/internal/maxapi"
)

// Delivering MAX history to Telegram without remembering messages.
//
// Each chat has a cursor (store.LastSync): the MAX time of the newest message
// already delivered. The invariant is that the cursor never passes a message
// that was not delivered — it moves only after a successful post, and only while
// the chat is "caught up" — so a catch-up after downtime asks MAX for exactly the
// messages newer than the cursor, and nothing is stored per message.
//
//   - Everything that writes into a chat's topic runs under that chat's lock
//     (lockChat): one at a time per chat, different chats side by side. Without
//     it a later message posted first would move the cursor past an earlier one
//     still uploading, and if that one failed it would never be fetched again.
//   - A chat is caught up once its history since the cursor is in Telegram in
//     this MAX session (ensureCaughtUp). The set is emptied on every new MAX
//     socket, and a live message for a chat outside it first catches the chat up.
//   - A message already linked to a Telegram message (store.MsgLinks, which the
//     bridge keeps anyway for replies and reactions) is never posted again: that
//     is what lets a live push and a catch-up both see the same message, and
//     what skips the echo of our own sends.
//   - A failure stops the chat at the failed message, leaves the cursor before
//     it, and schedules a retry with growing pauses. A message that keeps
//     failing (see strikeOut) is eventually skipped so it cannot block its chat.

const (
	historyPageSize = 100 // messages per GET_HISTORY page when catching up
	historyMaxPages = 60  // hard stop for one chat's catch-up (6000 messages)

	strikeLimit = 3
	strikeSpan  = time.Hour // failures must span this long: a short outage is not "this message is broken"
	strikeCap   = 500

	retryBase = 30 * time.Second
	retryMax  = 15 * time.Minute
)

type strikeRec struct {
	n     int
	first time.Time
}

// chatSync is the bookkeeping behind the per-chat cursor.
type chatSync struct {
	mu      sync.Mutex
	locks   map[string]*sync.Mutex
	caught  map[string]bool
	strikes map[string]strikeRec
	retry   *time.Timer
	attempt int
}

// lockChat serializes work on one MAX chat; call the returned func to release.
func (b *Bridge) lockChat(chatKey string) func() {
	b.cs.mu.Lock()
	if b.cs.locks == nil {
		b.cs.locks = make(map[string]*sync.Mutex)
	}
	l := b.cs.locks[chatKey]
	if l == nil {
		l = &sync.Mutex{}
		b.cs.locks[chatKey] = l
	}
	b.cs.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (b *Bridge) isCaught(chatKey string) bool {
	b.cs.mu.Lock()
	defer b.cs.mu.Unlock()
	return b.cs.caught[chatKey]
}

func (b *Bridge) markCaught(chatKey string) {
	b.cs.mu.Lock()
	defer b.cs.mu.Unlock()
	if b.cs.caught == nil {
		b.cs.caught = make(map[string]bool)
	}
	b.cs.caught[chatKey] = true
}

func (b *Bridge) markDirty(chatKey string) {
	b.cs.mu.Lock()
	defer b.cs.mu.Unlock()
	delete(b.cs.caught, chatKey)
}

// resetCaught forgets every chat: after a new MAX socket each one must be
// caught up again, because the push stream had a gap.
func (b *Bridge) resetCaught() {
	b.cs.mu.Lock()
	defer b.cs.mu.Unlock()
	b.cs.caught = make(map[string]bool)
}

// strikeOut records one more failed delivery of a message and reports whether
// it has failed often enough, over a long enough time, to be given up on.
func (b *Bridge) strikeOut(id string) bool {
	if id == "" {
		return false
	}
	b.cs.mu.Lock()
	defer b.cs.mu.Unlock()
	if b.cs.strikes == nil {
		b.cs.strikes = make(map[string]strikeRec)
	}
	rec := b.cs.strikes[id]
	if rec.n == 0 {
		rec.first = time.Now()
	}
	rec.n++
	if len(b.cs.strikes) >= strikeCap { // bounded: forget an arbitrary old entry
		for k := range b.cs.strikes {
			delete(b.cs.strikes, k)
			break
		}
	}
	b.cs.strikes[id] = rec
	return rec.n >= strikeLimit && time.Since(rec.first) >= strikeSpan
}

func (b *Bridge) clearStrike(id string) {
	b.cs.mu.Lock()
	defer b.cs.mu.Unlock()
	delete(b.cs.strikes, id)
}

// retryDelay is the pause before the attempt-th retry: 30s, 1m, 2m, … 15m.
func retryDelay(attempt int) time.Duration {
	if attempt > 10 {
		attempt = 10
	}
	d := retryBase << attempt
	if d > retryMax {
		d = retryMax
	}
	return d
}

// requestRetry schedules another catch-up pass, unless one is already pending.
func (b *Bridge) requestRetry() {
	b.cs.mu.Lock()
	defer b.cs.mu.Unlock()
	if b.cs.retry != nil {
		return
	}
	d := retryDelay(b.cs.attempt)
	b.cs.attempt++
	log.Printf("[sync] some messages could not be delivered; retrying in %v", d)
	b.cs.retry = time.AfterFunc(d, func() {
		b.cs.mu.Lock()
		b.cs.retry = nil
		b.cs.mu.Unlock()
		b.runCatchUp(false)
	})
}

// SyncAllChats re-catches-up every mapped chat from its cursor. Wired as the MAX
// client's OnConnect hook (see app.go): it runs after the first connect and after
// every reconnect, because pushes only cover messages sent while connected.
// Also behind the panel's "resync" button.
func (b *Bridge) SyncAllChats() { b.runCatchUp(true) }

// runCatchUp walks the mapped chats; reset forgets which ones were caught up.
func (b *Bridge) runCatchUp(reset bool) {
	if b.bot.GroupID() == 0 {
		return
	}
	if !b.syncing.CompareAndSwap(false, true) {
		return // a pass is already running (e.g. two reconnects in a row)
	}
	defer b.syncing.Store(false)
	if reset {
		b.resetCaught()
	}

	delivered, failed := 0, false
	for chatKey, threadID := range b.st.AllTopics() {
		chatID, err := strconv.ParseInt(chatKey, 10, 64)
		if err != nil || b.st.IsBanned(chatKey) {
			continue // "pending:<user>" topics have no MAX chat yet
		}
		unlock := b.lockChat(chatKey)
		n, ok := b.ensureCaughtUp(chatKey, chatID, threadID)
		unlock()
		delivered += n
		if !ok {
			failed = true
		}
	}
	if delivered > 0 {
		log.Printf("[sync] caught up: %d missed message(s) delivered", delivered)
	}
	if failed {
		b.requestRetry()
		return
	}
	b.cs.mu.Lock()
	b.cs.attempt = 0
	b.cs.mu.Unlock()
}

// ensureCaughtUp brings a chat's history since its cursor into its topic and
// puts the chat in the caught-up set. The caller holds the chat's lock. Returns
// how many messages it handled and whether the chat is now caught up.
func (b *Bridge) ensureCaughtUp(chatKey string, chatID int64, threadID int) (int, bool) {
	if b.isCaught(chatKey) {
		return 0, true
	}
	limit := b.cfg.SyncCount
	if limit <= 0 { // catching up is switched off: live delivery only
		b.markCaught(chatKey)
		return 0, true
	}
	cursor := b.st.LastSyncTime(chatKey)
	if cursor == 0 {
		// Nothing says how far this chat's history was delivered (a state file
		// restored without cursors, a topic made by hand). Re-posting "everything"
		// would flood the topic, so the history that exists now counts as delivered.
		b.baselineChat(chatKey, chatID)
		b.markCaught(chatKey)
		return 0, true
	}
	msgs, err := collectSince(func(from int64, n int) ([]maxapi.IncomingMessage, int64, int, error) {
		return b.max.HistoryPage(chatID, from, n)
	}, cursor, limit)
	if err != nil {
		log.Printf("[sync] reading the history of chat %s failed: %v", chatKey, err)
		return 0, false
	}
	for i, m := range msgs {
		if !b.deliverOne(threadID, chatKey, m, true, true) {
			return i, false
		}
	}
	b.markCaught(chatKey)
	return len(msgs), true
}

// baselineChat sets a chat's cursor to its newest message without posting
// anything.
func (b *Bridge) baselineChat(chatKey string, chatID int64) {
	page, _, _, err := b.max.HistoryPage(chatID, 0, 20)
	var newest int64
	for _, m := range page {
		if m.Time > newest {
			newest = m.Time
		}
	}
	if err != nil || newest == 0 {
		newest = time.Now().UnixMilli()
	}
	b.st.BumpLastSync(chatKey, newest)
	log.Printf("[sync] chat %s had no history cursor; starting from its newest message instead of re-posting its history", chatKey)
}

// deliverOne posts one MAX message into a topic unless it is already there (it
// has a linked Telegram message). resolve fetches attachment URLs (history
// messages come without them); advance moves the chat's cursor past the message
// once it is delivered. Reports false when delivery failed and should be retried.
func (b *Bridge) deliverOne(threadID int, chatKey string, m maxapi.IncomingMessage, resolve, advance bool) bool {
	done := func() {
		b.clearStrike(m.MessageID)
		if advance {
			b.st.BumpLastSync(chatKey, m.Time)
		}
	}
	if m.MessageID != "" {
		if _, linked := b.st.TgByMax(chatKey, m.MessageID); linked {
			done()
			return true
		}
	}
	if resolve {
		b.max.ResolveAttachments(&m)
	}
	if b.postMessage(threadID, m) {
		done()
		return true
	}
	if b.strikeOut(m.MessageID) {
		log.Printf("[sync] giving up on message %s of chat %s: delivery kept failing", m.MessageID, chatKey)
		b.alert("give-up:"+chatKey, "⚠️ Одно сообщение из MAX так и не удалось переслать — оно пропущено (подробности в логе).", 30*time.Minute)
		done()
		return true
	}
	return false
}

// pageFn reads one page of a chat's history older than from (0 = newest).
type pageFn func(from int64, count int) (page []maxapi.IncomingMessage, oldest int64, raw int, err error)

// collectSince returns the chat's messages newer than cursor, oldest-first,
// paging backwards through the history until it reaches the cursor. With limit
// > 0 only the newest limit messages are kept (a very long outage must not turn
// into an hours-long flood).
func collectSince(fetch pageFn, cursor int64, limit int) ([]maxapi.IncomingMessage, error) {
	var all []maxapi.IncomingMessage
	seen := make(map[string]bool)
	var from int64
	for i := 0; i < historyMaxPages; i++ {
		page, oldest, raw, err := fetch(from, historyPageSize)
		if err != nil {
			return nil, err
		}
		if raw == 0 {
			break // start of the chat
		}
		for _, m := range page {
			if seen[m.MessageID] {
				continue
			}
			seen[m.MessageID] = true
			if m.Time > cursor {
				all = append(all, m)
			}
		}
		if oldest <= cursor {
			break // everything further back is already delivered
		}
		if from != 0 && oldest >= from {
			break // the page did not move: the start of the chat
		}
		from = oldest
		if limit > 0 && len(all) > limit {
			break // enough: only the newest limit are kept anyway
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Time < all[j].Time })
	if limit > 0 && len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all, nil
}
