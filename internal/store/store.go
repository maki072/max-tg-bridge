package store

import (
	"database/sql"
	"encoding/json"
	"os"
	"sync"

	"maxtg/internal/maxapi"
)

// Store is the persisted state: MAX credentials, chat<->topic mapping,
// Telegram update offset, and a bounded set of seen message IDs for dedupe.
type Store struct {
	path string
	mu   sync.Mutex

	DeviceID  string          `json:"device_id"`
	Token     string          `json:"token"`
	UserAgent string          `json:"user_agent"`
	Cookie    string          `json:"cookie"`     // MAX web cookies captured from the browser
	TokenKind string          `json:"token_kind"` // "desktop" (browserless sign-in) or "web"; empty = legacy web
	TgOffset  int64           `json:"tg_offset"`
	ChatTopic map[string]int  `json:"chat_topic"` // maxChatID -> tg thread id
	Seen      []string        `json:"seen"` // legacy bounded dedupe list; imported into delivered.db once, then emptied
	seenSet   map[string]bool `json:"-"`

	db *sql.DB `json:"-"` // delivered-messages registry (delivered.go)

	// LastSync is a per-chat baseline: the timestamp (ms) of the newest MAX
	// message that was already in Telegram before the delivered-messages
	// registry existed (or when the chat was first seen with unknown history).
	// History at or before it is never re-posted. It is not advanced afterwards:
	// everything newer is tracked per message in the registry, so a message whose
	// delivery failed can never be skipped just because a later one succeeded.
	LastSync map[string]int64 `json:"last_sync"` // maxChatID -> baseline message time (ms)

	// MsgLinks is a bounded, bidirectional map between a MAX message
	// (chatID+messageID) and the Telegram message that mirrors it, used for
	// reactions, replies, edits and deletes travelling in either direction.
	MsgLinks []MsgLink      `json:"msg_links"`
	byMax    map[string]int `json:"-"` // "maxChatID:maxMsgID" -> index into MsgLinks
	byTg     map[int64]int  `json:"-"` // tgMsgID -> index into MsgLinks

	// Polls are MAX polls mirrored into Telegram (see polls.go).
	Polls []PollRec `json:"polls"`

	// Banned are muted MAX chats (maxChatID -> title at the time): their
	// messages are not mirrored and they get no topic until unbanned.
	Banned map[string]string `json:"banned"`
	// Titles remembers each mirrored chat's name (maxChatID -> title) so the
	// panel can list chats readably.
	Titles map[string]string `json:"titles"`
}

// PendingPrefix marks a ChatTopic entry for a MAX user we have no dialog with
// yet ("pending:<userId>"): the topic exists, and the first message written in
// it opens the dialog (see Bridge.openDialog).
const PendingPrefix = "pending:"

// MsgLink records one bridged message: a MAX (chatID,messageID) paired with
// the Telegram message (in the group's topic) that mirrors it.
type MsgLink struct {
	MaxChatID string `json:"max_chat_id"`
	MaxMsgID  string `json:"max_msg_id"`
	TgMsgID   int64  `json:"tg_msg_id"`
	ThreadID  int    `json:"thread_id"`
}

const maxMsgLinks = 3000

// Load reads the state file (or returns an empty store).
func Load(path string) (*Store, error) {
	s := &Store{
		path:      path,
		ChatTopic: make(map[string]int),
		seenSet:   make(map[string]bool),
		byMax:     make(map[string]int),
		byTg:      make(map[int64]int),
		LastSync:  make(map[string]int64),
		Banned:    make(map[string]string),
		Titles:    make(map[string]string),
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if err := s.openDelivered(); err != nil {
				return nil, err
			}
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, err
	}
	if s.ChatTopic == nil {
		s.ChatTopic = make(map[string]int)
	}
	if s.LastSync == nil {
		s.LastSync = make(map[string]int64)
	}
	if s.Banned == nil {
		s.Banned = make(map[string]string)
	}
	if s.Titles == nil {
		s.Titles = make(map[string]string)
	}
	s.seenSet = make(map[string]bool, len(s.Seen))
	for _, id := range s.Seen {
		s.seenSet[id] = true
	}
	s.rebuildMsgIndex()
	s.path = path
	if err := s.openDelivered(); err != nil {
		return nil, err
	}
	return s, nil
}

// rebuildMsgIndex recomputes byMax/byTg from MsgLinks. Caller must hold the lock.
func (s *Store) rebuildMsgIndex() {
	s.byMax = make(map[string]int, len(s.MsgLinks))
	s.byTg = make(map[int64]int, len(s.MsgLinks))
	for i, l := range s.MsgLinks {
		s.byMax[l.MaxChatID+":"+l.MaxMsgID] = i
		if l.TgMsgID != 0 {
			s.byTg[l.TgMsgID] = i
		}
	}
}

// LinkMessages records a bridged message pair (idempotent — a repeated call
// for an already-known MAX message updates its Telegram side, e.g. when a
// caption and its media land as separate Telegram messages).
func (s *Store) LinkMessages(maxChatID, maxMsgID string, tgMsgID int64, threadID int) {
	if maxChatID == "" || maxMsgID == "" || tgMsgID == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := maxChatID + ":" + maxMsgID
	if i, ok := s.byMax[key]; ok {
		s.MsgLinks[i].TgMsgID = tgMsgID
		s.MsgLinks[i].ThreadID = threadID
		s.byTg[tgMsgID] = i
		_ = s.save()
		return
	}
	s.MsgLinks = append(s.MsgLinks, MsgLink{MaxChatID: maxChatID, MaxMsgID: maxMsgID, TgMsgID: tgMsgID, ThreadID: threadID})
	if len(s.MsgLinks) > maxMsgLinks {
		s.MsgLinks = s.MsgLinks[len(s.MsgLinks)-maxMsgLinks:]
		s.rebuildMsgIndex()
	} else {
		i := len(s.MsgLinks) - 1
		s.byMax[key] = i
		s.byTg[tgMsgID] = i
	}
	_ = s.save()
}

// TgByMax returns the Telegram message linked to a MAX message, if any.
func (s *Store) TgByMax(maxChatID, maxMsgID string) (MsgLink, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.byMax[maxChatID+":"+maxMsgID]
	if !ok {
		return MsgLink{}, false
	}
	return s.MsgLinks[i], true
}

// MaxByTg returns the MAX message linked to a Telegram message, if any.
func (s *Store) MaxByTg(tgMsgID int64) (MsgLink, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.byTg[tgMsgID]
	if !ok {
		return MsgLink{}, false
	}
	return s.MsgLinks[i], true
}

// save writes the state file atomically. Caller must hold the lock.
func (s *Store) save() error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// SetCredentials persists MAX deviceId + token + userAgent + cookie.
func (s *Store) SetCredentials(deviceID, token, userAgent, cookie string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.DeviceID = deviceID
	s.Token = token
	s.UserAgent = userAgent
	s.Cookie = cookie
	s.TokenKind = TokenWeb
	_ = s.save()
}

// GetCredentials returns the stored MAX deviceId + token + userAgent + cookie.
func (s *Store) GetCredentials() (deviceID, token, userAgent, cookie string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.DeviceID, s.Token, s.UserAgent, s.Cookie
}

// EnsureDeviceID returns the stored deviceId, generating and persisting a
// fresh one if none exists yet. Use this вЂ” not a browser's deviceId вЂ” for our
// own session identity (see maxapi client.go session() for why).
func (s *Store) EnsureDeviceID() string {
	s.mu.Lock()
	if s.DeviceID != "" {
		id := s.DeviceID
		s.mu.Unlock()
		return id
	}
	s.mu.Unlock()

	id, err := maxapi.NewDeviceID()
	if err != nil {
		return "" // caller falls back to empty; maxapi.Client will generate one itself
	}
	s.mu.Lock()
	s.DeviceID = id
	_ = s.save()
	s.mu.Unlock()
	return id
}

// SetOffset persists the Telegram getUpdates offset.
func (s *Store) SetOffset(offset int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.TgOffset = offset
	_ = s.save()
}

// GetOffset returns the stored Telegram offset.
func (s *Store) GetOffset() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.TgOffset
}

// Topic returns the tg thread id mapped to a MAX chat (ok=false if none).
func (s *Store) Topic(maxChatID string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.ChatTopic[maxChatID]
	return t, ok
}

// SetTopic maps a MAX chat to a tg thread id and persists.
func (s *Store) SetTopic(maxChatID string, threadID int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ChatTopic[maxChatID] = threadID
	_ = s.save()
}

// RemoveChat forgets a MAX chat entirely: its topic mapping, resync watermark,
// remembered title and message links. Returns the thread id it had (0 if none).
func (s *Store) RemoveChat(maxChatID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	thread := s.ChatTopic[maxChatID]
	delete(s.ChatTopic, maxChatID)
	delete(s.LastSync, maxChatID)
	s.forgetChat(maxChatID)
	delete(s.Titles, maxChatID)
	kept := s.MsgLinks[:0]
	for _, l := range s.MsgLinks {
		if l.MaxChatID != maxChatID {
			kept = append(kept, l)
		}
	}
	s.MsgLinks = kept
	s.rebuildMsgIndex()
	_ = s.save()
	return thread
}

// MoveTopic re-keys a topic mapping (a "pending:<userId>" entry becoming the
// real MAX chat id once the dialog exists), carrying the title along.
func (s *Store) MoveTopic(oldKey, newKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	thread, ok := s.ChatTopic[oldKey]
	if !ok {
		return
	}
	delete(s.ChatTopic, oldKey)
	s.ChatTopic[newKey] = thread
	if t := s.Titles[oldKey]; t != "" {
		delete(s.Titles, oldKey)
		s.Titles[newKey] = t
	}
	_ = s.save()
}

// SetTitle remembers a chat's display name.
func (s *Store) SetTitle(maxChatID, title string) {
	if title == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Titles[maxChatID] != title {
		s.Titles[maxChatID] = title
		_ = s.save()
	}
}

// Title returns a remembered chat name ("" if unknown).
func (s *Store) Title(maxChatID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Titles[maxChatID]
}

// Ban mutes a MAX chat.
func (s *Store) Ban(maxChatID, title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Banned[maxChatID] = title
	_ = s.save()
}

// Unban lifts a mute; reports whether the chat was muted.
func (s *Store) Unban(maxChatID string) (title string, was bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	title, was = s.Banned[maxChatID]
	if was {
		delete(s.Banned, maxChatID)
		_ = s.save()
	}
	return title, was
}

// IsBanned reports whether a MAX chat is muted.
func (s *Store) IsBanned(maxChatID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.Banned[maxChatID]
	return ok
}

// Bans returns a snapshot of the muted chats (maxChatID -> title).
func (s *Store) Bans() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.Banned))
	for k, v := range s.Banned {
		out[k] = v
	}
	return out
}

// AllTopics returns a snapshot of the MAX chat -> tg thread id mapping, for
// re-syncing every known chat's history (e.g. after a reconnect).
func (s *Store) AllTopics() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.ChatTopic))
	for k, v := range s.ChatTopic {
		out[k] = v
	}
	return out
}

// LastSyncTime returns the newest MAX message time (ms) already delivered for
// a chat, or 0 if none is recorded yet.
func (s *Store) LastSyncTime(maxChatID string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.LastSync[maxChatID]
}

// BumpLastSync advances the stored watermark for a chat to t, unless t is not
// newer than what's already recorded (the watermark never moves backwards).
func (s *Store) BumpLastSync(maxChatID string, t int64) {
	if t <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t > s.LastSync[maxChatID] {
		s.LastSync[maxChatID] = t
		_ = s.save()
	}
}

// ChatByTopic returns the MAX chat id mapped to a tg thread id.
func (s *Store) ChatByTopic(threadID int) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for chatID, t := range s.ChatTopic {
		if t == threadID {
			return chatID, true
		}
	}
	return "", false
}

// Token kinds: which kind of MAX client a stored session token was minted for.
// A token from SMS sign-in via the bridge is a "desktop" one and is used over
// the TCP transport; one imported from a browser (or an older state file, where
// the kind is empty) belongs to a web session and keeps the WebSocket transport.
const (
	TokenWeb     = "web"
	TokenDesktop = "desktop"
)

// SetDesktopCredentials stores a session token minted by the browserless
// (desktop/TCP) sign-in. No web user agent or cookies apply to it.
func (s *Store) SetDesktopCredentials(deviceID, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.DeviceID = deviceID
	s.Token = token
	s.UserAgent = ""
	s.Cookie = ""
	s.TokenKind = TokenDesktop
	_ = s.save()
}

// ClearCredentials forgets the MAX session (logout); the device id is kept.
func (s *Store) ClearCredentials() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Token = ""
	s.UserAgent = ""
	s.Cookie = ""
	s.TokenKind = ""
	_ = s.save()
}

// TokenKindOf returns TokenDesktop, TokenWeb, or "" (no token / legacy state).
func (s *Store) TokenKindOf() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Token == "" {
		return ""
	}
	return s.TokenKind
}
