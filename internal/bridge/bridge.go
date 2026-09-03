package bridge

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"maxtg/internal/browsergrab"
	"maxtg/internal/config"
	"maxtg/internal/maxapi"
	"maxtg/internal/paths"
	"maxtg/internal/store"
	"maxtg/internal/tg"
)

type Bridge struct {
	max          *maxapi.Client
	bot          *tg.Bot
	st           *store.Store
	cfg          *config.Config
	configPath   string
	warnOnce     sync.Once
	authCodeChan chan string
	authCapChan  chan string
	authMu       sync.Mutex
	authActive   atomic.Bool // true while an SMS auth flow is waiting for input
}

func New(max *maxapi.Client, bot *tg.Bot, st *store.Store, cfg *config.Config, configPath string) *Bridge {
	return &Bridge{
		max:          max,
		bot:          bot,
		st:           st,
		cfg:          cfg,
		configPath:   configPath,
		// Buffered (size 1) so a code/captcha the admin sends a moment before the
		// auth goroutine reaches its receive is queued instead of being dropped.
		authCodeChan: make(chan string, 1),
		authCapChan:  make(chan string, 1),
	}
}

// drainString removes any stale queued value from a buffered channel.
func drainString(ch chan string) {
	select {
	case <-ch:
	default:
	}
}

func (b *Bridge) notify(text string) {
	if b.bot.GroupID() != 0 {
		_ = b.bot.SendMessage(text)
	}
}

func (b *Bridge) OnMaxMessage(m maxapi.IncomingMessage) {
	if b.bot.GroupID() == 0 {
		return
	}
	if m.MessageID != "" && b.st.SeenAndMark("max:"+m.MessageID) {
		return
	}

	chatKey := strconv.FormatInt(m.ChatID, 10)
	threadID, ok := b.st.Topic(chatKey)
	if !ok {
		id, err := b.bot.CreateForumTopic(m.ChatTitle)
		if err != nil {
			log.Printf("[bridge] createForumTopic for %q failed: %v", m.ChatTitle, err)
			b.notify(fmt.Sprintf("⚠️ Failed to create a topic for MAX chat %q: %v", m.ChatTitle, err))
			return
		}
		threadID = id
		b.st.SetTopic(chatKey, threadID)
	}

	text := fmt.Sprintf("%s:\n%s", m.SenderName, m.Text)
	if err := b.bot.SendToTopic(threadID, text); err != nil {
		log.Printf("[bridge] sendToTopic %d failed: %v", threadID, err)
		b.notify(fmt.Sprintf("⚠️ Failed to deliver a MAX message to its topic: %v", err))
	}
}

func (b *Bridge) RunTelegram() {
	offset := b.st.GetOffset()
	for {
		updates, err := b.bot.GetUpdates(offset, 30)
		if err != nil {
			log.Printf("[tg] getUpdates: %v", err)
			time.Sleep(3 * time.Second)
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			b.safeDispatch(u)
		}
		b.st.SetOffset(offset)
	}
}

// safeDispatch processes one Telegram update, recovering from any panic so a
// single bad update can never take the whole bridge down (nothing else would
// restart it in the user-session autostart model).
func (b *Bridge) safeDispatch(u tg.Update) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[tg] recovered from panic handling update %d: %v", u.UpdateID, r)
		}
	}()
	b.autoDetectGroup(u)
	b.handleUpdate(u)
}

func (b *Bridge) autoDetectGroup(u tg.Update) {
	if b.bot.GroupID() != 0 || u.Message == nil {
		return
	}
	c := u.Message.Chat
	if c.Type != "supergroup" && c.Type != "group" {
		return
	}
	b.bot.SetGroupID(c.ID)
	b.cfg.TelegramGroupID = c.ID
	if err := config.Save(b.configPath, b.cfg); err != nil {
		log.Printf("[bridge] group id detected (%d) but saving config failed: %v", c.ID, err)
	} else {
		log.Printf("[bridge] auto-detected Telegram group id=%d (%q), saved to config", c.ID, c.Title)
		b.notify("✅ Бот успешно подключен к группе! Администраторы могут перейти в личные сообщения бота для вызова графического меню управления.")
	}
}

func (b *Bridge) isAdmin(userID int64) bool {
	if b.bot.GroupID() == 0 {
		return false
	}
	admins, err := b.bot.GetChatAdministrators(b.bot.GroupID())
	if err != nil {
		return false
	}
	for _, id := range admins {
		if id == userID {
			return true
		}
	}
	return false
}

func (b *Bridge) handleUpdate(u tg.Update) {
	msg := u.Message
	if msg == nil || msg.Text == "" || msg.From.IsBot {
		return
	}

	// 1. Приватные сообщения боту (Личка) - только для Администраторов группы!
	if msg.Chat.Type == "private" {
		if !b.isAdmin(msg.From.ID) {
			_ = b.bot.SendMenu(msg.Chat.ID, "У вас нет прав администратора в привязанной группе MAX-TG.", tg.ReplyKeyboardMarkup{ResizeKeyboard: true})
			return
		}
		b.handleAdminPM(msg.Chat.ID, msg.Text)
		return
	}

	// 2. Действия внутри привязанной супергруппы
	if b.bot.GroupID() != 0 && msg.Chat.ID == b.bot.GroupID() {
		// General topic
		if msg.MessageThreadID == 0 {
			if strings.HasPrefix(msg.Text, "/") {
				// Оставим базовые команды в General на всякий случай
				if reply := b.processCommand(msg.Text); reply != "" {
					b.notify(reply)
				}
			}
			return
		}

		// A named topic: forward the reply to the MAX chat it's mapped to.
		chatKey, ok := b.st.ChatByTopic(msg.MessageThreadID)
		if !ok {
			return
		}
		chatID, err := strconv.ParseInt(chatKey, 10, 64)
		if err != nil {
			return
		}
		if err := b.max.SendMessage(chatID, msg.Text); err != nil {
			log.Printf("[bridge] MAX send to chat %d failed: %v", chatID, err)
		}
	}
}

func (b *Bridge) handleAdminPM(chatID int64, text string) {
	cmd := strings.TrimSpace(text)
	
	menu := tg.ReplyKeyboardMarkup{
		Keyboard: [][]tg.KeyboardButton{
			{{Text: "📊 Статус"}, {Text: "🔄 Перезапустить"}},
			{{Text: "📱 SMS Авторизация (рекомендуется)"}},
			{{Text: "📄 Логи службы"}, {Text: "📋 Сырой лог (Debug)"}},
			{{Text: "🩺 Диагностика MAX"}, {Text: "💬 Тестовое сообщение"}},
			{{Text: "🌐 Кража токена из браузера (Опасно, могут пропасть пуши)"}},
		},
		ResizeKeyboard:  true,
		OneTimeKeyboard: false,
	}

	// Транслируем текстовые кнопки в команды
	switch cmd {
	case "📊 Статус":
		cmd = "/status"
	case "🔄 Перезапустить":
		cmd = "/restart"
	case "📱 SMS Авторизация (рекомендуется)":
		cmd = "/auth"
	case "📄 Логи службы":
		cmd = "/log"
	case "📋 Сырой лог (Debug)":
		cmd = "/debuglog"
	case "🩺 Диагностика MAX":
		cmd = "/maxdiag"
	case "💬 Тестовое сообщение":
		cmd = "/testmsg"
	case "🌐 Кража токена из браузера (Опасно, могут пропасть пуши)":
		cmd = "/pulltoken"
	default:
		// Возможно, юзер ввел СМС код (6 цифр)
		if _, err := strconv.Atoi(cmd); err == nil && len(cmd) >= 4 && len(cmd) <= 8 {
			cmd = "/code " + cmd
		}
	}

	reply := b.processCommand(cmd)
	if reply == "" {
		reply = "Выберите действие в графическом меню управления мостом MAX-TG 👇"
	}
	
	_ = b.bot.SendMenu(chatID, reply, menu)
}

func (b *Bridge) processCommand(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	cmd := strings.ToLower(fields[0])
	
	// Strip a "@botname" suffix if present
	if i := strings.Index(cmd, "@"); i >= 0 {
		cmd = cmd[:i]
	}

	switch cmd {
	case "/start", "/help", "/menu":
		return "Добро пожаловать в панель управления мостом MAX-TG!"
	case "/status", "/ping":
		connected, selfID, lastErr := b.max.Status()
		
		s := "🔴 MAX: Отключен"
		if connected {
			s = fmt.Sprintf("🟢 MAX: Подключен (ID: %d)", selfID)
		}
		
		var sb strings.Builder
		sb.WriteString(s + "\n")
		if lastErr != "" {
			sb.WriteString("⚠️ Последняя ошибка MAX: " + lastErr + "\n")
		}
		sb.WriteString(fmt.Sprintf("🔹 Группа TG ID: %d\n", b.bot.GroupID()))
		return sb.String()

	case "/log":
		n := 20
		if len(fields) > 1 {
			if v, err := strconv.Atoi(fields[1]); err == nil && v > 0 {
				n = v
			}
		}
		if n > 200 { n = 200 }
		return b.tailLog(n)

	case "/auth":
		phone := b.cfg.Phone
		if len(fields) > 1 {
			phone = fields[1]
		}
		go b.runSmsAuth(phone)
		return fmt.Sprintf("⏳ Запущена SMS-авторизация для %s.\nЖду код (отправьте его сюда сообщением 6 цифр).", phone)

	case "/code":
		if len(fields) < 2 {
			return "Использование: /code 123456"
		}
		if !b.authActive.Load() {
			return "❌ SMS-авторизация сейчас не запущена. Нажмите соответствующую кнопку в меню."
		}
		select {
		case b.authCodeChan <- fields[1]:
			return "✅ Код отправлен на сервер."
		default:
			return "⚠️ Код уже принят, дождитесь ответа сервера."
		}

	case "/captcha":
		if len(fields) < 2 {
			return "Использование: /captcha <token>"
		}
		if !b.authActive.Load() {
			return "❌ Сейчас не нужно."
		}
		select {
		case b.authCapChan <- fields[1]:
			return "✅ Токен капчи отправлен."
		default:
			return "⚠️ Токен капчи уже принят."
		}

	case "/settoken":
		if len(fields) < 2 {
			return "Использование: /settoken <token>"
		}
		token := fields[1]
		ourDeviceID := b.st.EnsureDeviceID()
		_, _, _, cookie := b.st.GetCredentials()
		b.st.SetCredentials(ourDeviceID, token, "", cookie)
		b.notify("Токен изменен вручную. Рестарт...")
		go b.restart()
		return ""

	case "/pulltoken":
		if b.cfg.BrowserCDPURL == "" {
			return "❌ Ошибка: выключено в настройках (browser_cdp_url не задан)."
		}
		go b.runPullToken()
		return "🔍 Ищу открытую вкладку браузера..."

	case "/setopcode":
		if len(fields) < 2 {
			return "Использование: /setopcode <N>"
		}
		op, err := strconv.Atoi(fields[1])
		if err != nil {
			return "Номер опкода недействителен."
		}
		b.cfg.IncomingOpcode = op
		_ = config.Save(b.configPath, b.cfg)
		b.notify("Opcode установлен в " + strconv.Itoa(op) + ". Перезапускаюсь...")
		go b.restart()
		return ""

	case "/testmsg":
		b.notify("Тестовое сервисное сообщение из моста в General!")
		return "✅ Тестовое сообщение отправлено."

	case "/restart":
		b.notify("🔄 Служба перезапускается...")
		go b.restart()
		return ""

	case "/maxdiag":
		go func() {
			defer func() {
				if r := recover(); r != nil {
					b.notify(fmt.Sprintf("🩺 Диагностика упала: %v", r))
				}
			}()
			b.notify(maxapi.Diagnose(b.cfg.MaxWSURL))
		}()
		return "🩺 Запускаю диагностику подключения к MAX (до ~30 сек)..."

	case "/debuglog":
		logs := b.max.DumpDebugLog()
		if len(logs) == 0 {
			return "📋 Debug log пуст. Сервер пока не присылал событий (Push/Ping)."
		}
		return "--- Последние фреймы от MAX ---\n\n" + strings.Join(logs[len(logs)-10:], "\n\n")

	default:
		return ""
	}
}

func (b *Bridge) runSmsAuth(phone string) {
	b.authMu.Lock()
	defer b.authMu.Unlock()

	b.authActive.Store(true)
	defer b.authActive.Store(false)

	client := maxapi.New(maxapi.Options{
		URL:       b.cfg.MaxWSURL,
		Phone:     phone,
		DeviceID:  b.st.EnsureDeviceID(),
		AllowSMS:  true,
		SaveToken: func(d, t, u, ck string) { b.st.SetCredentials(d, t, u, ck) },
		LoadToken: func() (string, string, string) { d, t, _, ck := b.st.GetCredentials(); return d, t, ck },
		ReadSMSCode: func() string {
			drainString(b.authCodeChan)
			select {
			case code := <-b.authCodeChan:
				return code
			case <-time.After(2 * time.Minute):
				return ""
			}
		},
		OnCaptcha: func(link string) string {
			drainString(b.authCapChan)
			b.notify("⚠️ MAX требует капчу. Откройте ссылку:\n" + link + "\nОтправьте токен через /captcha <token>")
			select {
			case token := <-b.authCapChan:
				return token
			case <-time.After(5 * time.Minute):
				return ""
			}
		},
	})
	if err := client.AuthOnce(); err != nil {
		b.notify(fmt.Sprintf("❌ Ошибка авторизации: %v", err))
		return
	}
	b.notify("✅ Токен успешно получен и сохранен через СМС! Служба перезапускается...")
	go b.restart()
}

// restart re-launches the bridge in the same user session, then exits the
// current process. The autostart model runs the bridge as a user-session app
// (not a service), so a plain exit would leave nothing to bring it back — we
// spawn a fresh copy first. The child inherits this process's (minimized)
// console, so no new window appears.
func (b *Bridge) restart() {
	time.Sleep(2 * time.Second)
	exe, err := os.Executable()
	if err != nil {
		log.Printf("[bridge] restart: cannot locate exe: %v (staying up)", err)
		return // do not exit if we cannot relaunch — a dead process won't come back
	}
	cmd := exec.Command(exe, "run", "-config", b.configPath) // relaunch self in-session
	if err := cmd.Start(); err != nil {
		log.Printf("[bridge] restart: relaunch failed: %v (staying up)", err)
		return // keep the current instance alive rather than exiting into nothing
	}
	os.Exit(0)
}

func (b *Bridge) runPullToken() {
	browserDeviceID, token, userAgent, cookie, err := browsergrab.Grab(b.cfg.BrowserCDPURL, b.cfg.BrowserMatchURL, 25*time.Second)
	if err != nil {
		b.notify(fmt.Sprintf("❌ Ошибка кражи токена из браузера: %v", err))
		return
	}
	if token == "" {
		b.notify("❌ Браузер не вернул токен.")
		return
	}

	// Keep OUR OWN persistent deviceId (never the browser's live one), so this
	// reader does not collide with the browser's push session — reusing the
	// browser's deviceId is what made the token flip back to "missing".
	ourDeviceID := b.st.EnsureDeviceID()
	_ = browserDeviceID
	b.st.SetCredentials(ourDeviceID, token, userAgent, cookie)

	b.notify("✅ Токен вытянут из браузера! Служба перезапускается...")
	go b.restart()
}

func (b *Bridge) tailLog(n int) string {
	data, err := os.ReadFile(paths.Log())
	if err != nil {
		return fmt.Sprintf("No log yet (%s): %v", paths.Log(), err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := strings.Join(lines, "\n")
	const maxLen = 3500 // Telegram messages cap at 4096 chars; leave headroom
	if len(out) > maxLen {
		out = "…(truncated)…\n" + out[len(out)-maxLen:]
	}
	if strings.TrimSpace(out) == "" {
		return "(log is empty)"
	}
	return out
}





