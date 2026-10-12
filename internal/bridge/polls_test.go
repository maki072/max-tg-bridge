package bridge

import (
	"strings"
	"testing"

	"maxtg/internal/maxapi"
	"maxtg/internal/store"
)

func testPoll() maxapi.Poll {
	return maxapi.Poll{ID: 1, Title: "Обедают?", Total: 2, Answers: []maxapi.PollAnswer{
		{ID: 1, Text: "Да", Votes: 2, Mine: true}, {ID: 2, Text: "Нет"},
	}}
}

func TestFitsNativePoll(t *testing.T) {
	p := testPoll()
	if !fitsNativePoll(&p) {
		t.Fatal("simple poll should fit")
	}
	p.Answers[0].Text = strings.Repeat("я", 101)
	if fitsNativePoll(&p) {
		t.Fatal("101-char option must not fit")
	}
	one := maxapi.Poll{Title: "q", Answers: []maxapi.PollAnswer{{ID: 1, Text: "a"}}}
	if fitsNativePoll(&one) {
		t.Fatal("a single option must not fit")
	}
}

func TestRenderPoll(t *testing.T) {
	native := store.PollRec{TgPollID: "x", Header: "Лилия", Poll: testPoll()}
	text, kb := renderPoll(native)
	if !strings.Contains(text, "✅ Да — 2 (100%)") || !strings.Contains(text, "▫️ Нет — 0 (0%)") || len(kb.InlineKeyboard) != 1 {
		t.Fatalf("native render:\n%s\n%+v", text, kb)
	}
	buttons := store.PollRec{Header: "Лилия", Poll: testPoll()}
	text, kb = renderPoll(buttons)
	if !strings.Contains(text, "📊 Опрос: Обедают?") || len(kb.InlineKeyboard) != 3 || kb.InlineKeyboard[0][0].CallbackData != "pv:1" {
		t.Fatalf("button render:\n%s\n%+v", text, kb)
	}
}
