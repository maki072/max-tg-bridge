package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClaimDoneRelease(t *testing.T) {
	s, err := testLoad(t, filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Claim("c1", "m1") {
		t.Fatal("first claim must win")
	}
	if s.Claim("c1", "m1") {
		t.Fatal("a pending message must not be claimed twice")
	}
	s.Release("m1") // delivery failed
	if !s.Claim("c1", "m1") {
		t.Fatal("a released message must be claimable again")
	}
	s.Done("c1", "m1")
	if s.Claim("c1", "m1") || !s.IsDelivered("m1") {
		t.Fatal("a delivered message must stay delivered")
	}
	s.Release("m1") // releasing after Done must not undo it
	if !s.IsDelivered("m1") {
		t.Fatal("Release removed a delivered message")
	}
}

func TestStaleClaimIsTakenOver(t *testing.T) {
	s, _ := testLoad(t, filepath.Join(t.TempDir(), "state.json"))
	if !s.Claim("c", "m") {
		t.Fatal("claim")
	}
	old := time.Now().Add(-2 * claimStale).Unix()
	if _, err := s.db.Exec(`UPDATE delivered SET ts = ? WHERE msg = 'm'`, old); err != nil {
		t.Fatal(err)
	}
	if !s.Claim("c", "m") {
		t.Fatal("a stale pending claim (crash mid-delivery) must be claimable")
	}
}

func TestDeliveredSurvivesRestartAndIsUnbounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := testLoad(t, path)
	for i := 0; i < 5000; i++ { // far past the old 2000-entry ring
		id := "m" + string(rune('a'+i%26)) + time.Duration(i).String()
		s.MarkDelivered("c", id)
	}
	s.MarkDelivered("c", "first")
	s2, err := testLoad(t, path)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.IsDelivered("first") || s2.Claim("c", "first") {
		t.Fatal("delivered record lost across restart")
	}
}

func TestSeenListIsImportedOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	raw, _ := json.Marshal(map[string]any{"seen": []string{"max:100", "max:200"}, "last_sync": map[string]int64{"7": 5}})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := testLoad(t, path)
	if err != nil {
		t.Fatal(err)
	}
	if !s.IsDelivered("100") || !s.IsDelivered("200") {
		t.Fatal("legacy seen ids not imported")
	}
	if len(s.Seen) != 0 {
		t.Fatal("legacy seen list not cleared after import")
	}
	data, _ := os.ReadFile(path)
	var st struct {
		Seen []string `json:"seen"`
	}
	_ = json.Unmarshal(data, &st)
	if len(st.Seen) != 0 {
		t.Fatal("state.json still carries the seen list")
	}
}

func TestChatTracked(t *testing.T) {
	s, _ := testLoad(t, filepath.Join(t.TempDir(), "state.json"))
	if s.ChatTracked("9") {
		t.Fatal("an unknown chat must not be tracked")
	}
	s.BumpLastSync("9", 1000) // baseline
	if !s.ChatTracked("9") {
		t.Fatal("a chat with a baseline is tracked")
	}
	s.MarkDelivered("5", "x")
	if !s.ChatTracked("5") {
		t.Fatal("a chat with registry rows is tracked")
	}
	s.RemoveChat("5")
	if s.ChatTracked("5") {
		t.Fatal("removing a chat must forget its delivery records")
	}
}

// testLoad is Load plus closing the registry when the test ends (Windows cannot
// delete an open SQLite file).
func testLoad(t *testing.T, path string) (*Store, error) {
	s, err := Load(path)
	if s != nil {
		t.Cleanup(s.Close)
	}
	return s, err
}
