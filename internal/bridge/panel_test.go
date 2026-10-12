package bridge

import (
	"strings"
	"testing"
	"time"

	"maxtg/internal/maxapi"
)

func TestNormalizePhone(t *testing.T) {
	ok := map[string]string{
		"+79991234567":       "+79991234567",
		"89991234567":        "+79991234567",
		"9991234567":         "+79991234567",
		"+7 (999) 123-45-67": "+79991234567",
		"+380501234567":      "+380501234567",
	}
	for in, want := range ok {
		if got, good := normalizePhone(in); !good || got != want {
			t.Errorf("normalizePhone(%q) = %q,%v want %q", in, got, good, want)
		}
	}
	for _, in := range []string{"", "12345", "abc", "+1234567890123456"} {
		if got, good := normalizePhone(in); good {
			t.Errorf("normalizePhone(%q) = %q, want failure", in, got)
		}
	}
}

func TestSplitRefsAndIDs(t *testing.T) {
	refs := splitRefs("id123, #456;789\n+79991234567")
	if len(refs) != 4 {
		t.Fatalf("refs = %q", refs)
	}
	b := &Bridge{}
	ids, problems := b.resolveUsers([]string{"id123", "#456", "789", "id123", "idx"})
	if len(ids) != 3 || ids[0] != 123 || ids[1] != 456 || ids[2] != 789 {
		t.Errorf("ids = %v", ids)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "idx") {
		t.Errorf("problems = %v", problems)
	}
}

func TestCommandName(t *testing.T) {
	for in, want := range map[string]string{
		"/panel": "/panel", "/Panel@my_bot x": "/panel", "hello": "", "": "", "  /ban ": "/ban",
	} {
		if got := commandName(in); got != want {
			t.Errorf("commandName(%q) = %q want %q", in, got, want)
		}
	}
}

func TestMiniAppLink(t *testing.T) {
	if got := miniAppLink("some_bot", "p-1"); got != "https://max.ru/some_bot?startapp=p-1" {
		t.Errorf("got %q", got)
	}
	if got := miniAppLink("some_bot", "bad payload!"); got != "https://max.ru/some_bot?startapp" {
		t.Errorf("bad payload must be dropped, got %q", got)
	}
	if got := miniAppLink("bad name", ""); got != "" {
		t.Errorf("bad app name must give no link, got %q", got)
	}
}

func TestKeyboardMarkup(t *testing.T) {
	b := &Bridge{}
	att := maxapi.Attachment{Kind: "keyboard", CallbackID: "cb-1", Buttons: [][]maxapi.KeyButton{
		{{Type: "LINK", Text: "Сайт", URL: "https://example.org"}, {Type: "CALLBACK", Text: "Да", Payload: "yes"}},
		{{Type: "CLIPBOARD", Text: "Копия", Payload: "code"}, {Type: "MESSAGE", Text: "Привет"}},
		{{Type: "OPEN_APP", Text: "App", WebApp: "some_bot"}, {Type: "REQUEST_GEO_LOCATION", Text: "Где"}},
		{{Type: "LINK", Text: "Плохая", URL: "javascript:alert(1)"}, {Type: "WEIRD", Text: "Прочее"}, {Type: "LINK", Text: "  "}},
	}}
	m := b.keyboardMarkup(att, 99)
	if m == nil || len(m.InlineKeyboard) != 4 {
		t.Fatalf("markup = %+v", m)
	}
	r := m.InlineKeyboard
	if r[0][0].URL != "https://example.org" || r[0][0].CallbackData != "" {
		t.Errorf("link button: %+v", r[0][0])
	}
	cb := r[0][1]
	if !strings.HasPrefix(cb.CallbackData, cbKeyboard) || len(cb.CallbackData) > 64 {
		t.Fatalf("callback button: %+v", cb)
	}
	stored, ok := b.kb.get(strings.TrimPrefix(cb.CallbackData, cbKeyboard))
	if !ok || stored.chatID != 99 || stored.callbackID != "cb-1" || stored.payload != "yes" || stored.sendsText {
		t.Errorf("stored callback: %+v %v", stored, ok)
	}
	if r[1][0].CopyText == nil || r[1][0].CopyText.Text != "code" {
		t.Errorf("clipboard button: %+v", r[1][0])
	}
	msgBtn, _ := b.kb.get(strings.TrimPrefix(r[1][1].CallbackData, cbKeyboard))
	if !msgBtn.sendsText || msgBtn.text != "Привет" {
		t.Errorf("message button: %+v", msgBtn)
	}
	if r[2][0].URL != "https://max.ru/some_bot?startapp" || r[2][1].CallbackData != cbKbGeo {
		t.Errorf("open-app / geo: %+v", r[2])
	}
	if len(r[3]) != 2 || r[3][0].CallbackData != cbKbNA || r[3][1].CallbackData != cbKbNA {
		t.Errorf("unsupported / unsafe URL must degrade to the n/a button, blank text dropped: %+v", r[3])
	}
	if b.keyboardMarkup(maxapi.Attachment{Kind: "keyboard"}, 1) != nil {
		t.Error("an empty keyboard must give nil")
	}
}

func TestKbRegistryEvicts(t *testing.T) {
	var r kbRegistry
	first := r.add(kbButton{text: "first"})
	for i := 0; i < kbRegistryCap; i++ {
		r.add(kbButton{text: "x"})
	}
	if _, ok := r.get(strings.TrimPrefix(first, cbKeyboard)); ok {
		t.Error("the oldest button should have been evicted")
	}
}

func TestAlertCooldown(t *testing.T) {
	var a alertState
	if !a.cooled("k", time.Hour) {
		t.Fatal("first fire must pass")
	}
	if a.cooled("k", time.Hour) {
		t.Fatal("second fire inside the cooldown must be blocked")
	}
	if !a.cooled("other", time.Hour) {
		t.Fatal("a different key is independent")
	}
	if !a.cooled("k", 0) {
		t.Fatal("a zero cooldown never blocks")
	}
}
