package bridge

import (
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"maxtg/internal/maxapi"
	"maxtg/internal/store"
	"maxtg/internal/tg"
)

// The control panel: an inline-keyboard card (/panel, in the group's General
// topic or in a private chat with the bot; admins only) for finding MAX
// contacts and starting chats, muting chats, creating groups, pausing MAX and
// checking status. The idea and the feature set follow the /panel of the
// Telemax bridge (github.com/Trollobot/Telemax); this implementation is ours.

// Callback-data scheme: "pn:<action>[:<arg>]", at most 64 bytes in total.
const (
	cbPanel = "pn:"

	promptTTL = 5 * time.Minute
)

type prompt struct {
	kind     string // find_phone | find_id | find_nick | new_group
	chatID   int64  // where the panel card lives
	panelMsg int64
	at       time.Time
}

type view struct {
	text   string
	markup *tg.InlineKeyboardMarkup
}

func btn(text, data string) tg.InlineKeyboardButton {
	return tg.InlineKeyboardButton{Text: text, CallbackData: data}
}

func kbd(rows ...[]tg.InlineKeyboardButton) *tg.InlineKeyboardMarkup {
	return &tg.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func row(bs ...tg.InlineKeyboardButton) []tg.InlineKeyboardButton { return bs }

func backBtn(to string) []tg.InlineKeyboardButton { return row(btn("◀️ Назад", cbPanel+to)) }

// ---- prompts: "the admin's next message is an answer" ----

func (b *Bridge) setPrompt(userID int64, p prompt) {
	b.promptMu.Lock()
	defer b.promptMu.Unlock()
	if b.prompts == nil {
		b.prompts = make(map[int64]prompt)
	}
	p.at = time.Now()
	b.prompts[userID] = p
}

func (b *Bridge) hasPrompt(userID int64) bool {
	b.promptMu.Lock()
	defer b.promptMu.Unlock()
	p, ok := b.prompts[userID]
	return ok && time.Since(p.at) < promptTTL
}

// takePrompt consumes the admin's pending prompt, if any, with text as the
// answer; reports whether the message was an answer (so it must not be handled
// as anything else). "/cancel" abandons the prompt.
func (b *Bridge) takePrompt(userID int64, text string) bool {
	b.promptMu.Lock()
	p, ok := b.prompts[userID]
	if ok {
		delete(b.prompts, userID)
	}
	b.promptMu.Unlock()
	if !ok || time.Since(p.at) >= promptTTL {
		return false
	}
	text = strings.TrimSpace(text)
	if commandName(text) == "/cancel" {
		b.showPanelView(p, b.rootView())
		return true
	}
	go b.answerPrompt(p, text)
	return true
}

// commandName returns the lower-cased "/command" a message starts with, without
// any "@botname" suffix ("" if it isn't a command).
func commandName(text string) string {
	f := strings.Fields(text)
	if len(f) == 0 || !strings.HasPrefix(f[0], "/") {
		return ""
	}
	cmd := strings.ToLower(f[0])
	if i := strings.Index(cmd, "@"); i >= 0 {
		cmd = cmd[:i]
	}
	return cmd
}

// showPanelView replaces the panel card a prompt belongs to (or posts a new one
// if it can no longer be edited).
func (b *Bridge) showPanelView(p prompt, v view) {
	if p.panelMsg != 0 {
		if err := b.bot.EditChatMessage(p.chatID, p.panelMsg, v.text, v.markup); err == nil {
			return
		}
	}
	if _, err := b.bot.SendChat(p.chatID, 0, v.text, v.markup); err != nil {
		log.Printf("[panel] cannot show panel view: %v", err)
	}
}

func (b *Bridge) answerPrompt(p prompt, text string) {
	defer func() {
		if r := recover(); r != nil {
			b.ReportPanic("panel", r)
		}
	}()
	back := kbd(row(btn("◀️ К панели", cbPanel+"root")))
	switch p.kind {
	case "find_phone":
		phone, ok := normalizePhone(text)
		if !ok {
			b.showPanelView(p, view{"❌ Не похоже на номер телефона. Пример: +79991234567", kbd(row(btn("🔢 Ещё раз", cbPanel+"find:phone")), backBtn("contacts"))})
			return
		}
		ct, err := b.max.FindByPhone(phone)
		switch {
		case err != nil:
			b.showPanelView(p, view{"❌ Поиск не удался: " + shortErr(err), back})
		case ct == nil:
			b.showPanelView(p, view{"😕 Номер " + phone + " не найден в MAX (или человек скрыл поиск по номеру — тогда пусть напишет первым).", kbd(backBtn("contacts"))})
		default:
			b.showPanelView(p, contactCard(*ct))
		}
	case "find_id":
		id, err := strconv.ParseInt(strings.TrimPrefix(strings.ToLower(text), "id"), 10, 64)
		if err != nil || id <= 0 {
			b.showPanelView(p, view{"❌ ID — это число, например 123456789", kbd(row(btn("🆔 Ещё раз", cbPanel+"find:id")), backBtn("contacts"))})
			return
		}
		ct, err := b.max.ContactByID(id)
		switch {
		case err != nil:
			b.showPanelView(p, view{"❌ Поиск не удался: " + shortErr(err), back})
		case ct == nil:
			b.showPanelView(p, view{fmt.Sprintf("😕 Пользователь с ID %d не найден.", id), kbd(backBtn("contacts"))})
		default:
			b.showPanelView(p, contactCard(*ct))
		}
	case "find_nick":
		contacts, channels, err := b.max.PublicSearch(text)
		if err != nil {
			b.showPanelView(p, view{"❌ Поиск не удался: " + shortErr(err), back})
			return
		}
		b.showPanelView(p, searchResults(text, contacts, channels))
	case "new_group":
		b.createGroupFromPrompt(p, text)
	}
}

// contactCard is the result card of a contact lookup.
func contactCard(ct maxapi.Contact) view {
	name := ct.Name
	if name == "" {
		name = fmt.Sprintf("Пользователь %d", ct.ID)
	}
	lines := []string{"👤 " + name}
	if ct.Phone != "" {
		lines = append(lines, "📞 +"+ct.Phone)
	}
	lines = append(lines, fmt.Sprintf("🆔 %d", ct.ID))
	if ct.Link != "" {
		lines = append(lines, "🔗 "+ct.Link)
	}
	if ct.Description != "" {
		lines = append(lines, "", truncateRunes(ct.Description, 300))
	}
	return view{strings.Join(lines, "\n"), kbd(
		row(btn("💬 Начать чат", fmt.Sprintf("%schat:%d", cbPanel, ct.ID))),
		backBtn("contacts"),
	)}
}

func searchResults(query string, contacts []maxapi.Contact, channels []maxapi.Channel) view {
	if len(contacts) == 0 && len(channels) == 0 {
		return view{"😕 По запросу «" + truncateRunes(query, 60) + "» ничего нет. Так находятся только публичные боты и каналы, не люди — людей ищите по номеру или ID.",
			kbd(backBtn("contacts"))}
	}
	lines := []string{"🔎 Найдено по «" + truncateRunes(query, 60) + "»:"}
	var rows [][]tg.InlineKeyboardButton
	for i, ct := range contacts {
		if i >= 8 {
			break
		}
		name := ct.Name
		if name == "" {
			name = fmt.Sprintf("ID %d", ct.ID)
		}
		line := "🤖 " + name
		if ct.Link != "" {
			line += " — " + ct.Link
		}
		lines = append(lines, line)
		rows = append(rows, row(btn("💬 "+truncateRunes(name, 40), fmt.Sprintf("%schat:%d", cbPanel, ct.ID))))
	}
	for i, ch := range channels {
		if i >= 8 {
			break
		}
		line := "📢 " + ch.Title
		if ch.Link != "" {
			line += " — " + ch.Link
		}
		lines = append(lines, line)
	}
	rows = append(rows, backBtn("contacts"))
	return view{strings.Join(lines, "\n"), kbd(rows...)}
}

// ---- views ----

func (b *Bridge) pauseLabel() string {
	until, ok := b.max.PausedUntil()
	if !ok {
		return ""
	}
	if time.Until(until) > 365*24*time.Hour {
		return "до перезапуска"
	}
	return "до " + until.Format("15:04")
}

func (b *Bridge) statusLine() string {
	if l := b.pauseLabel(); l != "" {
		return "⏸ MAX на паузе (" + l + ")"
	}
	connected, selfID, lastErr := b.max.Status()
	if connected {
		return fmt.Sprintf("🟢 MAX подключён (ID %d)", selfID)
	}
	s := "🔴 MAX не подключён"
	if lastErr != "" {
		s += "\n⚠️ " + truncateRunes(lastErr, 200)
	}
	return s
}

func (b *Bridge) rootView() view {
	chats := 0
	for k := range b.st.AllTopics() {
		if !strings.HasPrefix(k, store.PendingPrefix) {
			chats++
		}
	}
	text := fmt.Sprintf("🎛 Панель управления MAX ↔ Telegram\n%s\nЧатов в Telegram: %d · заглушено: %d", b.statusLine(), chats, len(b.st.Bans()))
	return view{text, kbd(
		row(btn("👤 Найти контакт", cbPanel+"contacts"), btn("👥 Группы", cbPanel+"groups")),
		row(btn("🔕 Заглушенные чаты", cbPanel+"muted"), btn("⚙️ Система", cbPanel+"sys")),
	)}
}

func contactsView() view {
	return view{"👤 Найти контакт в MAX и начать с ним чат.\nЧем искать:", kbd(
		row(btn("🔢 По номеру", cbPanel+"find:phone"), btn("🆔 По ID", cbPanel+"find:id")),
		row(btn("🔎 Бот или канал по названию/нику", cbPanel+"find:nick")),
		backBtn("root"),
	)}
}

func groupsView() view {
	text := "👥 Группы MAX\n\n" +
		"Создать группу можно кнопкой ниже. Остальным управляют командами в теме самой группы (только администраторы):\n" +
		"/rename <название> — переименовать\n" +
		"/descr <текст> — описание\n" +
		"/add <номера или id…> — добавить участников\n" +
		"/kick <номера или id…> — убрать участников\n" +
		"/leave — выйти из группы\n" +
		"/delchat — удалить чат (у себя или у всех)\n" +
		"/ban — заглушить чат"
	return view{text, kbd(row(btn("➕ Создать группу", cbPanel+"newgroup")), backBtn("root"))}
}

func (b *Bridge) mutedView() view {
	bans := b.st.Bans()
	if len(bans) == 0 {
		return view{"🔕 Заглушенных чатов нет.\n\nЧтобы заглушить чат, напишите /ban в его теме: сообщения перестанут пересылаться, а тема удалится.",
			kbd(backBtn("root"))}
	}
	keys := make([]string, 0, len(bans))
	for k := range bans {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	rows := make([][]tg.InlineKeyboardButton, 0, len(keys)+1)
	for i, k := range keys {
		if i >= 20 {
			break
		}
		title := bans[k]
		if title == "" {
			title = "Чат " + k
		}
		rows = append(rows, row(btn("🔔 Вернуть: "+truncateRunes(title, 40), cbPanel+"unban:"+k)))
	}
	rows = append(rows, backBtn("root"))
	return view{fmt.Sprintf("🔕 Заглушено чатов: %d. Нажмите, чтобы вернуть чат — тема создастся заново с его следующим сообщением.", len(bans)), kbd(rows...)}
}

func (b *Bridge) systemView() view {
	pauseBtn := btn("⏸ Пауза MAX", cbPanel+"pause")
	if b.max.Paused() {
		pauseBtn = btn("▶️ Возобновить MAX", cbPanel+"resume")
	}
	return view{"⚙️ Система\n" + b.statusLine(), kbd(
		row(btn("📊 Статус", cbPanel+"status")),
		row(pauseBtn, btn("🔄 Пересинхронизация", cbPanel+"resync")),
		backBtn("root"),
	)}
}

func pauseView() view {
	return view{"⏸ На сколько поставить MAX на паузу? Мост отключится от MAX и не будет переподключаться (сообщения в это время не теряются — после паузы мост их подтянет).", kbd(
		row(btn("10 минут", cbPanel+"pause:600"), btn("1 час", cbPanel+"pause:3600")),
		row(btn("1 сутки", cbPanel+"pause:86400"), btn("До перезапуска", cbPanel+"pause:0")),
		backBtn("sys"),
	)}
}

// statusText is the full status report shown by /status and the panel.
func (b *Bridge) statusText() string {
	var sb strings.Builder
	sb.WriteString(b.statusLine() + "\n")
	sb.WriteString(fmt.Sprintf("🔹 Группа TG ID: %d\n", b.bot.GroupID()))
	chats := 0
	for k := range b.st.AllTopics() {
		if !strings.HasPrefix(k, store.PendingPrefix) {
			chats++
		}
	}
	sb.WriteString(fmt.Sprintf("💬 Чатов: %d · заглушено: %d\n", chats, len(b.st.Bans())))
	return sb.String()
}

// ---- entry points ----

// sendPanel posts the panel card into chatID (a private chat or the group's
// General topic). In the group it is also pinned.
func (b *Bridge) sendPanel(chatID int64) {
	v := b.rootView()
	id, err := b.bot.SendChat(chatID, 0, v.text, v.markup)
	if err != nil {
		log.Printf("[panel] send failed: %v", err)
		return
	}
	if chatID == b.bot.GroupID() {
		if err := b.bot.PinMessage(id); err != nil {
			log.Printf("[panel] pin failed (the bot needs the \"pin messages\" right): %v", err)
		}
	}
}

// handlePanelCallback serves every "pn:" button. Panel actions act in the
// bridged MAX account's name, so they are limited to group admins.
func (b *Bridge) handlePanelCallback(cq *tg.CallbackQuery) {
	defer func() {
		if r := recover(); r != nil {
			b.ReportPanic("panel", r)
		}
	}()
	answer := func(text string, alert bool) {
		if err := b.bot.AnswerCallbackQuery(cq.ID, text, alert); err != nil {
			log.Printf("[panel] answerCallbackQuery failed: %v", err)
		}
	}
	if cq.Message == nil {
		answer("", false)
		return
	}
	if !b.isAdmin(cq.From.ID) {
		answer("Панель доступна только администраторам группы", true)
		return
	}
	chatID, msgID := cq.Message.Chat.ID, cq.Message.MessageID
	show := func(v view) {
		if err := b.bot.EditChatMessage(chatID, msgID, v.text, v.markup); err != nil {
			log.Printf("[panel] edit failed: %v", err)
		}
	}
	arg := strings.TrimPrefix(cq.Data, cbPanel)
	action, rest, _ := strings.Cut(arg, ":")

	switch action {
	case "root":
		show(b.rootView())
		answer("", false)
	case "contacts":
		show(contactsView())
		answer("", false)
	case "groups":
		show(groupsView())
		answer("", false)
	case "muted":
		show(b.mutedView())
		answer("", false)
	case "sys":
		show(b.systemView())
		answer("", false)
	case "status":
		show(view{b.statusText(), kbd(row(btn("🔄 Обновить", cbPanel+"status")), backBtn("sys"))})
		answer("", false)
	case "find":
		kinds := map[string]struct{ kind, ask string }{
			"phone": {"find_phone", "🔢 Отправьте номер телефона (например +79991234567)."},
			"id":    {"find_id", "🆔 Отправьте MAX ID (число)."},
			"nick":  {"find_nick", "🔎 Отправьте название или ник бота/канала (без @)."},
		}
		k, ok := kinds[rest]
		if !ok {
			answer("", false)
			return
		}
		b.setPrompt(cq.From.ID, prompt{kind: k.kind, chatID: chatID, panelMsg: msgID})
		show(view{k.ask + "\n\n/cancel — отмена", kbd(backBtn("contacts"))})
		answer("", false)
	case "newgroup":
		b.setPrompt(cq.From.ID, prompt{kind: "new_group", chatID: chatID, panelMsg: msgID})
		show(view{"➕ Новая группа MAX. Отправьте одним сообщением:\n\n" +
			"Название | участник, участник…\n\n" +
			"Участники — по желанию: номер телефона (+79991234567) или id123456789. Например:\n" +
			"Рабочий чат | +79991234567, id123456789\n\n/cancel — отмена", kbd(backBtn("groups"))})
		answer("", false)
	case "chat":
		id, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			answer("", false)
			return
		}
		msg, err := b.startChat(id)
		if err != nil {
			answer("❌ "+shortErr(err), true)
			return
		}
		answer(msg, false)
	case "unban":
		title, was := b.st.Unban(rest)
		if !was {
			answer("Уже не заглушён", false)
		} else {
			if title == "" {
				title = rest
			}
			answer("🔔 «"+truncateRunes(title, 40)+"» снова пересылается", false)
		}
		show(b.mutedView())
	case "pause":
		if rest == "" {
			show(pauseView())
			answer("", false)
			return
		}
		secs, err := strconv.Atoi(rest)
		if err != nil || secs < 0 {
			answer("", false)
			return
		}
		b.pauseMax(time.Duration(secs) * time.Second)
		show(b.systemView())
		answer("⏸ MAX на паузе "+b.pauseLabel(), false)
	case "resume":
		if b.max.Resume() {
			answer("▶️ MAX возобновлён", false)
		} else {
			answer("MAX не на паузе", false)
		}
		show(b.systemView())
	case "resync":
		go b.SyncAllChats()
		answer("🔄 Пересинхронизация запущена", false)
	case "lvy": // leave a group (confirmed)
		b.doLeave(rest, msgID, answer)
	case "dcy": // delete a chat (confirmed); rest = "<chatKey>:<0|1>"
		key, forAll, _ := strings.Cut(rest, ":")
		b.doDeleteChat(key, forAll == "1", msgID, answer)
	case "no":
		_ = b.bot.EditChatMessage(chatID, msgID, "Отменено.", nil)
		answer("", false)
	default:
		answer("", false)
	}
}

// pauseMax disconnects from MAX for d (0 = until restart) and tells the group.
func (b *Bridge) pauseMax(d time.Duration) {
	b.max.Pause(d)
	b.notify("⏸ MAX поставлен на паузу (" + b.pauseLabel() + "). Сообщения из MAX не приходят и в MAX не уходят; после паузы мост подтянет пропущенное.")
}

// normalizePhone turns what a person typed into "+<digits>" (Russian numbers
// written as 8…, 9… or 7… are completed to 7…).
func normalizePhone(s string) (string, bool) {
	plus := strings.HasPrefix(strings.TrimSpace(s), "+")
	var digits strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	d := digits.String()
	if !plus {
		switch {
		case len(d) == 11 && d[0] == '8':
			d = "7" + d[1:]
		case len(d) == 10 && d[0] == '9':
			d = "7" + d
		}
	}
	if len(d) < 10 || len(d) > 15 {
		return "", false
	}
	return "+" + d, true
}
