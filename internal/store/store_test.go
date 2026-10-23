package store

import (
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := testLoad(t, filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestBanUnbanPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := testLoad(t, path)
	s.Ban("10", "Класс 5А")
	if !s.IsBanned("10") || s.IsBanned("11") {
		t.Fatal("IsBanned wrong")
	}
	s2, err := testLoad(t, path)
	if err != nil || !s2.IsBanned("10") || s2.Bans()["10"] != "Класс 5А" {
		t.Fatalf("ban must survive a reload: %v %+v", err, s2.Bans())
	}
	if title, was := s2.Unban("10"); !was || title != "Класс 5А" {
		t.Fatalf("Unban = %q,%v", title, was)
	}
	if _, was := s2.Unban("10"); was {
		t.Fatal("second Unban must report not banned")
	}
}

func TestRemoveChatForgetsEverything(t *testing.T) {
	s := newTestStore(t)
	s.SetTopic("10", 5)
	s.SetTopic("11", 6)
	s.SetTitle("10", "A")
	s.BumpLastSync("10", 1000)
	s.LinkMessages("10", "m1", 100, 5)
	s.LinkMessages("11", "m2", 101, 6)

	if got := s.RemoveChat("10"); got != 5 {
		t.Fatalf("RemoveChat returned thread %d, want 5", got)
	}
	if _, ok := s.Topic("10"); ok || s.Title("10") != "" || s.LastSyncTime("10") != 0 {
		t.Error("chat 10 not fully forgotten")
	}
	if _, ok := s.MaxByTg(100); ok {
		t.Error("links of chat 10 must be gone")
	}
	if l, ok := s.MaxByTg(101); !ok || l.MaxMsgID != "m2" {
		t.Error("links of chat 11 must survive with a rebuilt index")
	}
	if _, ok := s.Topic("11"); !ok {
		t.Error("chat 11 must survive")
	}
	if got := s.RemoveChat("nope"); got != 0 {
		t.Errorf("unknown chat returned %d", got)
	}
}

func TestMoveTopic(t *testing.T) {
	s := newTestStore(t)
	s.SetTopic(PendingPrefix+"7", 9)
	s.SetTitle(PendingPrefix+"7", "Иван")
	s.MoveTopic(PendingPrefix+"7", "555")
	if th, ok := s.Topic("555"); !ok || th != 9 {
		t.Fatalf("Topic(555) = %d,%v", th, ok)
	}
	if _, ok := s.Topic(PendingPrefix + "7"); ok {
		t.Error("old key must be gone")
	}
	if s.Title("555") != "Иван" {
		t.Error("title must move with the topic")
	}
	if key, ok := s.ChatByTopic(9); !ok || key != "555" {
		t.Errorf("ChatByTopic = %q,%v", key, ok)
	}
}

func TestTokenKinds(t *testing.T) {
	s := newTestStore(t)
	if s.TokenKindOf() != "" {
		t.Error("no token, no kind")
	}
	s.SetCredentials("dev", "webtok", "ua", "ck")
	if s.TokenKindOf() != TokenWeb {
		t.Errorf("browser-style credentials are web, got %q", s.TokenKindOf())
	}
	s.SetDesktopCredentials("dev", "desktoptok")
	if s.TokenKindOf() != TokenDesktop {
		t.Errorf("got %q", s.TokenKindOf())
	}
	if _, tok, ua, ck := s.GetCredentials(); tok != "desktoptok" || ua != "" || ck != "" {
		t.Errorf("desktop credentials must not carry a web UA/cookie: %q %q %q", tok, ua, ck)
	}
	s.ClearCredentials()
	if _, tok, _, _ := s.GetCredentials(); tok != "" || s.TokenKindOf() != "" {
		t.Error("logout must clear the token")
	}
	if s.EnsureDeviceID() == "" {
		t.Error("the device id survives logout")
	}
}
