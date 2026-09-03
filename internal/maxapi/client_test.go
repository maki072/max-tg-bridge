package maxapi

import "testing"

// The service and a logged-in browser share one MAX token; the server closes a
// session if two connections use the same deviceId. mergeRefresh must therefore
// never let our deviceId become the browser's.

func TestMergeRefreshKeepsOwnDeviceID(t *testing.T) {
	var savedDev string
	c := &Client{deviceID: "own-id", saveToken: func(d, _, _, _ string) { savedDev = d }}
	c.mergeRefresh("browser-id", "tok", "ua", "ck")
	if c.deviceID != "own-id" {
		t.Fatalf("deviceID changed to %q, want own-id", c.deviceID)
	}
	if savedDev != "own-id" {
		t.Fatalf("saved deviceID %q, want own-id", savedDev)
	}
	if c.token != "tok" || c.cookie != "ck" {
		t.Fatalf("token/cookie not applied: token=%q cookie=%q", c.token, c.cookie)
	}
}

func TestMergeRefreshRegeneratesOnCollision(t *testing.T) {
	c := &Client{deviceID: "collide", saveToken: func(string, string, string, string) {}}
	c.mergeRefresh("collide", "tok", "ua", "ck") // browser id == ours
	if c.deviceID == "collide" || c.deviceID == "" {
		t.Fatalf("deviceID not regenerated on collision: %q", c.deviceID)
	}
}

func TestMergeRefreshIgnoresEmptyToken(t *testing.T) {
	c := &Client{deviceID: "own-id", token: "old"}
	c.mergeRefresh("browser-id", "", "ua", "ck")
	if c.token != "old" {
		t.Fatalf("empty refresh clobbered token: %q", c.token)
	}
}
