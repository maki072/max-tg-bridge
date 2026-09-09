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
		max:        max,
		bot:        bot,
		st:         st,
		cfg:        cfg,
		configPath: configPath,
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
		b.onNewTopic(threadID, m.ChatID) // avatar + history backfill
	}

	b.postMessage(threadID, m)
}

// postMessage renders one MAX message into a topic (header + text + media),
// then records the tg<->max message link so later reactions/replies/deletes
// (in either direction) can find their way back to this message.
func (b *Bridge) postMessage(threadID int, m maxapi.IncomingMessage) {
	prefix := ""
	if m.Outgoing {
		prefix = "➡️ "
	}
	if m.Forwarded {
		prefix += "↪ "
	} else if m.ReplyToID != "" {
		prefix += "↩️ "
	}
	header := prefix + m.SenderName
	chatKey := strconv.FormatInt(m.ChatID, 10)

	var replyToTg int64
	if m.ReplyToID != "" {
		if link, ok := b.st.TgByMax(chatKey, m.ReplyToID); ok {
			replyToTg = link.TgMsgID
		}
	}

	linked := false
	link := func(tgID int64) {
		if !linked && tgID != 0 {
			b.st.LinkMessages(chatKey, m.MessageID, tgID, threadID)
			linked = true
		}
	}

	if strings.TrimSpace(m.Text) != "" {
		tgID, err := b.bot.SendToTopic(threadID, header+":\n"+m.Text, replyToTg)
		if err != nil {
			log.Printf("[bridge] sendToTopic %d failed: %v", threadID, err)
		}
		link(tgID)
	}
	for _, att := range m.Attachments {
		link(b.forwardAttachment(threadID, header, att, replyToTg))
	}
}

// onNewTopic posts the chat avatar and backfills recent history into a new topic.
func (b *Bridge) onNewTopic(threadID int, chatID int64) {
	if url := b.max.ChatIcon(chatID); url != "" {
		b.forwardAttachment(threadID, "🖼", maxapi.Attachment{Kind: "photo", URL: url}, 0)
	}
	for _, hm := range b.max.History(chatID, b.cfg.HistoryCount) {
		if hm.MessageID != "" && b.st.SeenAndMark("max:"+hm.MessageID) {
			continue
		}
		b.postMessage(threadID, hm)
	}
}

// forwardAttachment downloads a MAX media attachment and posts it into the
// topic, optionally as a native Telegram reply (replyToTg == 0 means none).
// Returns the id of the Telegram message it posted (0 if none/failed), for
// tg<->max message linking.
func (b *Bridge) forwardAttachment(threadID int, header string, att maxapi.Attachment, replyToTg int64) int64 {
	caption := header + ":"
	switch att.Kind {
	case "photo":
		data, _, err := httpDownload(att.URL, 25*1024*1024)
		if err != nil {
			log.Printf("[bridge] photo download failed: %v", err)
			id, _ := b.bot.SendToTopic(threadID, caption+" 🖼 "+att.URL, replyToTg)
			return id
		}
		// MAX photos are WebP; Telegram sendPhoto needs JPEG/PNG.
		if jpg, err := webpToJPEG(data); err == nil {
			data = jpg
		}
		id, err := b.bot.SendPhoto(threadID, data, caption, replyToTg)
		if err != nil {
			log.Printf("[bridge] sendPhoto failed: %v; sending as document", err)
			id, _ = b.bot.SendDocument(threadID, data, "photo.jpg", caption, replyToTg)
		}
		return id
	case "file":
		data, _, err := httpDownload(att.URL, 45*1024*1024)
		if err != nil {
			log.Printf("[bridge] file download failed: %v", err)
			id, _ := b.bot.SendToTopic(threadID, caption+" 📎 "+att.URL, replyToTg)
			return id
		}
		name := att.Name
		if name == "" {
			name = "file"
		}
		id, err := b.bot.SendDocument(threadID, data, name, caption, replyToTg)
		if err != nil {
			log.Printf("[bridge] sendDocument failed: %v", err)
		}
		return id
	case "video":
		// Videos carry a large signed CDN URL. Download it and re-upload so it
		// plays inside Telegram; fall back to a document, then to posting the
		// link (video missing / too big for the bot upload limit / fetch error).
		data, _, tooLarge, err := httpDownloadCapped(att.URL, tgUploadLimit)
		if att.URL == "" || tooLarge || err != nil || len(data) == 0 {
			if err != nil {
				log.Printf("[bridge] video download failed: %v; posting link", err)
			} else if tooLarge {
				log.Printf("[bridge] video exceeds %d bytes; posting link", tgUploadLimit)
			}
			id, _ := b.bot.SendToTopic(threadID, caption+" 🎥 "+att.URL, replyToTg)
			return id
		}
		name := att.Name
		if name == "" {
			name = "video.mp4"
		}
		id, err := b.bot.SendVideo(threadID, data, name, caption, replyToTg)
		if err != nil {
			log.Printf("[bridge] sendVideo failed: %v; sending as document", err)
			id, err = b.bot.SendDocument(threadID, data, name, caption, replyToTg)
			if err != nil {
				log.Printf("[bridge] sendDocument (video) failed: %v; posting link", err)
				id, _ = b.bot.SendToTopic(threadID, caption+" 🎥 "+att.URL, replyToTg)
			}
		}
		return id
	case "voice":
		// Voice notes previously had no fetchable URL in most cases (MAX gave
		// only an internal audioId/token); Client.resolveAttachments now tries
		// AUDIO_PLAY first, so att.URL is populated whenever that succeeds.
		// Fall back to the "open MAX" placeholder only if it's still empty.
		if att.URL == "" {
			return b.postVoicePlaceholder(threadID, caption, att.Duration, replyToTg)
		}
		data, _, tooLarge, err := httpDownloadCapped(att.URL, tgUploadLimit)
		if tooLarge || err != nil || len(data) == 0 {
			if err != nil {
				log.Printf("[bridge] voice download failed: %v; posting placeholder", err)
			}
			return b.postVoicePlaceholder(threadID, caption, att.Duration, replyToTg)
		}
		name := att.Name
		if name == "" {
			name = "audio"
		}
		id, err := b.bot.SendVoice(threadID, data, caption, replyToTg)
		if err != nil {
			id, err = b.bot.SendAudio(threadID, data, name, caption, replyToTg)
			if err != nil {
				id, err = b.bot.SendDocument(threadID, data, name, caption, replyToTg)
				if err != nil {
					log.Printf("[bridge] voice/audio send failed: %v; posting placeholder", err)
					return b.postVoicePlaceholder(threadID, caption, att.Duration, replyToTg)
				}
			}
		}
		return id
	case "poll":
		text := caption + " 📊 Голосование"
		if strings.TrimSpace(att.Text) != "" {
			text += ": " + att.Text
		}
		text += " — откройте MAX, чтобы посмотреть варианты и проголосовать"
		id, _ := b.bot.SendToTopic(threadID, text, replyToTg)
		return id
	case "other":
		// MAX substitutes a bare "UNSUPPORTED" attach (no other fields at
		// all) for several content types it won't describe to a WEB session
		// — confirmed live for both voice notes and polls, so it's likely a
		// blanket "can't render this for you" marker rather than one
		// specific type. Naming it plainly ("UNSUPPORTED") reads like an
		// internal error to a user; say what's actually going on instead.
		if att.Name == "UNSUPPORTED" {
			text := caption + " 📎 Вложение недоступно для этой сессии MAX (голосовое, видео‑кружок, опрос или что‑то ещё — сервер не сообщает, что именно) — откройте MAX, чтобы посмотреть"
			id, _ := b.bot.SendToTopic(threadID, text, replyToTg)
			return id
		}
		label := att.Name
		if label == "" {
			label = "вложение"
		}
		id, _ := b.bot.SendToTopic(threadID, fmt.Sprintf("%s 📎 Неподдерживаемое вложение (%s) — откройте MAX, чтобы посмотреть", caption, label), replyToTg)
		return id
	}
	return 0
}

// postVoicePlaceholder posts the "open MAX to listen" notice used whenever a
// voice note can't be re-uploaded (no URL, too large, or a fetch/send failure).
func (b *Bridge) postVoicePlaceholder(threadID int, caption string, duration int64, replyToTg int64) int64 {
	id, _ := b.bot.SendToTopic(threadID, fmt.Sprintf("%s 🎤 Голосовое сообщение (%s) — откройте MAX, чтобы прослушать", caption, formatDuration(duration)), replyToTg)
	return id
}

// formatDuration renders a millisecond duration as m:ss for voice notes.
func formatDuration(ms int64) string {
	total := ms / 1000
	return fmt.Sprintf("%d:%02d", total/60, total%60)
}

// OnMaxReaction forwards a NOTIF_REACTION event into the mapped topic, if one
// already exists for that chat (reactions never create a topic by themselves).
func (b *Bridge) OnMaxReaction(r maxapi.IncomingReaction) {
	if b.bot.GroupID() == 0 {
		return
	}
	chatKey := strconv.FormatInt(r.ChatID, 10)
	threadID, ok := b.st.Topic(chatKey)
	if !ok {
		return
	}
	if len(r.Counters) == 0 {
		_, _ = b.bot.SendToTopic(threadID, "🔔 Реакции на сообщение сняты", 0)
		return
	}
	parts := make([]string, 0, len(r.Counters))
	for _, c := range r.Counters {
		parts = append(parts, fmt.Sprintf("%s x%d", c.Reaction, c.Count))
	}
	_, _ = b.bot.SendToTopic(threadID, "🔔 Реакция: "+strings.Join(parts, ", "), 0)
}

// OnMaxDelete mirrors a MAX message deletion by deleting the Telegram message
// that mirrors it, if one is known (only messages this bridge itself
// forwarded — in either direction — are tracked, so unrelated chat history
// is never touched).
func (b *Bridge) OnMaxDelete(d maxapi.IncomingDelete) {
	if b.bot.GroupID() == 0 {
		return
	}
	chatKey := strconv.FormatInt(d.ChatID, 10)
	for _, id := range d.MessageIDs {
		link, ok := b.st.TgByMax(chatKey, id)
		if !ok {
			continue
		}
		if err := b.bot.DeleteMessage(link.TgMsgID); err != nil {
			log.Printf("[bridge] delete mirrored tg message %d (max msg %s) failed: %v", link.TgMsgID, id, err)
		}
	}
}

// handleReaction forwards a Telegram message_reaction update to MAX: setting
// (or, if all reactions were removed, clearing) our reaction on the MAX
// message the reacted-to Telegram message mirrors.
func (b *Bridge) handleReaction(r *tg.MessageReactionUpdated) {
	if b.bot.GroupID() == 0 || r.Chat.ID != b.bot.GroupID() || r.User == nil || r.User.IsBot {
		return
	}
	link, ok := b.st.MaxByTg(r.MessageID)
	if !ok {
		return
	}
	chatID, err := strconv.ParseInt(link.MaxChatID, 10, 64)
	if err != nil {
		return
	}
	if len(r.NewReaction) == 0 {
		if err := b.max.RemoveReaction(chatID, link.MaxMsgID); err != nil {
			log.Printf("[bridge] MAX remove reaction failed: %v", err)
		}
		return
	}
	emoji := r.NewReaction[0].Emoji
	if emoji == "" {
		return // custom_emoji reaction — no MAX equivalent
	}
	if err := b.max.AddReaction(chatID, link.MaxMsgID, emoji); err != nil {
		log.Printf("[bridge] MAX add reaction failed: %v", err)
	}
}

// handleEdit forwards a Telegram edited_message update to MAX (MSG_EDIT), for
// a message this bridge itself previously sent to MAX from that same
// Telegram message. Edits made directly in MAX are not currently mirrored
// back — no confirmed push opcode for that exists.
func (b *Bridge) handleEdit(m *tg.Message) {
	if b.bot.GroupID() == 0 || m.Chat.ID != b.bot.GroupID() || m.MessageThreadID == 0 {
		return
	}
	link, ok := b.st.MaxByTg(m.MessageID)
	if !ok {
		return
	}
	chatID, err := strconv.ParseInt(link.MaxChatID, 10, 64)
	if err != nil {
		return
	}
	if err := b.max.EditMessage(chatID, link.MaxMsgID, m.Text); err != nil {
		log.Printf("[bridge] MAX edit message failed: %v", err)
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
	if u.MessageReaction != nil {
		b.handleReaction(u.MessageReaction)
		return
	}
	if u.EditedMessage != nil {
		b.handleEdit(u.EditedMessage)
		return
	}
	msg := u.Message
	if msg == nil || msg.From.IsBot {
		return
	}
	hasMedia := len(msg.Photo) > 0 || msg.Document != nil || msg.Video != nil || msg.Voice != nil || msg.Audio != nil || msg.Sticker != nil

	// 1. Приватные сообщения боту (Личка) — только для Администраторов группы!
	if msg.Chat.Type == "private" {
		if !b.isAdmin(msg.From.ID) {
			_ = b.bot.SendMenu(msg.Chat.ID, "У вас нет прав администратора в привязанной группе MAX-TG.", tg.ReplyKeyboardMarkup{ResizeKeyboard: true})
			return
		}
		if msg.Text != "" {
			b.handleAdminPM(msg.Chat.ID, msg.Text)
		}
		return
	}

	// 2. Действия внутри привязанной супергруппы
	if b.bot.GroupID() == 0 || msg.Chat.ID != b.bot.GroupID() {
		return
	}
	// General topic — только базовые команды.
	if msg.MessageThreadID == 0 {
		if strings.HasPrefix(msg.Text, "/") {
			if reply := b.processCommand(msg.Text); reply != "" {
				b.notify(reply)
			}
		}
		return
	}

	// A named topic: forward the reply (text or media) to the mapped MAX chat.
	if msg.Text == "" && !hasMedia {
		return
	}
	chatKey, ok := b.st.ChatByTopic(msg.MessageThreadID)
	if !ok {
		return
	}
	chatID, err := strconv.ParseInt(chatKey, 10, 64)
	if err != nil {
		return
	}

	// "/del" (or "/delete") as a reply to a mirrored message deletes the
	// corresponding MAX message — Telegram's Bot API has no "message was
	// deleted" update, so this is the only way to drive MSG_DELETE from here.
	cmdText := strings.ToLower(strings.TrimSpace(msg.Text))
	if (cmdText == "/del" || cmdText == "/delete") && msg.ReplyToMessage != nil {
		if link, ok := b.st.MaxByTg(msg.ReplyToMessage.MessageID); ok {
			if err := b.max.DeleteMessage(chatID, link.MaxMsgID, false); err != nil {
				log.Printf("[bridge] MAX delete message %s failed: %v", link.MaxMsgID, err)
			}
		}
		return
	}

	// If this Telegram message replies to one we can map back to a MAX
	// message (one MAX sent that we mirrored here, or one an admin sent from
	// Telegram earlier), carry that over as a MAX REPLY link.
	var replyToMax string
	if msg.ReplyToMessage != nil {
		if link, ok := b.st.MaxByTg(msg.ReplyToMessage.MessageID); ok {
			replyToMax = link.MaxMsgID
		}
	}

	// NOTE: video/voice are deliberately sent via the plain FILE_UPLOAD path
	// (SendFileReply), NOT the native VIDEO_UPLOAD attach type. Live testing
	// showed MAX reject a native voice (AUDIO) attach with "Invalid
	// attachment" AND — reproduced twice — kill the whole websocket session
	// (1006) right after, with the very next request often timing out
	// post-reconnect too (looks like a brief server-side cooldown/anti-abuse
	// reaction to the malformed frame). Given this repo's own risk-of-ban
	// disclaimer, that's not a trade worth making for a cosmetic "shows as a
	// real video/voice" gain, so both use FILE_UPLOAD, the long-proven-safe
	// path (video shares VIDEO_UPLOAD's upload mechanics with voice, so it's
	// kept on the safe path too even though only voice was confirmed bad).
	// Photo is the exception — see its case below: PHOTO_UPLOAD is a
	// completely different, simpler flow and was confirmed working live.
	var maxMsgID string
	switch {
	case msg.Voice != nil:
		data, _, dlErr := b.bot.DownloadFile(msg.Voice.FileID)
		if dlErr != nil {
			log.Printf("[bridge] TG voice download failed: %v", dlErr)
			return
		}
		maxMsgID, err = b.max.SendFileReply(chatID, "voice.ogg", data, msg.Caption, replyToMax)
		if err != nil {
			log.Printf("[bridge] MAX SendFile (voice) to chat %d failed: %v", chatID, err)
		}
	case msg.Video != nil:
		data, dlName, dlErr := b.bot.DownloadFile(msg.Video.FileID)
		if dlErr != nil {
			log.Printf("[bridge] TG video download failed: %v", dlErr)
			return
		}
		name := msg.Video.FileName
		if name == "" {
			name = dlName
		}
		maxMsgID, err = b.max.SendFileReply(chatID, name, data, msg.Caption, replyToMax)
		if err != nil {
			log.Printf("[bridge] MAX SendFile (video) to chat %d failed: %v", chatID, err)
		}
	case len(msg.Photo) > 0:
		// Unlike voice/video, PHOTO_UPLOAD is a simple synchronous flow (the
		// token comes straight back in the HTTP upload response, no
		// NOTIF_ATTACH wait) and has been confirmed live to work without
		// issue, so photos DO use the native attach — falls back to a plain
		// file only if MAX still rejects it for some reason.
		data, _, dlErr := b.bot.DownloadFile(msg.Photo[len(msg.Photo)-1].FileID) // largest size
		if dlErr != nil {
			log.Printf("[bridge] TG photo download failed: %v", dlErr)
			return
		}
		maxMsgID, err = b.max.SendPhoto(chatID, data, msg.Caption, replyToMax)
		if err != nil {
			log.Printf("[bridge] MAX SendPhoto to chat %d failed: %v; sending as file instead", chatID, err)
			maxMsgID, err = b.max.SendFileReply(chatID, "photo.jpg", data, msg.Caption, replyToMax)
			if err != nil {
				log.Printf("[bridge] MAX SendFile (photo fallback) to chat %d failed: %v", chatID, err)
			}
		}
	case msg.Sticker != nil:
		// MAX has no way to accept an arbitrary Telegram sticker as a native
		// MAX sticker (that requires owning/registering a sticker pack via
		// STICKER_SYNC/STICKER_DATA — out of scope), so it goes through as a
		// plain file: .webp for static, .webm for video stickers, .tgs
		// (gzipped Lottie JSON — not directly viewable, but at least not lost)
		// for animated ones.
		data, _, dlErr := b.bot.DownloadFile(msg.Sticker.FileID)
		if dlErr != nil {
			log.Printf("[bridge] TG sticker download failed: %v", dlErr)
			return
		}
		ext := "webp"
		if msg.Sticker.IsVideo {
			ext = "webm"
		} else if msg.Sticker.IsAnimated {
			ext = "tgs"
		}
		name := "sticker." + ext
		if msg.Sticker.Emoji != "" {
			name = msg.Sticker.Emoji + " " + name
		}
		maxMsgID, err = b.max.SendFileReply(chatID, name, data, msg.Caption, replyToMax)
		if err != nil {
			log.Printf("[bridge] MAX SendFile (sticker) to chat %d failed: %v", chatID, err)
		}
	case msg.Document != nil || msg.Audio != nil:
		var fileID, name string
		if msg.Document != nil {
			fileID, name = msg.Document.FileID, msg.Document.FileName
		} else {
			fileID, name = msg.Audio.FileID, msg.Audio.FileName
		}
		data, dlName, dlErr := b.bot.DownloadFile(fileID)
		if dlErr != nil {
			log.Printf("[bridge] TG file download failed: %v", dlErr)
			return
		}
		if name == "" {
			name = dlName
		}
		maxMsgID, err = b.max.SendFileReply(chatID, name, data, msg.Caption, replyToMax)
		if err != nil {
			log.Printf("[bridge] MAX SendFile to chat %d failed: %v", chatID, err)
		}
	default:
		maxMsgID, err = b.max.SendMessageReply(chatID, msg.Text, replyToMax)
		if err != nil {
			log.Printf("[bridge] MAX send to chat %d failed: %v", chatID, err)
		}
	}
	if maxMsgID != "" {
		b.st.SeenAndMark("max:" + maxMsgID) // skip our own echo
		b.st.LinkMessages(chatKey, maxMsgID, msg.MessageID, msg.MessageThreadID)
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
		if n > 200 {
			n = 200
		}
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
		n := 10
		if len(logs) < n {
			n = len(logs) // fewer than 10 frames so far (e.g. right after a fresh
			// reconnect) — avoid a negative slice bound.
		}
		return "--- Последние фреймы от MAX ---\n\n" + strings.Join(logs[len(logs)-n:], "\n\n")

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
