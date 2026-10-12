package bridge

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"maxtg/internal/maxapi"
	"maxtg/internal/store"
	"maxtg/internal/tg"
)

// MAX polls are mirrored as a native Telegram poll plus a reply showing the
// MAX results (a bot can't inject foreign votes into a Telegram poll). A group
// admin's vote in that poll is cast in MAX as the bridged account; other
// members' votes stay Telegram-only. Polls that don't fit Telegram's limits
// fall back to one text message with an inline vote button per answer. A
// background loop keeps the MAX numbers fresh by editing the results message,
// the same way web.max.ru re-polls results on screen.
const (
	pollRefreshEvery  = 60 * time.Second
	pollRefreshWindow = 7 * 24 * time.Hour // stop refreshing polls older than this
	pollBatch         = 20                 // polls per opcode-306 request

	cbPollVote    = "pv:" // + answerId
	cbPollRefresh = "pr"
)

// pollMissingLogged remembers polls whose refresh reply carried no state, so
// that is logged once per poll instead of every refresh cycle.
var pollMissingLogged sync.Map

// postPoll posts a MAX poll into a topic and records it for votes/refreshes:
// as a native Telegram poll when it fits Telegram's limits, otherwise (or if
// sendPoll fails) as a text message with one vote button per answer.
// Returns the Telegram message id to link with the MAX message (0 on failure).
func (b *Bridge) postPoll(threadID int, header, chatKey, maxMsgID string, p *maxapi.Poll, replyToTg int64) int64 {
	if fitsNativePoll(p) {
		if id := b.postNativePoll(threadID, header, chatKey, maxMsgID, p, replyToTg); id != 0 {
			return id
		}
	}
	return b.postButtonPoll(threadID, header, chatKey, maxMsgID, p, replyToTg)
}

func fitsNativePoll(p *maxapi.Poll) bool {
	if n := len(p.Answers); n < 2 || n > tg.PollMaxOptions {
		return false
	}
	if l := len([]rune(p.Title)); l == 0 || l > tg.PollMaxQuestion {
		return false
	}
	for _, a := range p.Answers {
		if l := len([]rune(a.Text)); l == 0 || l > tg.PollMaxOption {
			return false
		}
	}
	return true
}

// postNativePoll sends a native Telegram poll plus a reply carrying the MAX
// results (a bot can't put foreign votes into a Telegram poll, so those live
// in that separate, periodically edited message).
func (b *Bridge) postNativePoll(threadID int, header, chatKey, maxMsgID string, p *maxapi.Poll, replyToTg int64) int64 {
	opts := make([]string, len(p.Answers))
	for i, a := range p.Answers {
		opts[i] = a.Text
	}
	id, tgPollID, err := b.bot.SendPoll(threadID, p.Title, opts, replyToTg)
	if err != nil {
		log.Printf("[poll] sendPoll for «%s» failed: %v; falling back to a button message", p.Title, err)
		return 0
	}
	rec := store.PollRec{
		MaxChatID: chatKey, MaxMsgID: maxMsgID, TgMsgID: id, TgPollID: tgPollID, Header: header,
		Created: time.Now().Unix(), Poll: *p,
	}
	text, kb := renderPoll(rec)
	rid, err := b.bot.SendToTopicMarkup(threadID, text, id, kb)
	if err != nil {
		log.Printf("[poll] posting MAX results for «%s» failed: %v", p.Title, err)
	}
	rec.ResultsMsgID = rid
	b.st.SavePoll(rec)
	log.Printf("[poll] new poll «%s» chat=%s msg=%s poll=%d: %d answers, %d votes → native tg poll (msg %d, results msg %d)",
		p.Title, chatKey, maxMsgID, p.ID, len(p.Answers), p.Total, id, rid)
	return id
}

// postButtonPoll posts the poll as text with one inline vote button per answer.
func (b *Bridge) postButtonPoll(threadID int, header, chatKey, maxMsgID string, p *maxapi.Poll, replyToTg int64) int64 {
	text, kb := renderButtonPoll(header, p)
	id, err := b.bot.SendToTopicMarkup(threadID, text, replyToTg, kb)
	if err != nil {
		log.Printf("[poll] posting poll «%s» (chat %s, msg %s) to Telegram failed: %v", p.Title, chatKey, maxMsgID, err)
		return 0
	}
	b.st.SavePoll(store.PollRec{
		MaxChatID: chatKey, MaxMsgID: maxMsgID, TgMsgID: id, ResultsMsgID: id, Header: header,
		Created: time.Now().Unix(), Poll: *p,
	})
	log.Printf("[poll] new poll «%s» chat=%s msg=%s poll=%d: %d answers, %d votes → button message (tg msg %d)",
		p.Title, chatKey, maxMsgID, p.ID, len(p.Answers), p.Total, id)
	return id
}

// resultsMsgID is the Telegram message showing a poll's MAX results (records
// saved before native polls existed have no ResultsMsgID).
func resultsMsgID(r store.PollRec) int64 {
	if r.ResultsMsgID != 0 || r.TgPollID != "" {
		return r.ResultsMsgID
	}
	return r.TgMsgID
}

// renderPoll renders the editable message of a poll: the MAX-results reply
// for a native poll, or the full button message otherwise.
func renderPoll(r store.PollRec) (string, *tg.InlineKeyboardMarkup) {
	if r.TgPollID == "" {
		return renderButtonPoll(r.Header, &r.Poll)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "📊 %s — результаты в MAX:\n", r.Header)
	writePollLines(&sb, &r.Poll)
	fmt.Fprintf(&sb, "Всего голосов в MAX: %d\nℹ️ Голос администратора в опросе выше уходит в MAX", r.Poll.Total)
	return sb.String(), &tg.InlineKeyboardMarkup{InlineKeyboard: [][]tg.InlineKeyboardButton{
		{{Text: "🔄 Обновить результаты", CallbackData: cbPollRefresh}},
	}}
}

// writePollLines writes one "mark answer — votes (pct%)" line per answer.
func writePollLines(sb *strings.Builder, p *maxapi.Poll) {
	for _, a := range p.Answers {
		mark := "▫️"
		if a.Mine {
			mark = "✅"
		}
		pct := 0
		if p.Total > 0 {
			pct = a.Votes * 100 / p.Total
		}
		fmt.Fprintf(sb, "%s %s — %d (%d%%)\n", mark, a.Text, a.Votes, pct)
	}
}

// renderButtonPoll builds the text and vote keyboard of a button-style poll.
func renderButtonPoll(header string, p *maxapi.Poll) (string, *tg.InlineKeyboardMarkup) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s:\n📊 Опрос: %s\n", header, p.Title)
	writePollLines(&sb, p)
	kb := &tg.InlineKeyboardMarkup{}
	for _, a := range p.Answers {
		label := truncateRunes(a.Text, 60)
		if a.Mine {
			label = "✅ " + label
		}
		kb.InlineKeyboard = append(kb.InlineKeyboard, []tg.InlineKeyboardButton{
			{Text: label, CallbackData: cbPollVote + strconv.FormatInt(a.ID, 10)},
		})
	}
	fmt.Fprintf(&sb, "Всего голосов: %d\n👇 Кнопки ниже голосуют в MAX от имени аккаунта моста", p.Total)
	kb.InlineKeyboard = append(kb.InlineKeyboard, []tg.InlineKeyboardButton{
		{Text: "🔄 Обновить результаты", CallbackData: cbPollRefresh},
	})
	return sb.String(), kb
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// handleCallback processes an inline-button press on a mirrored poll.
func (b *Bridge) handleCallback(cq *tg.CallbackQuery) {
	answer := func(text string, alert bool) {
		if err := b.bot.AnswerCallbackQuery(cq.ID, text, alert); err != nil {
			log.Printf("[poll] answerCallbackQuery failed: %v", err)
		}
	}
	// Control panel and MAX bot buttons have their own handlers (panel.go,
	// keyboard.go); the panel also works in private chats, so it is routed
	// before the "must be in the bridged group" check below.
	switch {
	case strings.HasPrefix(cq.Data, cbPanel):
		b.handlePanelCallback(cq)
		return
	case strings.HasPrefix(cq.Data, cbKeyboard), cq.Data == cbKbGeo, cq.Data == cbKbContact, cq.Data == cbKbNA:
		b.handleKeyboardCallback(cq)
		return
	}
	if cq.Message == nil || b.bot.GroupID() == 0 || cq.Message.Chat.ID != b.bot.GroupID() {
		answer("", false)
		return
	}
	who := cq.From.FirstName
	if cq.From.Username != "" {
		who += " (@" + cq.From.Username + ")"
	}
	rec, ok := b.st.PollByTg(cq.Message.MessageID)
	if !ok {
		log.Printf("[poll] button %q by %s on tg msg %d: no poll record (too old?)", cq.Data, who, cq.Message.MessageID)
		answer("Опрос не найден — сообщение слишком старое", true)
		return
	}

	switch {
	case cq.Data == cbPollRefresh:
		if err := b.refreshPolls([]store.PollRec{rec}); err != nil {
			answer("❌ Не удалось обновить: "+shortErr(err), true)
			return
		}
		answer("🔄 Результаты обновлены", false)

	case strings.HasPrefix(cq.Data, cbPollVote):
		if !b.isAdmin(cq.From.ID) {
			log.Printf("[poll] vote by %s in «%s» refused: not a group admin", who, rec.Poll.Title)
			answer("Голосовать от имени MAX-аккаунта могут только администраторы группы", true)
			return
		}
		ansID, _ := strconv.ParseInt(strings.TrimPrefix(cq.Data, cbPollVote), 10, 64)
		ansText, err := b.castVote(rec, []int64{ansID}, who)
		if err != nil {
			answer("❌ MAX не принял голос: "+shortErr(err), true)
			return
		}
		answer("✅ Голос «"+truncateRunes(ansText, 40)+"» отправлен в MAX", false)

	default:
		answer("", false)
	}
}

// castVote votes in MAX for the given answers of a mirrored poll and refreshes
// its results message. Returns the chosen answers' text (for feedback).
func (b *Bridge) castVote(rec store.PollRec, ansIDs []int64, who string) (string, error) {
	var texts []string
	for _, id := range ansIDs {
		for _, a := range rec.Poll.Answers {
			if a.ID == id {
				texts = append(texts, a.Text)
			}
		}
	}
	chatID, err := strconv.ParseInt(rec.MaxChatID, 10, 64)
	if err != nil || len(texts) != len(ansIDs) || len(ansIDs) == 0 {
		return "", fmt.Errorf("неизвестный вариант ответа")
	}
	ansText := strings.Join(texts, ", ")
	st, err := b.max.Vote(chatID, rec.MaxMsgID, rec.Poll.ID, ansIDs)
	if err != nil {
		log.Printf("[poll] vote by %s for «%s» in «%s» FAILED: %v", who, ansText, rec.Poll.Title, err)
		return ansText, err
	}
	log.Printf("[poll] vote by %s for «%s» in «%s» → MAX ok", who, ansText, rec.Poll.Title)
	if st != nil {
		b.applyPollState(rec, *st)
	} else if err := b.refreshPolls([]store.PollRec{rec}); err != nil {
		log.Printf("[poll] refresh after vote failed: %v", err)
	}
	return ansText, nil
}

// handlePollAnswer forwards a vote in a native Telegram poll to MAX — only
// for group admins, since it is cast as the bridged MAX account. Other
// members' votes stay Telegram-only.
func (b *Bridge) handlePollAnswer(pa *tg.PollAnswer) {
	if pa.User == nil || pa.User.IsBot {
		return
	}
	rec, ok := b.st.PollByTgPollID(pa.PollID)
	if !ok {
		return // not one of ours (or too old)
	}
	who := pa.User.FirstName
	if pa.User.Username != "" {
		who += " (@" + pa.User.Username + ")"
	}
	if len(pa.OptionIDs) == 0 {
		log.Printf("[poll] %s retracted their Telegram vote in «%s» — the MAX vote is left as is (retraction isn't bridged)", who, rec.Poll.Title)
		return
	}
	if !b.isAdmin(pa.User.ID) {
		log.Printf("[poll] Telegram-only vote by %s in «%s» (not a group admin, not sent to MAX)", who, rec.Poll.Title)
		return
	}
	var ansIDs []int64
	for _, i := range pa.OptionIDs {
		if i >= 0 && i < len(rec.Poll.Answers) {
			ansIDs = append(ansIDs, rec.Poll.Answers[i].ID)
		}
	}
	if _, err := b.castVote(rec, ansIDs, who); err != nil {
		// No toast exists for poll votes — tell the topic instead.
		if link, ok := b.st.TgByMax(rec.MaxChatID, rec.MaxMsgID); ok {
			_, _ = b.bot.SendToTopic(link.ThreadID, "❌ Голос в MAX не прошёл: "+shortErr(err), resultsMsgID(rec))
		}
	}
}

func shortErr(err error) string {
	return truncateRunes(err.Error(), 150)
}

// RunPollRefresher periodically re-fetches results of recent polls and edits
// their Telegram messages when the numbers change. Blocks forever.
func (b *Bridge) RunPollRefresher() {
	for {
		time.Sleep(pollRefreshEvery)
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[poll] refresher panic: %v", r)
				}
			}()
			if b.bot.GroupID() == 0 || !b.max.Binary() {
				return // over JSON MAX hides polls entirely; nothing to refresh
			}
			if connected, _, _ := b.max.Status(); !connected {
				return
			}
			recs := b.st.PollsSince(time.Now().Add(-pollRefreshWindow).Unix())
			if err := b.refreshPolls(recs); err != nil {
				log.Printf("[poll] periodic refresh: %v", err)
			}
		}()
	}
}

// refreshPolls fetches fresh results for recs (grouped per chat) and applies
// them. Returns the last error encountered, if any.
func (b *Bridge) refreshPolls(recs []store.PollRec) error {
	byChat := map[string][]store.PollRec{}
	for _, r := range recs {
		byChat[r.MaxChatID] = append(byChat[r.MaxChatID], r)
	}
	var lastErr error
	for chatKey, list := range byChat {
		chatID, err := strconv.ParseInt(chatKey, 10, 64)
		if err != nil {
			continue
		}
		for start := 0; start < len(list); start += pollBatch {
			batch := list[start:min(start+pollBatch, len(list))]
			refs := make([]maxapi.PollRef, len(batch))
			for i, r := range batch {
				refs[i] = maxapi.PollRef{MessageID: r.MaxMsgID, PollID: r.Poll.ID}
			}
			states, err := b.max.PollStates(chatID, refs)
			if err != nil {
				lastErr = fmt.Errorf("chat %s: %w", chatKey, err)
				continue
			}
			for _, r := range batch {
				st, ok := states[r.Poll.ID]
				if !ok {
					if _, logged := pollMissingLogged.LoadOrStore(r.Poll.ID, true); !logged {
						log.Printf("[poll] refresh reply has no state for poll %d «%s» (chat %s) — see the opcode 306 sample in the log", r.Poll.ID, r.Poll.Title, chatKey)
					}
					continue
				}
				b.applyPollState(r, st)
			}
		}
	}
	return lastErr
}

// applyPollState updates a mirrored poll with fresh results, editing its
// Telegram message only when what it shows actually changed.
func (b *Bridge) applyPollState(rec store.PollRec, st maxapi.PollState) {
	before, _ := renderPoll(rec)
	oldTotal := rec.Poll.Total
	rec.Poll.Apply(st)
	after, kb := renderPoll(rec)
	if after == before {
		return
	}
	msgID := resultsMsgID(rec)
	if msgID == 0 {
		return // the results message was never posted
	}
	if err := b.bot.EditMessageText(msgID, after, kb); err != nil {
		log.Printf("[poll] editing tg msg %d for «%s» failed: %v", msgID, rec.Poll.Title, err)
		return
	}
	b.st.SavePoll(rec)
	log.Printf("[poll] results updated «%s» (chat %s): votes %d → %d; %s",
		rec.Poll.Title, rec.MaxChatID, oldTotal, rec.Poll.Total, maxapi.FormatPollState(st))
}
