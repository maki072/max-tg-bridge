package bridge

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"maxtg/internal/maxapi"
)

// fakeHistory serves a chat of messages with times 1000, 2000, … n*1000 the way
// GET_HISTORY does: the newest `count` messages older than from (inclusive of
// from, so consecutive pages overlap by one and the caller must dedupe).
func fakeHistory(n int, calls *int) pageFn {
	return func(from int64, count int) ([]maxapi.IncomingMessage, int64, int, error) {
		*calls++
		var all []maxapi.IncomingMessage
		for i := 1; i <= n; i++ {
			t := int64(i) * 1000
			if from == 0 || t <= from {
				all = append(all, maxapi.IncomingMessage{MessageID: "m" + strconv.Itoa(i), Time: t})
			}
		}
		if len(all) > count {
			all = all[len(all)-count:]
		}
		var oldest int64
		if len(all) > 0 {
			oldest = all[0].Time
		}
		return all, oldest, len(all), nil
	}
}

func TestCollectSinceStopsAtCursor(t *testing.T) {
	calls := 0
	got, err := collectSince(fakeHistory(450, &calls), 440*1000, 0)
	if err != nil || len(got) != 10 {
		t.Fatalf("got %d messages, err %v; want 10", len(got), err)
	}
	if got[0].Time != 441*1000 || got[9].Time != 450*1000 {
		t.Fatalf("not oldest-first / wrong range: %d..%d", got[0].Time, got[9].Time)
	}
	if calls != 1 {
		t.Fatalf("read %d pages for 10 missed messages; the cursor should stop paging", calls)
	}
}

func TestCollectSincePagesAcrossManyMessages(t *testing.T) {
	calls := 0
	got, err := collectSince(fakeHistory(450, &calls), 50*1000, 0)
	if err != nil || len(got) != 400 {
		t.Fatalf("got %d messages, err %v; want 400", len(got), err)
	}
	seen := map[string]bool{}
	for i, m := range got {
		if seen[m.MessageID] {
			t.Fatalf("message %s returned twice (pages overlap)", m.MessageID)
		}
		seen[m.MessageID] = true
		if i > 0 && got[i-1].Time >= m.Time {
			t.Fatal("not strictly oldest-first")
		}
	}
	if calls < 4 {
		t.Fatalf("only %d pages for 400 messages", calls)
	}
}

func TestCollectSinceLimitKeepsNewest(t *testing.T) {
	calls := 0
	got, err := collectSince(fakeHistory(450, &calls), 1000, 50)
	if err != nil || len(got) != 50 {
		t.Fatalf("got %d, err %v; want 50", len(got), err)
	}
	if got[0].Time != 401*1000 || got[49].Time != 450*1000 {
		t.Fatalf("kept %d..%d, want the newest 50 (401..450)", got[0].Time/1000, got[49].Time/1000)
	}
	if calls > 4 {
		t.Fatalf("%d pages read for a 50-message limit", calls)
	}
}

func TestCollectSinceShortChatAndNothingNew(t *testing.T) {
	calls := 0
	got, err := collectSince(fakeHistory(5, &calls), 0+1, 0)
	if err != nil || len(got) != 5 {
		t.Fatalf("short chat: got %d, err %v", len(got), err)
	}
	got, err = collectSince(fakeHistory(5, &calls), 5000, 0)
	if err != nil || len(got) != 0 {
		t.Fatalf("nothing new: got %d, err %v", len(got), err)
	}
}

func TestCollectSincePropagatesErrors(t *testing.T) {
	boom := errors.New("rate limited")
	_, err := collectSince(func(int64, int) ([]maxapi.IncomingMessage, int64, int, error) {
		return nil, 0, 0, boom
	}, 1, 0)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the fetch error (a failed read must not look like an empty history)", err)
	}
}

func TestRetryDelayGrowsAndCaps(t *testing.T) {
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for i, w := range want {
		if got := retryDelay(i); got != w {
			t.Errorf("retryDelay(%d) = %v, want %v", i, got, w)
		}
	}
	if retryDelay(1000) != retryMax {
		t.Error("a huge attempt count must stay at the cap")
	}
}

func TestStrikeOutNeedsCountAndTime(t *testing.T) {
	b := &Bridge{}
	for i := 0; i < 5; i++ {
		if b.strikeOut("m1") {
			t.Fatalf("struck out after %d quick failures; a short outage must not condemn a message", i+1)
		}
	}
	// Backdate the first failure: same count, but now spread over the span.
	b.cs.mu.Lock()
	rec := b.cs.strikes["m1"]
	rec.first = time.Now().Add(-2 * strikeSpan)
	b.cs.strikes["m1"] = rec
	b.cs.mu.Unlock()
	if !b.strikeOut("m1") {
		t.Fatal("repeated failures over a long span must strike out")
	}
	b.clearStrike("m1")
	if b.strikeOut("m1") {
		t.Fatal("a cleared message starts over")
	}
}
