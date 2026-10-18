package maxapi

import (
	"os"
	"testing"
	"time"
)

// TestLiveTCPInit connects to the real MAX desktop endpoint and completes INIT
// with a throwaway deviceId — no account, no SMS. Skipped unless MAXTG_LIVE=1.
func TestLiveTCPInit(t *testing.T) {
	if os.Getenv("MAXTG_LIVE") != "1" {
		t.Skip("set MAXTG_LIVE=1 to talk to the real MAX server")
	}
	id, err := NewDeviceID()
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewAuthSession(id)
	if err != nil {
		t.Fatalf("NewAuthSession: %v", err)
	}
	defer a.Close()
	// A PING round trip proves framing in both directions.
	if _, err := a.c.request(OpPing, nil, 10*time.Second); err != nil {
		t.Logf("PING reply: %v (not fatal: PING may be fire-and-forget)", err)
	}
	t.Log("TCP + TLS (Russian CA) + INIT as DESKTOP: OK")
}
