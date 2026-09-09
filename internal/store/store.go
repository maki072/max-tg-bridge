package store

import (
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
	Cookie    string          `json:"cookie"` // MAX web cookies captured from the browser
	TgOffset  int64           `json:"tg_offset"`
	ChatTopic map[string]int  `json:"chat_topic"` // maxChatID -> tg thread id
	Seen      []string        `json:"seen"`       // recent message ids (dedupe)
	seenSet   map[string]bool `json:"-"`

	// MsgLinks is a bounded, bidirectional map between a MAX message
	// (chatID+messageID) and the Telegram message that mirrors it, used for
	// reactions, replies, edits and deletes travelling in either direction.
	MsgLinks []MsgLink      `json:"msg_links"`
	byMax    map[string]int `json:"-"` // "maxChatID:maxMsgID" -> index into MsgLinks
	byTg     map[int64]int  `json:"-"` // tgMsgID -> index into MsgLinks
}

// MsgLink records one bridged message: a MAX (chatID,messageID) paired with
// the Telegram message (in the group's topic) that mirrors it.
type MsgLink struct {
	MaxChatID string `json:"max_chat_id"`
	MaxMsgID  string `json:"max_msg_id"`
	TgMsgID   int64  `json:"tg_msg_id"`
	ThreadID  int    `json:"thread_id"`
}

const maxSeen = 2000
const maxMsgLinks = 3000

// Load reads the state file (or returns an empty store).
func Load(path string) (*Store, error) {
	s := &Store{
		path:      path,
		ChatTopic: make(map[string]int),
		seenSet:   make(map[string]bool),
		byMax:     make(map[string]int),
		byTg:      make(map[int64]int),
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
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
	s.seenSet = make(map[string]bool, len(s.Seen))
	for _, id := range s.Seen {
		s.seenSet[id] = true
	}
	s.rebuildMsgIndex()
	s.path = path
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

// SeenAndMark reports whether id was already seen; marks it otherwise.
func (s *Store) SeenAndMark(id string) bool {
	if id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seenSet[id] {
		return true
	}
	s.seenSet[id] = true
	s.Seen = append(s.Seen, id)
	if len(s.Seen) > maxSeen {
		drop := s.Seen[:len(s.Seen)-maxSeen]
		for _, d := range drop {
			delete(s.seenSet, d)
		}
		s.Seen = s.Seen[len(s.Seen)-maxSeen:]
	}
	_ = s.save()
	return false
}
