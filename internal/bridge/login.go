package bridge

import (
	"fmt"
	"log"
	"strings"
	"time"

	"maxtg/internal/config"
	"maxtg/internal/maxapi"
	"maxtg/internal/tg"
)

// Signing in to MAX from the Telegram bot, with no browser: /login in a private
// chat with the bot walks through phone → SMS code → (2FA password) and stores
// the session. The codes and the password are deleted from the chat the moment
// they arrive. Mirrors the /login flow of the Telemax bridge
// (github.com/Trollobot/Telemax); the implementation is ours.

const (
	cbLogin      = "lg:"
	loginFlowTTL = 10 * time.Minute
)

type loginFlow struct {
	step   string // phone | code | password
	chatID int64
	phone  string
	sess   *maxapi.AuthSession
	timer  *time.Timer
}

// maskPhone hides the middle of a phone number for chat output.
func maskPhone(p string) string {
	r := []rune(p)
	if len(r) < 8 {
		return p
	}
	return string(r[:5]) + "•••" + string(r[len(r)-2:])
}

func (b *Bridge) setBotName(n string) {
	b.loginMu.Lock()
	b.botName = strings.TrimPrefix(n, "@")
	b.loginMu.Unlock()
}

// loginLink is the deep link that opens a private chat with the bot and starts
// the sign-in ("" until the bot's username is known).
func (b *Bridge) loginLink() string {
	b.loginMu.Lock()
	defer b.loginMu.Unlock()
	if b.botName == "" {
		return ""
	}
	return "https://t.me/" + b.botName + "?start=login"
}

func (b *Bridge) loginFor(userID int64) *loginFlow {
	b.loginMu.Lock()
	defer b.loginMu.Unlock()
	return b.logins[userID]
}

// loginEnd drops a user's flow and closes its MAX connection.
func (b *Bridge) loginEnd(userID int64) {
	b.loginMu.Lock()
	fl := b.logins[userID]
	delete(b.logins, userID)
	b.loginMu.Unlock()
	if fl == nil {
		return
	}
	if fl.timer != nil {
		fl.timer.Stop()
	}
	if fl.sess != nil {
		fl.sess.Close()
	}
}

func (b *Bridge) loginSet(userID int64, fl *loginFlow) {
	b.loginMu.Lock()
	old := b.logins[userID]
	if b.logins == nil {
		b.logins = make(map[int64]*loginFlow)
	}
	if old != nil && old != fl {
		if old.timer != nil {
			old.timer.Stop()
		}
		if old.sess != nil && old.sess != fl.sess {
			old.sess.Close()
		}
	}
	if fl.timer != nil {
		fl.timer.Stop()
	}
	fl.timer = time.AfterFunc(loginFlowTTL, func() { b.loginExpire(userID, fl) })
	b.logins[userID] = fl
	b.loginMu.Unlock()
}

func (b *Bridge) loginExpire(userID int64, fl *loginFlow) {
	b.loginMu.Lock()
	cur := b.logins[userID]
	b.loginMu.Unlock()
	if cur != fl {
		return
	}
	b.loginEnd(userID)
	_, _ = b.bot.SendChat(fl.chatID, 0, "⌛ Вход в MAX отменён: слишком долго не было ответа. Начать снова — /login.", nil)
}

func (b *Bridge) reply(chatID int64, text string) {
	if _, err := b.bot.SendChat(chatID, 0, text, nil); err != nil {
		log.Printf("[login] reply failed: %v", err)
	}
}

// loginBegin starts the flow in a private chat; arg may carry the phone.
func (b *Bridge) loginBegin(chatID, userID int64, arg string) {
	if b.bot.GroupID() == 0 {
		b.reply(chatID, "Сначала добавьте бота администратором в группу с включёнными темами и напишите там любое сообщение — так мост узнает группу. Потом /login.")
		return
	}
	arg = strings.TrimSpace(arg)
	if arg != "" {
		b.loginSendSMS(chatID, userID, arg)
		return
	}
	if phone := b.cfg.Phone; phone != "" {
		b.loginSet(userID, &loginFlow{step: "confirm", chatID: chatID})
		_, _ = b.bot.SendChat(chatID, 0, "🔐 Вход в MAX.\nВойти с номером "+maskPhone(phone)+"?", kbd(row(
			btn("✅ Да", cbLogin+"yes"), btn("✏️ Другой номер", cbLogin+"other"))))
		return
	}
	b.loginSet(userID, &loginFlow{step: "phone", chatID: chatID})
	b.reply(chatID, "🔐 Вход в MAX.\nПришлите номер телефона MAX в формате +7XXXXXXXXXX. Отмена — /cancel.")
}

// loginSendSMS normalizes the phone, opens a fresh MAX connection and requests
// the code.
func (b *Bridge) loginSendSMS(chatID, userID int64, rawPhone string) {
	phone, ok := normalizePhone(rawPhone)
	if !ok {
		b.reply(chatID, "❌ Не похоже на номер телефона. Пришлите в формате +7XXXXXXXXXX или /cancel.")
		return
	}
	sess, err := maxapi.NewAuthSession(b.st.EnsureDeviceID())
	if err != nil {
		b.loginEnd(userID)
		b.reply(chatID, "❌ Не удалось подключиться к MAX: "+maxapi.DescribeError(err)+"\nПопробуйте позже: /login.")
		return
	}
	if err := sess.RequestSMS(phone); err != nil {
		sess.Close()
		b.loginSet(userID, &loginFlow{step: "phone", chatID: chatID})
		b.reply(chatID, "❌ "+err.Error()+"\nПроверьте номер и пришлите ещё раз, или /cancel.")
		return
	}
	b.loginSet(userID, &loginFlow{step: "code", chatID: chatID, phone: phone, sess: sess})
	b.reply(chatID, "📲 Код отправлен по SMS на "+maskPhone(phone)+". Пришлите его сюда одним сообщением — я сразу удалю его из чата.")
}

// handleLoginInput consumes a private-chat message that belongs to the sign-in
// (a trigger command, or the answer to the step in progress). Reports whether
// the message was consumed.
func (b *Bridge) handleLoginInput(msg *tg.Message) bool {
	userID, chatID := msg.From.ID, msg.Chat.ID
	text := strings.TrimSpace(msg.Text)
	cmd := commandName(text)
	_, arg, _ := strings.Cut(text, " ")

	switch {
	case cmd == "/login" || cmd == "/auth" || text == "/start login" || text == "🔐 Вход в MAX":
		b.loginBegin(chatID, userID, arg)
		return true
	case cmd == "/start" && strings.TrimSpace(arg) == "login":
		b.loginBegin(chatID, userID, "")
		return true
	}
	fl := b.loginFor(userID)
	if fl == nil {
		return false
	}
	if cmd == "/cancel" {
		b.loginEnd(userID)
		b.reply(chatID, "Отменено.")
		return true
	}
	if cmd != "" { // another command: let it run, but the flow stays open
		return false
	}
	switch fl.step {
	case "confirm", "phone":
		b.loginSendSMS(chatID, userID, text)
	case "code":
		_ = b.bot.DeleteChatMessage(chatID, msg.MessageID) // don't leave the code in the chat
		res, needPassword, hint, err := fl.sess.VerifyCode(text)
		switch {
		case err != nil:
			b.reply(chatID, "❌ "+err.Error()+"\nПришлите код ещё раз или /cancel.")
		case needPassword:
			b.loginSet(userID, &loginFlow{step: "password", chatID: chatID, phone: fl.phone, sess: fl.sess})
			if hint != "" {
				hint = "\nПодсказка: " + hint
			}
			b.reply(chatID, "🔐 На аккаунте включён пароль (2FA)."+hint+"\nПришлите пароль — я удалю ваше сообщение сразу.")
		default:
			b.loginFinish(userID, chatID, fl.phone, res)
		}
	case "password":
		_ = b.bot.DeleteChatMessage(chatID, msg.MessageID) // as early as possible
		res, err := fl.sess.CheckPassword(text)
		if err != nil {
			b.reply(chatID, "❌ Пароль не подошёл: "+err.Error()+"\nПришлите пароль ещё раз или /cancel.")
			return true
		}
		b.reply(chatID, "🔐 Пароль принят.")
		b.loginFinish(userID, chatID, fl.phone, res)
	}
	return true
}

// handleLoginCallback serves the two buttons of the "confirm the saved number" step.
func (b *Bridge) handleLoginCallback(cq *tg.CallbackQuery) {
	answer := func(t string, alert bool) { _ = b.bot.AnswerCallbackQuery(cq.ID, t, alert) }
	if cq.Message == nil || cq.Message.Chat.Type != "private" {
		answer("", false)
		return
	}
	userID, chatID := cq.From.ID, cq.Message.Chat.ID
	if !b.isAdmin(userID) {
		answer("Только для администратора группы", true)
		return
	}
	fl := b.loginFor(userID)
	if fl == nil || fl.step != "confirm" {
		answer("Сессия истекла — начните заново: /login", true)
		return
	}
	answer("", false)
	if cq.Data == cbLogin+"other" {
		b.loginSet(userID, &loginFlow{step: "phone", chatID: chatID})
		b.reply(chatID, "Пришлите номер телефона MAX в формате +7XXXXXXXXXX.")
		return
	}
	b.loginSendSMS(chatID, userID, b.cfg.Phone)
}

// loginFinish stores the new session, remembers the phone, and restarts the
// bridge so it reconnects with the new token over the right transport.
func (b *Bridge) loginFinish(userID, chatID int64, phone string, res *maxapi.AuthResult) {
	b.st.SetDesktopCredentials(b.st.EnsureDeviceID(), res.Token)
	if phone != "" && b.cfg.Phone != phone {
		b.cfg.Phone = phone
		if err := config.Save(b.configPath, b.cfg); err != nil {
			log.Printf("[login] saving the phone to config failed: %v", err)
		}
	}
	b.loginEnd(userID)
	log.Printf("[login] MAX signed in as %d (browserless, token stored)", res.SelfID)
	b.reply(chatID, fmt.Sprintf("✅ MAX авторизован (%s). Перезапускаюсь и подключаюсь…", maskPhone(phone)))
	b.notify("✅ MAX авторизован — мост перезапускается и скоро начнёт пересылку.")
	go b.restart()
}

// logout forgets the MAX session and restarts, leaving the bridge signed out.
func (b *Bridge) logout() {
	b.st.ClearCredentials()
	b.notify("🚪 Сессия MAX удалена. Войти снова — /login в личке бота. Саму сессию можно завершить в приложении MAX (Настройки → Устройства).")
	go b.restart()
}
