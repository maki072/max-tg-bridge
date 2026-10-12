package dinner

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type fakeSender struct {
	mu        sync.Mutex
	connected bool
	err       error
	sent      []string
}

func (f *fakeSender) SendMessage(chatID int64, text string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	f.sent = append(f.sent, text)
	return "1", nil
}
func (f *fakeSender) Status() (bool, int64, string) { return f.connected, 0, "" }

func TestPollSendsAndAcks(t *testing.T) {
	var mu sync.Mutex
	var acks []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/api/bridge/pending" {
			json.NewEncoder(w).Encode(map[string]any{"messages": []message{{ID: 7, Kind: "final", Day: "2026-10-07", Text: "ОБЕД"}}})
			return
		}
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		acks = append(acks, m)
		mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	o := Options{URL: srv.URL, Token: "tok", ChatID: 5, Interval: time.Second}

	s := &fakeSender{connected: true}
	poll(o, s)
	if len(s.sent) != 1 || s.sent[0] != "ОБЕД" || len(acks) != 1 || acks[0]["ok"] != true {
		t.Fatalf("успешная отправка: sent=%v acks=%v", s.sent, acks)
	}

	acks = nil
	poll(o, &fakeSender{connected: false})
	if len(acks) != 1 || acks[0]["ok"] != false {
		t.Fatalf("без MAX ждали ack с ошибкой: %v", acks)
	}

	acks = nil
	poll(o, &fakeSender{connected: true, err: errors.New("boom")})
	if len(acks) != 1 || acks[0]["ok"] != false {
		t.Fatalf("ошибка отправки: %v", acks)
	}

	o.Token = "wrong"
	if _, err := fetch(o); err == nil {
		t.Fatal("неверный токен должен давать ошибку")
	}
}
