package bridge

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// Failure notifications. The bridge runs unattended, so anything that stops it
// from working has to reach the group — but a flapping connection or a
// misbehaving chat must not turn into a flood. Every alert has a per-key
// cooldown, and there is a global cap on top.

const (
	downGrace         = 60 * time.Second // a disconnect must last this long before it is reported
	alertGlobalMax    = 8                // at most this many alerts...
	alertGlobalWindow = time.Hour        // ...per window, whatever their keys
)

type alertState struct {
	mu          sync.Mutex
	last        map[string]time.Time // key -> when it last fired
	recent      []time.Time          // global fire times inside the window
	downTimer   *time.Timer          // armed while MAX is disconnected
	downAlerted bool                 // an "MAX is down" alert went out and awaits its "restored"
}

// cooled reports whether key may fire now (and records it if so).
func (a *alertState) cooled(key string, cooldown time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.last == nil {
		a.last = make(map[string]time.Time)
	}
	now := time.Now()
	if t, ok := a.last[key]; ok && now.Sub(t) < cooldown {
		return false
	}
	a.last[key] = now
	return true
}

// alert posts text to the group's General topic unless the same key fired
// within cooldown, or too many alerts went out lately. Reports whether it was
// actually sent.
func (b *Bridge) alert(key, text string, cooldown time.Duration) bool {
	if !b.al.cooled(key, cooldown) {
		return false
	}
	b.al.mu.Lock()
	now := time.Now()
	kept := b.al.recent[:0]
	for _, t := range b.al.recent {
		if now.Sub(t) < alertGlobalWindow {
			kept = append(kept, t)
		}
	}
	b.al.recent = kept
	if len(kept) >= alertGlobalMax {
		b.al.mu.Unlock()
		log.Printf("[alert] suppressed (global cap): %s", text)
		return false
	}
	b.al.recent = append(b.al.recent, now)
	b.al.mu.Unlock()

	b.notify(text)
	return true
}

// ReportPanic turns a recovered panic into a (rate-limited) group alert.
func (b *Bridge) ReportPanic(where string, r any) {
	b.alert("panic:"+where, fmt.Sprintf("⚠️ Внутренняя ошибка (%s): %v\nМост продолжает работать; подробности — в логе.", where, r), 30*time.Minute)
}

// reportSendFailure tells the topic that something written there did not reach
// MAX — silently losing a reply is the worst failure for a bridge. Throttled
// per topic so a burst of failures yields one note.
func (b *Bridge) reportSendFailure(threadID int, replyToTg int64, what string, err error) {
	log.Printf("[bridge] %s failed: %v", what, err)
	if !b.al.cooled(fmt.Sprintf("send:%d", threadID), 15*time.Second) {
		return
	}
	reason := shortErr(err)
	if b.max.Paused() {
		reason = "MAX на паузе (/resume — включить)"
	}
	_, _ = b.bot.SendToTopic(threadID, "⚠️ Не доставлено в MAX: "+what+" — "+reason, replyToTg)
}

// OnMaxStatus is wired to the MAX client's connected/disconnected flips. A
// disconnect is only reported once it has lasted downGrace (reconnects are
// routine), and a matching "restored" note follows if it was.
func (b *Bridge) OnMaxStatus(connected bool) {
	b.al.mu.Lock()
	defer b.al.mu.Unlock()
	if connected {
		if b.al.downTimer != nil {
			b.al.downTimer.Stop()
			b.al.downTimer = nil
		}
		if b.al.downAlerted {
			b.al.downAlerted = false
			go b.notify("🟢 Соединение с MAX восстановлено.")
		}
		return
	}
	if b.max.Paused() {
		return // deliberate
	}
	b.armDownTimerLocked(downGrace)
}

// armDownTimerLocked starts the "still down?" check unless one is pending.
func (b *Bridge) armDownTimerLocked(d time.Duration) {
	if b.al.downTimer != nil {
		return
	}
	b.al.downTimer = time.AfterFunc(d, b.checkDown)
}

// checkDown fires downGrace after a disconnect: if MAX is still unreachable
// (and not paused on purpose), say so once.
func (b *Bridge) checkDown() {
	b.al.mu.Lock()
	b.al.downTimer = nil
	b.al.mu.Unlock()

	connected, _, lastErr := b.max.Status()
	if connected || b.max.Paused() {
		return
	}
	text := "🔴 Нет соединения с MAX уже больше минуты — мост переподключается сам."
	if lastErr != "" {
		text += "\nПоследняя ошибка: " + truncateRunes(lastErr, 300)
	}
	if _, tok, _, _ := b.st.GetCredentials(); tok == "" {
		text = "🔴 MAX не авторизован — мост ничего не пересылает. Войдите: /login в личке бота (панель → «Вход в MAX»)."
	} else if strings.Contains(lastErr, "login") || strings.Contains(lastErr, "token") {
		text += "\nПохоже, MAX не принял токен — войдите заново: /login в личке бота."
	}
	if b.alert("max-down", text, 30*time.Minute) {
		b.al.mu.Lock()
		b.al.downAlerted = true
		b.al.mu.Unlock()
	}
	// Still down: look again later, so a long outage is eventually re-reported
	// (the cooldown above keeps that to one message per 30 minutes).
	b.al.mu.Lock()
	b.armDownTimerLocked(10 * time.Minute)
	b.al.mu.Unlock()
}
