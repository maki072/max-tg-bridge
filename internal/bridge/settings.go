package bridge

import (
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"

	"maxtg/internal/config"
	"maxtg/internal/installer"
	"maxtg/internal/maxapi"
	"maxtg/internal/store"
	"maxtg/internal/tg"
)

// Settings and account management from the bot — everything the console menu
// offered after first-time setup (the Telegram bot token itself has to come
// from the console: the bot can't configure itself before it exists).

// handleContextCommand runs the commands that need to know which chat they came
// from (they answer in it). Reports whether text was one of them.
func (b *Bridge) handleContextCommand(chatID int64, text string) bool {
	switch commandName(text) {
	case "/panel":
		b.sendPanel(chatID)
	case "/settings":
		b.sendView(chatID, b.settingsView())
	case "/logout":
		if strings.Contains(strings.ToLower(text), "yes") {
			b.logout()
		} else {
			b.sendView(chatID, logoutConfirmView())
		}
	default:
		return false
	}
	return true
}

func (b *Bridge) sendView(chatID int64, v view) {
	if _, err := b.bot.SendChat(chatID, 0, v.text, v.markup); err != nil {
		log.Printf("[panel] send view failed: %v", err)
	}
}

func logoutConfirmView() view {
	return view{"🚪 Выйти из MAX и удалить сохранённую сессию? Мост перестанет пересылать сообщения, пока вы не войдёте снова (/login).",
		kbd(row(btn("✅ Да, выйти", cbPanel+"logouty"), btn("Отмена", cbPanel+"no")))}
}

// maskProxy hides the credentials of a proxy URL.
func maskProxy(p string) string {
	if p == "" {
		return "нет (напрямую)"
	}
	if u, err := url.Parse(p); err == nil && u.User != nil {
		u.User = url.User("***")
		return u.String()
	}
	return p
}

func (b *Bridge) transportLabel() string {
	_, token, _, _ := b.st.GetCredentials()
	label := "WebSocket (веб-клиент, токен из браузера)"
	if b.cfg.UseTCP(token != "", b.st.TokenKindOf()) {
		label = "TCP (десктоп-клиент, без браузера)"
	}
	if t := strings.ToLower(b.cfg.MaxTransport); t == "tcp" || t == "ws" {
		label += " — задан вручную"
	}
	return label
}

// versionLabel describes how the MAX app version is chosen.
func (b *Bridge) versionLabel() string {
	switch {
	case b.cfg.MaxAppVersion != "":
		return b.cfg.MaxAppVersion + " (задана вручную)"
	case b.cfg.MaxAppVersionAuto != "":
		return b.cfg.MaxAppVersionAuto + " (подобрана автоматически)"
	}
	return "автоподбор (начиная с " + maxapi.TCPVersion() + ")"
}

func (b *Bridge) versionView() view {
	return view{"🧬 Версия приложения MAX: " + b.versionLabel() + "\n\n" +
		"MAX отвечает «Приложение устарело», если мост представляется слишком старой версией. " +
		"Автоподбор сам перебирает версии выше, пока MAX не примет одну из них, и запоминает её. " +
		"Вручную можно указать версию из десктопного MAX (Настройки → О программе).",
		kbd(row(btn("🔎 Автоподбор", cbPanel+"verauto"), btn("✏️ Указать вручную", cbPanel+"verman")), backBtn("set"))}
}

func (b *Bridge) settingsView() view {
	_, token, _, _ := b.st.GetCredentials()
	account := "не авторизован — /login"
	if token != "" {
		account = "вход выполнен"
		if b.st.TokenKindOf() == store.TokenDesktop {
			account += " (через бота)"
		} else {
			account += " (токен из браузера)"
		}
		if b.cfg.Phone != "" {
			account += ", " + maskPhone(b.cfg.Phone)
		}
	}
	autostart := "выключен"
	toggle := "🔌 Включить автозапуск"
	if installer.AutostartRegistered() {
		autostart, toggle = "включён", "🔌 Выключить автозапуск"
	}
	text := "🛠 Настройки\n" +
		"• MAX: " + account + "\n" +
		"• Транспорт: " + b.transportLabel() + "\n" +
		"• Версия MAX: " + b.versionLabel() + "\n" +
		"• Прокси Telegram: " + maskProxy(b.cfg.TelegramProxy) + "\n" +
		fmt.Sprintf("• История в новой теме: %d сообщ.\n", b.cfg.HistoryCount) +
		fmt.Sprintf("• Дозагрузка после простоя: %d сообщ. на чат\n", b.cfg.SyncCount) +
		"• Автозапуск при входе в Windows: " + autostart
	hist := func(n int) tg.InlineKeyboardButton {
		mark := ""
		if b.cfg.HistoryCount == n {
			mark = "✓ "
		}
		return btn(mark+strconv.Itoa(n), fmt.Sprintf("%shist:%d", cbPanel, n))
	}
	syncB := func(n int) tg.InlineKeyboardButton {
		mark := ""
		if b.cfg.SyncCount == n {
			mark = "✓ "
		}
		return btn(mark+strconv.Itoa(n), fmt.Sprintf("%ssync:%d", cbPanel, n))
	}
	return view{text, kbd(
		row(btn("🔐 Вход в MAX", cbPanel+"login"), btn("🚪 Выйти из MAX", cbPanel+"logout")),
		row(btn("🌐 Прокси Telegram", cbPanel+"proxy"), btn("🔁 Транспорт", cbPanel+"tr")),
		row(btn("🧬 Версия MAX", cbPanel+"ver")),
		row(btn(toggle, cbPanel+"auto")),
		row(btn("📚 История:", cbPanel+"set"), hist(0), hist(20), hist(50), hist(100)),
		row(btn("🔄 Дозагрузка:", cbPanel+"set"), syncB(0), syncB(50), syncB(200)),
		backBtn("root"),
	)}
}

// settingsAction serves the settings-related panel buttons. Reports whether
// action was one of them.
func (b *Bridge) settingsAction(action, rest string, cq *tg.CallbackQuery, show func(view), answer func(string, bool)) bool {
	chatID := cq.Message.Chat.ID
	switch action {
	case "set":
		show(b.settingsView())
		answer("", false)
	case "login":
		if cq.Message.Chat.Type == "private" {
			answer("", false)
			b.loginBegin(chatID, cq.From.ID, "")
		} else if link := b.loginLink(); link != "" {
			answer("Откройте личку с ботом: "+link+" — или отправьте боту /login", true)
		} else {
			answer("Откройте личку с ботом и отправьте /login", true)
		}
	case "hist", "sync":
		n, err := strconv.Atoi(rest)
		if err != nil || n < 0 || n > 200 {
			answer("", false)
			return true
		}
		if action == "hist" {
			b.cfg.HistoryCount = n
		} else {
			b.cfg.SyncCount = n
		}
		b.saveConfig()
		show(b.settingsView())
		answer("Сохранено", false)
	case "tr":
		next := map[string]string{"": "tcp", "auto": "tcp", "tcp": "ws", "ws": ""}[strings.ToLower(b.cfg.MaxTransport)]
		b.cfg.MaxTransport = next
		b.saveConfig()
		answer("Транспорт сохранён, перезапускаюсь…", false)
		show(b.settingsView())
		b.notify("🔁 Транспорт MAX изменён — мост перезапускается.")
		go b.restart()
	case "ver":
		show(b.versionView())
		answer("", false)
	case "verauto":
		b.cfg.MaxAppVersion, b.cfg.MaxAppVersionAuto = "", ""
		b.saveConfig()
		answer("Автоподбор включён, перезапускаюсь…", false)
		show(b.versionView())
		b.notify("🧬 Версия MAX: автоподбор — мост перезапускается.")
		go b.restart()
	case "verman":
		b.setPrompt(cq.From.ID, prompt{kind: "set_version", chatID: chatID, panelMsg: cq.Message.MessageID})
		show(view{"✏️ Пришлите версию приложения MAX в формате 26.24.0 (из десктопного MAX: Настройки → О программе).\n\n/cancel — отмена",
			kbd(backBtn("ver"))})
		answer("", false)
	case "auto":
		var err error
		if installer.AutostartRegistered() {
			err = installer.Uninstall()
		} else {
			err = installer.Install()
		}
		if err != nil {
			answer("❌ "+shortErr(err), true)
		} else {
			answer("Готово", false)
		}
		show(b.settingsView())
	case "proxy":
		b.setPrompt(cq.From.ID, prompt{kind: "set_proxy", chatID: chatID, panelMsg: cq.Message.MessageID})
		show(view{"🌐 Прокси для Telegram (MAX всегда идёт напрямую).\nОтправьте адрес вида socks5://host:1080 или socks5://user:pass@host:1080.\n«-» — отключить прокси. Адрес проверяется до сохранения.\n\n/cancel — отмена",
			kbd(backBtn("set"))})
		answer("", false)
	case "logout":
		show(logoutConfirmView())
		answer("", false)
	case "logouty":
		answer("Выхожу…", false)
		b.logout()
	default:
		return false
	}
	return true
}

func (b *Bridge) saveConfig() {
	if err := config.Save(b.configPath, b.cfg); err != nil {
		log.Printf("[settings] saving config failed: %v", err)
	}
}

// applyVersion saves a MAX app version typed into the panel and restarts.
func (b *Bridge) applyVersion(p prompt, text string) {
	v := strings.TrimSpace(text)
	if !maxapi.ValidVersion(v) {
		b.showPanelView(p, view{"❌ Не похоже на версию. Пример: 26.24.0", kbd(row(btn("✏️ Ещё раз", cbPanel+"verman")), backBtn("ver"))})
		return
	}
	b.cfg.MaxAppVersion = v
	b.saveConfig()
	b.showPanelView(p, view{"✅ Версия MAX сохранена: " + v + ". Перезапускаюсь…", kbd(backBtn("set"))})
	go b.restart()
}

// applyProxy validates a proxy typed into the panel (a real getMe through it),
// saves it, and restarts so the Telegram client uses it.
func (b *Bridge) applyProxy(p prompt, text string) {
	back := kbd(backBtn("set"))
	proxy := strings.TrimSpace(text)
	if proxy == "-" {
		proxy = ""
	} else if u, err := url.Parse(proxy); err != nil || u.Host == "" || !strings.Contains("socks5 socks5h http https", u.Scheme) {
		b.showPanelView(p, view{"❌ Не похоже на адрес прокси. Пример: socks5://127.0.0.1:1080", kbd(row(btn("🌐 Ещё раз", cbPanel+"proxy")), backBtn("set"))})
		return
	}
	if proxy != "" {
		test, err := tg.New(b.cfg.TelegramBotToken, b.bot.GroupID(), proxy)
		if err == nil {
			_, err = test.GetMe()
		}
		if err != nil {
			b.showPanelView(p, view{"❌ Через этот прокси Telegram не отвечает: " + shortErr(err) + "\nНастройка не сохранена.", back})
			return
		}
	}
	b.cfg.TelegramProxy = proxy
	b.saveConfig()
	b.showPanelView(p, view{"✅ Прокси сохранён: " + maskProxy(proxy) + ". Перезапускаюсь…", back})
	go b.restart()
}
