package maxapi

import (
	"errors"
	"sync"
	"time"
)

// errPaused is returned by a session that was cut short by Pause.
var errPaused = errors.New("paused")

// pauseState is the "deliberately offline" switch: while it is set, Run keeps
// the MAX connection closed and does not reconnect, until the timer runs out or
// Resume is called. It lives in memory only — a restart resumes MAX.
type pauseState struct {
	mu    sync.Mutex
	until time.Time     // zero: not paused
	wake  chan struct{} // closed by Resume to wake waitWhilePaused early
}

// untilRestart is the "until restart" pause length (effectively forever).
const untilRestart = 100 * 365 * 24 * time.Hour

// Pause closes the MAX connection and keeps it closed for d (d <= 0: until the
// process restarts or Resume is called).
func (c *Client) Pause(d time.Duration) {
	if d <= 0 {
		d = untilRestart
	}
	c.pause.mu.Lock()
	c.pause.until = time.Now().Add(d)
	if c.pause.wake == nil {
		c.pause.wake = make(chan struct{})
	}
	c.pause.mu.Unlock()

	c.writeMu.Lock()
	if c.conn != nil {
		_ = c.conn.Close() // the read loop fails and the session unwinds
	}
	c.writeMu.Unlock()
}

// Resume ends a pause; Run reconnects right away. Reports whether it was paused.
func (c *Client) Resume() bool {
	c.pause.mu.Lock()
	was := !c.pause.until.IsZero()
	c.pause.until = time.Time{}
	if c.pause.wake != nil {
		close(c.pause.wake)
		c.pause.wake = nil
	}
	c.pause.mu.Unlock()
	return was
}

// Paused reports whether a pause is currently in force.
func (c *Client) Paused() bool {
	_, ok := c.PausedUntil()
	return ok
}

// PausedUntil returns when the pause ends (more than a year ahead means "until
// restart"), and whether a pause is in force at all.
func (c *Client) PausedUntil() (time.Time, bool) {
	c.pause.mu.Lock()
	defer c.pause.mu.Unlock()
	if c.pause.until.IsZero() || !time.Now().Before(c.pause.until) {
		return time.Time{}, false
	}
	return c.pause.until, true
}

// waitWhilePaused blocks Run's reconnect loop for as long as a pause lasts.
func (c *Client) waitWhilePaused() {
	for {
		c.pause.mu.Lock()
		until, wake := c.pause.until, c.pause.wake
		c.pause.mu.Unlock()
		if until.IsZero() {
			return
		}
		d := time.Until(until)
		if d <= 0 {
			c.Resume() // the timer ran out
			return
		}
		select {
		case <-time.After(d):
		case <-wake:
		}
	}
}
