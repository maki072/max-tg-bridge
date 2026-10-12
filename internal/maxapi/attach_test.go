package maxapi

import (
	"encoding/json"
	"testing"
)

// Shapes as logged live from MAX (values made up).
func TestShareContactStickerKeyboard(t *testing.T) {
	raw := `[
	 {"_type":"SHARE","contentLevel":false,"host":"ИнтернетУрок","description":"Школьный этап","shareId":1,
	  "title":"Олимпиада","url":"https://example.org/olimp","image":{"_type":"PHOTO","url":"https://i.example/x"}},
	 {"_type":"CONTACT","contactId":2,"firstName":"Айнур","lastName":"Тестов","name":"Айнур","phone":"+70000000000"},
	 {"_type":"CONTACT","contactId":3,"name":"Рустем","firstName":"Рустем"},
	 {"_type":"STICKER","stickerType":"LOTTIE","tags":["👋","🍄"],"url":"https://i.example/s.png","lottieUrl":"https://fd.example/s"},
	 {"_type":"INLINE_KEYBOARD","keyboard":{"buttons":[[{"type":"LINK","text":"Канал","url":"https://max.ru/join/x"}],
	  [{"type":"CALLBACK","text":"Проверить","payload":"menu:check"}]]}}
	]`
	var as []attach
	if err := json.Unmarshal([]byte(raw), &as); err != nil {
		t.Fatal(err)
	}
	got := toAttachments(as)
	if len(got) != 5 {
		t.Fatalf("got %d attachments: %+v", len(got), got)
	}
	if s := got[0]; s.Kind != "share" || s.URL != "https://example.org/olimp" || s.Name != "Олимпиада" || s.Text != "Школьный этап" || s.Host != "ИнтернетУрок" {
		t.Errorf("share: %+v", s)
	}
	if c := got[1]; c.Kind != "contact" || c.Name != "Айнур Тестов" || c.Phone != "+70000000000" {
		t.Errorf("contact: %+v", c)
	}
	if c := got[2]; c.Kind != "contact" || c.Name != "Рустем" || c.Phone != "" {
		t.Errorf("contact without phone: %+v", c)
	}
	if s := got[3]; s.Kind != "sticker" || s.URL != "https://i.example/s.png" || s.LottieURL != "https://fd.example/s" || s.Text != "👋🍄" {
		t.Errorf("sticker: %+v", s)
	}
	if k := got[4]; k.Kind != "keyboard" || k.Text != "🔗 Канал: https://max.ru/join/x\n🔘 Проверить" {
		t.Errorf("keyboard: %+v", k)
	}
}
