package bridge

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"unicode"

	"maxtg/internal/store"
	"maxtg/internal/tg"
)

// Chat and group management from Telegram: opening a chat with a new contact,
// creating a group, and the per-topic admin commands (/rename, /descr, /add,
// /kick, /leave, /delchat, /ban). The feature set mirrors Telemax's panel
// (github.com/Trollobot/Telemax); this implementation is ours.

// startChat prepares a topic for a MAX user we have no dialog with yet. The
// first message written in it opens the dialog (see openDialog) — MAX creates
// a 1:1 chat only when the first message is sent.
func (b *Bridge) startChat(userID int64) (string, error) {
	key := store.PendingPrefix + strconv.FormatInt(userID, 10)
	if _, ok := b.st.Topic(key); ok {
		return "Тема для этого контакта уже создана — напишите в ней.", nil
	}
	name := fmt.Sprintf("Пользователь %d", userID)
	phone := ""
	if ct, err := b.max.ContactByID(userID); err == nil && ct != nil {
		if ct.Name != "" {
			name = ct.Name
		}
		phone = ct.Phone
	}
	b.dialogMu.Lock()
	defer b.dialogMu.Unlock()
	thread, err := b.bot.CreateForumTopic(name)
	if err != nil {
		return "", fmt.Errorf("не удалось создать тему: %w", err)
	}
	b.st.SetTopic(key, thread)
	b.st.SetTitle(key, name)
	intro := "💬 Чат с " + name
	if phone != "" {
		intro += " (+" + phone + ")"
	}
	intro += "\n\nЧата в MAX ещё нет. Напишите здесь первое сообщение — оно откроет чат."
	_, _ = b.bot.SendToTopic(thread, intro, 0)
	return "💬 Тема «" + truncateRunes(name, 40) + "» создана — напишите в ней первое сообщение", nil
}

// openDialog sends the first message written in a "pending" topic, which makes
// MAX create the dialog, then re-keys the topic to the real chat id.
func (b *Bridge) openDialog(msg *tg.Message, pendingKey string) {
	thread := msg.MessageThreadID
	userID, err := strconv.ParseInt(strings.TrimPrefix(pendingKey, store.PendingPrefix), 10, 64)
	if err != nil {
		return
	}
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		_, _ = b.bot.SendToTopic(thread, "Чтобы открыть чат, сначала отправьте текстовое сообщение.", msg.MessageID)
		return
	}
	// Held across the request and the re-keying so that the echo of our own
	// message can't create a second topic for the brand-new chat meanwhile.
	b.dialogMu.Lock()
	defer b.dialogMu.Unlock()
	if _, still := b.st.Topic(pendingKey); !still {
		return // handled by a concurrent message
	}
	chatID, maxMsgID, err := b.max.SendToNewDialog(userID, text)
	if err != nil {
		b.reportSendFailure(thread, msg.MessageID, "открыть чат", err)
		return
	}
	chatKey := strconv.FormatInt(chatID, 10)
	if existing, ok := b.st.Topic(chatKey); ok {
		// The dialog already existed and has its own topic: the message went
		// there (its echo will show up as "➡️ Вы"), so this placeholder is spare.
		b.st.RemoveChat(pendingKey)
		_ = b.bot.DeleteForumTopic(thread)
		b.notify(fmt.Sprintf("ℹ️ Чат с этим контактом уже существует (тема %d) — сообщение отправлено туда.", existing))
		return
	}
	b.st.SeenAndMark("max:" + maxMsgID) // skip our own echo
	b.st.MoveTopic(pendingKey, chatKey)
	b.st.LinkMessages(chatKey, maxMsgID, msg.MessageID, thread)
	log.Printf("[bridge] opened MAX dialog %s with user %d (topic %d)", chatKey, userID, thread)
}

// createGroupFromPrompt handles the "Name | member, member" answer of the
// new-group prompt.
func (b *Bridge) createGroupFromPrompt(p prompt, text string) {
	title, members, _ := strings.Cut(text, "|")
	title = strings.TrimSpace(title)
	retry := kbd(row(btn("➕ Ещё раз", cbPanel+"newgroup")), backBtn("groups"))
	if title == "" {
		b.showPanelView(p, view{"❌ Нужно название группы.", retry})
		return
	}
	ids, problems := b.resolveUsers(splitRefs(members))
	if len(problems) > 0 {
		b.showPanelView(p, view{"❌ Группа не создана:\n" + strings.Join(problems, "\n"), retry})
		return
	}

	b.dialogMu.Lock()
	defer b.dialogMu.Unlock()
	chatID, err := b.max.CreateGroup(title, ids)
	if err != nil {
		b.showPanelView(p, view{"❌ MAX не создал группу: " + shortErr(err), retry})
		return
	}
	chatKey := strconv.FormatInt(chatID, 10)
	if _, ok := b.st.Topic(chatKey); !ok {
		thread, err := b.bot.CreateForumTopic(title)
		if err != nil {
			b.showPanelView(p, view{fmt.Sprintf("✅ Группа «%s» создана в MAX (id %d), но тему в Telegram создать не удалось: %s\nТема появится с первым сообщением группы.", title, chatID, shortErr(err)), kbd(backBtn("groups"))})
			return
		}
		b.st.SetTopic(chatKey, thread)
		b.st.SetTitle(chatKey, title)
		_, _ = b.bot.SendToTopic(thread, "👥 Группа «"+title+"» создана. Управление — командами /rename /descr /add /kick /leave /delchat.", 0)
	}
	b.showPanelView(p, view{fmt.Sprintf("✅ Группа «%s» создана, участников приглашено: %d. Тема уже в Telegram.", title, len(ids)), kbd(backBtn("groups"))})
}

// splitRefs splits a list of user references on commas, semicolons, spaces and
// newlines.
func splitRefs(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || unicode.IsSpace(r) })
}

// resolveUsers turns phone numbers ("+79991234567", "89991234567") and ids
// ("id123456789", or a plain number under 10 digits) into MAX user ids.
// problems lists the references that could not be resolved.
func (b *Bridge) resolveUsers(refs []string) (ids []int64, problems []string) {
	seen := map[int64]bool{}
	add := func(id int64) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, ref := range refs {
		low := strings.ToLower(ref)
		if rest := strings.TrimPrefix(strings.TrimPrefix(low, "id"), "#"); rest != low {
			if id, err := strconv.ParseInt(rest, 10, 64); err == nil && id > 0 {
				add(id)
			} else {
				problems = append(problems, "• «"+ref+"» — не похоже на id")
			}
			continue
		}
		digits := 0
		for _, r := range ref {
			if r >= '0' && r <= '9' {
				digits++
			}
		}
		if !strings.HasPrefix(ref, "+") && digits > 0 && digits < 10 && digits == len(ref) {
			id, _ := strconv.ParseInt(ref, 10, 64)
			add(id)
			continue
		}
		phone, ok := normalizePhone(ref)
		if !ok {
			problems = append(problems, "• «"+ref+"» — не понял, это номер или id?")
			continue
		}
		ct, err := b.max.FindByPhone(phone)
		switch {
		case err != nil:
			problems = append(problems, "• "+phone+" — поиск не удался: "+shortErr(err))
		case ct == nil:
			problems = append(problems, "• "+phone+" — нет в MAX (или скрыт поиск по номеру)")
		default:
			add(ct.ID)
		}
	}
	return ids, problems
}

// topicCommands are the admin commands that act on the MAX chat behind a topic.
var topicCommands = map[string]bool{
	"/rename": true, "/descr": true, "/add": true, "/kick": true,
	"/leave": true, "/delchat": true, "/ban": true,
}

// handleTopicCommand runs a management command written in a chat's topic and
// reports whether msg was one (so it must not be forwarded to MAX as text).
func (b *Bridge) handleTopicCommand(msg *tg.Message, chatKey string) bool {
	fields := strings.Fields(msg.Text)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return false
	}
	cmd := strings.ToLower(fields[0])
	if i := strings.Index(cmd, "@"); i >= 0 {
		cmd = cmd[:i]
	}
	if !topicCommands[cmd] {
		return false
	}
	thread := msg.MessageThreadID
	reply := func(t string) { _, _ = b.bot.SendToTopic(thread, t, msg.MessageID) }
	if !b.isAdmin(msg.From.ID) {
		reply("Эта команда — только для администраторов группы.")
		return true
	}
	args := strings.TrimSpace(strings.TrimPrefix(msg.Text, fields[0]))
	title := b.st.Title(chatKey)
	if title == "" {
		title = "чат " + chatKey
	}

	if strings.HasPrefix(chatKey, store.PendingPrefix) {
		if cmd == "/ban" { // on a not-yet-opened chat: just drop the placeholder topic
			b.dropChat(chatKey)
			return true
		}
		reply("Чат в MAX ещё не открыт — сначала напишите здесь первое сообщение.")
		return true
	}
	chatID, err := strconv.ParseInt(chatKey, 10, 64)
	if err != nil {
		return true
	}

	switch cmd {
	case "/rename":
		if args == "" {
			reply("Использование: /rename <новое название>")
			return true
		}
		if err := b.max.SetChatInfo(chatID, &args, nil); err != nil {
			reply("❌ MAX не принял название: " + shortErr(err) + "\n(переименовать можно только группу, где вы администратор)")
			return true
		}
		b.st.SetTitle(chatKey, args)
		_ = b.bot.RenameForumTopic(thread, args)
		reply("✅ Переименовано.")
	case "/descr":
		if err := b.max.SetChatInfo(chatID, nil, &args); err != nil {
			reply("❌ MAX не принял описание: " + shortErr(err))
			return true
		}
		reply("✅ Описание обновлено.")
	case "/add", "/kick":
		refs := splitRefs(args)
		if len(refs) == 0 {
			reply("Использование: " + cmd + " +79991234567 id123456789 …")
			return true
		}
		ids, problems := b.resolveUsers(refs)
		var out []string
		if len(ids) > 0 {
			var err error
			verb := "Добавлено"
			if cmd == "/add" {
				err = b.max.AddMembers(chatID, ids)
			} else {
				verb = "Убрано"
				err = b.max.RemoveMembers(chatID, ids)
			}
			if err != nil {
				out = append(out, "❌ MAX отказал: "+shortErr(err))
			} else {
				out = append(out, fmt.Sprintf("✅ %s участников: %d", verb, len(ids)))
			}
		}
		out = append(out, problems...)
		reply(strings.Join(out, "\n"))
	case "/leave":
		_, _ = b.bot.SendChat(b.bot.GroupID(), thread, "Выйти из «"+title+"» в MAX?", kbd(row(
			btn("✅ Выйти", cbPanel+"lvy:"+chatKey), btn("Отмена", cbPanel+"no"))))
	case "/delchat":
		_, _ = b.bot.SendChat(b.bot.GroupID(), thread, "Удалить чат «"+title+"»?\n«У меня» — только из вашего аккаунта; «У всех» — у каждого участника, это необратимо.", kbd(
			row(btn("🗑 У меня", cbPanel+"dcy:"+chatKey+":0"), btn("🗑 У всех", cbPanel+"dcy:"+chatKey+":1")),
			row(btn("Отмена", cbPanel+"no"))))
	case "/ban":
		b.banChat(chatKey, b.st.Title(chatKey))
	}
	return true
}

// dropChat forgets a MAX chat locally and deletes its Telegram topic.
func (b *Bridge) dropChat(chatKey string) {
	thread := b.st.RemoveChat(chatKey)
	if thread != 0 {
		if err := b.bot.DeleteForumTopic(thread); err != nil {
			log.Printf("[bridge] delete topic %d failed: %v", thread, err)
		}
	}
}

// banChat mutes a MAX chat: its messages stop being mirrored and its topic is
// deleted. Reversible from the panel.
func (b *Bridge) banChat(chatKey, title string) {
	b.st.Ban(chatKey, title)
	b.dropChat(chatKey)
	if title == "" {
		title = "чат " + chatKey
	}
	b.notify("🔕 «" + title + "» заглушён: сообщения из этого чата больше не пересылаются. Вернуть — /panel → «Заглушенные чаты».")
}

// onChatClosed mirrors a chat deleted in MAX: its topic goes away too.
func (b *Bridge) onChatClosed(chatKey string) {
	if _, ok := b.st.Topic(chatKey); !ok {
		return // never mirrored, or already removed by us (/leave, /delchat, /ban)
	}
	title := b.st.Title(chatKey)
	b.dropChat(chatKey)
	if title == "" {
		title = "чат " + chatKey
	}
	log.Printf("[bridge] MAX chat %s deleted — removed its Telegram topic", chatKey)
	b.notify("🗑 «" + title + "» удалён в MAX — тема в Telegram тоже удалена.")
}

func (b *Bridge) doLeave(chatKey string, confirmMsg int64, answer func(string, bool)) {
	chatID, err := strconv.ParseInt(chatKey, 10, 64)
	if err != nil {
		answer("", false)
		return
	}
	title := b.st.Title(chatKey)
	if err := b.max.LeaveChat(chatID); err != nil {
		answer("❌ MAX: "+shortErr(err), true)
		return
	}
	answer("Вы вышли из группы", false)
	b.dropChat(chatKey)
	if title == "" {
		title = "чат " + chatKey
	}
	b.notify("🚪 Вы вышли из «" + title + "» — тема удалена.")
}

func (b *Bridge) doDeleteChat(chatKey string, forAll bool, confirmMsg int64, answer func(string, bool)) {
	chatID, err := strconv.ParseInt(chatKey, 10, 64)
	if err != nil {
		answer("", false)
		return
	}
	title := b.st.Title(chatKey)
	if err := b.max.DeleteChat(chatID, forAll); err != nil {
		answer("❌ MAX: "+shortErr(err), true)
		return
	}
	answer("Чат удалён", false)
	b.dropChat(chatKey)
	if title == "" {
		title = "чат " + chatKey
	}
	scope := "у вас"
	if forAll {
		scope = "у всех"
	}
	b.notify("🗑 «" + title + "» удалён " + scope + " — тема удалена.")
}
