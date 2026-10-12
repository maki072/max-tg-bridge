package bridge

import (
	"crypto/rand"
	"encoding/base64"
	"log"
	"regexp"
	"strings"
	"sync"

	"maxtg/internal/maxapi"
	"maxtg/internal/tg"
)

// MAX bot keyboards in Telegram. The layout is kept; each button becomes its
// closest Telegram twin: LINK and OPEN_APP → URL buttons, CLIPBOARD → a copy
// button, CALLBACK and MESSAGE → a callback button that, when an admin presses
// it, is relayed to MAX in the bridged account's name. Button shapes follow
// what the Telemax bridge (github.com/Trollobot/Telemax) observed live.

const (
	cbKeyboard  = "kb:"        // + registry key: a relayable MAX button
	cbKbGeo     = "kb_geo"     // REQUEST_GEO_LOCATION: hint only
	cbKbContact = "kb_contact" // REQUEST_CONTACT: hint only
	cbKbNA      = "kb_na"      // anything else: no Telegram counterpart

	kbRegistryCap  = 2000 // pressable buttons remembered; older ones expire
	kbMaxPerRow    = 8
	kbMaxButtons   = 100
	kbMaxTextRunes = 60
)

// kbButton is what a relayable button needs when it is pressed.
type kbButton struct {
	chatID     int64
	callbackID string
	payload    string
	text       string
	sendsText  bool // MESSAGE button: pressing it sends its text as our own message
}

// kbRegistry maps the short callback_data keys (Telegram caps them at 64
// bytes) to the buttons they stand for. In memory only: after a restart old
// keyboards say "expired" and the button has to be pressed in MAX itself.
type kbRegistry struct {
	mu    sync.Mutex
	m     map[string]kbButton
	order []string
}

func (r *kbRegistry) add(btn kbButton) string {
	var raw [9]byte
	_, _ = rand.Read(raw[:])
	key := base64.RawURLEncoding.EncodeToString(raw[:])
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = make(map[string]kbButton)
	}
	r.m[key] = btn
	r.order = append(r.order, key)
	if len(r.order) > kbRegistryCap {
		delete(r.m, r.order[0])
		r.order = r.order[1:]
	}
	return cbKeyboard + key
}

func (r *kbRegistry) get(key string) (kbButton, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	btn, ok := r.m[key]
	return btn, ok
}

var (
	urlButtonRe    = regexp.MustCompile(`^(?i)(https?|tg)://.`)
	miniAppNameRe  = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)
	miniAppStartRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,512}$`)
)

// miniAppLink is the public deep link of a MAX mini-app button
// (https://max.ru/<bot>?startapp=<payload>); "" when the name isn't usable. A
// payload MAX would drop (wrong characters or over 512) is left out.
func miniAppLink(webApp, payload string) string {
	if !miniAppNameRe.MatchString(webApp) {
		return ""
	}
	link := "https://max.ru/" + webApp + "?startapp"
	if miniAppStartRe.MatchString(payload) {
		link += "=" + payload
	}
	return link
}

// keyboardMarkup converts a MAX keyboard attachment into a Telegram inline
// keyboard (nil when nothing usable is left). Clamped to Telegram's limits.
func (b *Bridge) keyboardMarkup(att maxapi.Attachment, chatID int64) *tg.InlineKeyboardMarkup {
	left := kbMaxButtons
	var rows [][]tg.InlineKeyboardButton
	for _, row := range att.Buttons {
		var out []tg.InlineKeyboardButton
		for _, bt := range row {
			if len(out) >= kbMaxPerRow || left <= 0 {
				break
			}
			text := truncateRunes(strings.TrimSpace(bt.Text), kbMaxTextRunes)
			if text == "" {
				continue
			}
			switch {
			case bt.Type == "LINK" && urlButtonRe.MatchString(bt.URL) && len(bt.URL) <= 2048:
				out = append(out, tg.InlineKeyboardButton{Text: text, URL: bt.URL})
			case bt.Type == "CLIPBOARD" && bt.Payload != "" && len(bt.Payload) <= 256:
				out = append(out, tg.InlineKeyboardButton{Text: text, CopyText: &tg.CopyTextInfo{Text: bt.Payload}})
			case bt.Type == "CALLBACK" && att.CallbackID != "":
				out = append(out, tg.InlineKeyboardButton{Text: text, CallbackData: b.kb.add(kbButton{
					chatID: chatID, callbackID: att.CallbackID, payload: bt.Payload, text: bt.Text,
				})})
			case bt.Type == "MESSAGE":
				out = append(out, tg.InlineKeyboardButton{Text: text, CallbackData: b.kb.add(kbButton{
					chatID: chatID, text: bt.Text, sendsText: true,
				})})
			case bt.Type == "OPEN_APP" && miniAppLink(bt.WebApp, bt.Payload) != "":
				out = append(out, tg.InlineKeyboardButton{Text: text, URL: miniAppLink(bt.WebApp, bt.Payload)})
			case bt.Type == "REQUEST_GEO_LOCATION":
				out = append(out, tg.InlineKeyboardButton{Text: text, CallbackData: cbKbGeo})
			case bt.Type == "REQUEST_CONTACT":
				out = append(out, tg.InlineKeyboardButton{Text: text, CallbackData: cbKbContact})
			default:
				out = append(out, tg.InlineKeyboardButton{Text: text, CallbackData: cbKbNA})
			}
			left--
		}
		if len(out) > 0 {
			rows = append(rows, out)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return &tg.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// handleKeyboardCallback relays a press of a bridged MAX bot button. Pressing
// acts in the bridged MAX account's name, so — like voting in polls — it is
// limited to group admins.
func (b *Bridge) handleKeyboardCallback(cq *tg.CallbackQuery) {
	answer := func(text string, alert bool) {
		if err := b.bot.AnswerCallbackQuery(cq.ID, text, alert); err != nil {
			log.Printf("[kb] answerCallbackQuery failed: %v", err)
		}
	}
	switch cq.Data {
	case cbKbNA:
		answer("Эта кнопка работает только в приложении MAX", false)
		return
	case cbKbGeo:
		answer("Бот просит геопозицию: отправьте её в эту тему (скрепка → Геопозиция) — мост передаст её в MAX.", true)
		return
	case cbKbContact:
		answer("Бот просит контакт: отправьте контакт в эту тему (скрепка → Контакт) — мост передаст его в MAX. "+
			"Подписи MAX у такого контакта нет, бот, который её проверяет, может его не принять.", true)
		return
	}
	if !b.isAdmin(cq.From.ID) {
		answer("Нажимать кнопки от имени MAX-аккаунта могут только администраторы группы", true)
		return
	}
	btn, ok := b.kb.get(strings.TrimPrefix(cq.Data, cbKeyboard))
	if !ok {
		answer("Кнопка устарела: мост перезапускался. Нажмите её в приложении MAX.", true)
		return
	}
	var err error
	if btn.sendsText {
		_, err = b.max.SendMessage(btn.chatID, btn.text)
	} else {
		err = b.max.PressButton(btn.chatID, btn.callbackID, btn.payload)
	}
	if err != nil {
		log.Printf("[kb] press of %q in chat %d failed: %v", btn.text, btn.chatID, err)
		answer("❌ Не удалось нажать в MAX: "+shortErr(err), true)
		return
	}
	log.Printf("[kb] button %q pressed in MAX chat %d", btn.text, btn.chatID)
	answer("", false)
}
