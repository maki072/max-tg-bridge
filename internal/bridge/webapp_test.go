package bridge

import (
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// signInitData builds an initData string the way Telegram does.
func signInitData(token string, vals map[string]string) string {
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sortStrings(keys)
	lines := make([]string, len(keys))
	for i, k := range keys {
		lines[i] = k + "=" + vals[k]
	}
	secret := hmacSHA256([]byte("WebAppData"), []byte(token))
	hash := hex.EncodeToString(hmacSHA256(secret, []byte(strings.Join(lines, "\n"))))
	q := url.Values{}
	for k, v := range vals {
		q.Set(k, v)
	}
	q.Set("hash", hash)
	return q.Encode()
}

func sortStrings(s []string) {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}

func TestValidateInitData(t *testing.T) {
	const token = "123:ABC"
	now := time.Unix(1_800_000_000, 0)
	good := map[string]string{
		"auth_date": strconv.FormatInt(now.Add(-time.Hour).Unix(), 10),
		"query_id":  "AAH",
		"user":      `{"id":4242,"first_name":"Мар"}`,
	}

	uid, err := validateInitData(signInitData(token, good), token, now)
	if err != nil || uid != 4242 {
		t.Fatalf("valid data: uid=%d err=%v", uid, err)
	}
	if _, err := validateInitData(signInitData(token, good), "other:token", now); err == nil {
		t.Error("data signed with another token was accepted")
	}
	tampered := strings.Replace(signInitData(token, good), "4242", "1", 1)
	if _, err := validateInitData(tampered, token, now); err == nil {
		t.Error("tampered user id was accepted")
	}
	old := map[string]string{"auth_date": strconv.FormatInt(now.Add(-48*time.Hour).Unix(), 10), "user": good["user"]}
	if _, err := validateInitData(signInitData(token, old), token, now); err == nil {
		t.Error("expired data was accepted")
	}
	if _, err := validateInitData("", token, now); err == nil {
		t.Error("empty init data was accepted")
	}
}

func TestTailFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.log")
	var sb strings.Builder
	for i := 1; i <= 100; i++ {
		sb.WriteString("line " + strconv.Itoa(i) + "\n")
	}
	if err := os.WriteFile(p, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	got := tailFile(p, 3, 1<<20)
	if got != "line 98\nline 99\nline 100" {
		t.Fatalf("tail = %q", got)
	}
	// A small byte window must not return a half-cut first line.
	got = tailFile(p, 50, 30)
	for _, l := range strings.Split(got, "\n") {
		if !strings.HasPrefix(l, "line ") || len(l) < 7 {
			t.Fatalf("cut line in %q", got)
		}
	}
}
