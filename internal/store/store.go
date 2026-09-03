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
}

const maxSeen = 2000

// Load reads the state file (or returns an empty store).
func Load(path string) (*Store, error) {
	s := &Store{
		path:      path,
		ChatTopic: make(map[string]int),
		seenSet:   make(map[string]bool),
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
	s.path = path
	return s, nil
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

