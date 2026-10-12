// Package dinner забирает готовые сообщения у сайта обедов (dinner.016.help)
// и отправляет их в группу MAX. Мост сам ходит на сайт (pull), поэтому внешний
// адрес у моста не нужен.
package dinner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Sender — то, что нужно от MAX-клиента (реализует maxapi.Client).
type Sender interface {
	SendMessage(chatID int64, text string) (string, error)
	Status() (connected bool, selfID int64, lastErr string)
}

type Options struct {
	URL      string // https://dinner.016.help
	Token    string
	ChatID   int64
	Interval time.Duration
}

type message struct {
	ID   int64  `json:"id"`
	Kind string `json:"kind"`
	Day  string `json:"day"`
	Text string `json:"text"`
}

var httpClient = &http.Client{Timeout: 20 * time.Second}

// Run блокирует: раз в Interval спрашивает сайт, что отправить, и отправляет.
func Run(o Options, s Sender) {
	o.URL = strings.TrimRight(o.URL, "/")
	if o.Interval < 10*time.Second {
		o.Interval = 30 * time.Second
	}
	log.Printf("[dinner] polling %s every %s, MAX chat %d", o.URL, o.Interval, o.ChatID)
	for {
		poll(o, s)
		time.Sleep(o.Interval)
	}
}

func poll(o Options, s Sender) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[dinner] recovered from panic: %v", r)
		}
	}()
	msgs, err := fetch(o)
	if err != nil {
		log.Printf("[dinner] fetch: %v", err)
		return
	}
	for _, m := range msgs {
		// сайт отдаёт сообщение с «арендой» на 2 минуты; если MAX недоступен,
		// сообщение вернётся в очередь само после ack с ошибкой
		connected, _, _ := s.Status()
		var sendErr error
		if !connected {
			sendErr = fmt.Errorf("MAX not connected")
		} else {
			_, sendErr = s.SendMessage(o.ChatID, m.Text)
		}
		if sendErr != nil {
			log.Printf("[dinner] send #%d (%s %s) failed: %v", m.ID, m.Kind, m.Day, sendErr)
		} else {
			log.Printf("[dinner] sent #%d (%s %s) to MAX chat %d", m.ID, m.Kind, m.Day, o.ChatID)
		}
		if err := ack(o, m.ID, sendErr); err != nil {
			// сообщение уже ушло, но сайт не узнал: после аренды оно придёт снова.
			// Это редкий случай (сеть пропала между отправкой и ack) — дубль лучше потери.
			log.Printf("[dinner] ack #%d: %v", m.ID, err)
		}
		if sendErr != nil {
			return // не долбим MAX, повторим на следующем опросе
		}
		time.Sleep(2 * time.Second) // не заваливаем чат пачкой
	}
}

func fetch(o Options) ([]message, error) {
	req, _ := http.NewRequest("GET", o.URL+"/api/bridge/pending", nil)
	req.Header.Set("Authorization", "Bearer "+o.Token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d (проверьте dinner_token)", resp.StatusCode)
	}
	var out struct {
		Messages []message `json:"messages"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, err
	}
	return out.Messages, nil
}

func ack(o Options, id int64, sendErr error) error {
	body := map[string]any{"id": id, "ok": sendErr == nil}
	if sendErr != nil {
		body["error"] = sendErr.Error()
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", o.URL+"/api/bridge/ack", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+o.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
