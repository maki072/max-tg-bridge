package tg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

// SendPhoto uploads a photo (JPEG/PNG bytes) into a topic thread. Returns the
// created Telegram message id (0 on failure) so the caller can link it to the
// MAX message it mirrors, for later reactions/replies/deletes.
func (b *Bot) SendPhoto(threadID int, photo []byte, caption string, replyToTgMsgID int64) (int64, error) {
	return b.sendMedia("sendPhoto", "photo", "photo.jpg", photo, threadID, caption, replyToTgMsgID)
}

// SendDocument uploads any file into a topic thread.
func (b *Bot) SendDocument(threadID int, doc []byte, filename, caption string, replyToTgMsgID int64) (int64, error) {
	if filename == "" {
		filename = "file"
	}
	return b.sendMedia("sendDocument", "document", filename, doc, threadID, caption, replyToTgMsgID)
}

// SendVideo uploads a video (Telegram plays it inline when the container/codec
// is supported, otherwise the caller falls back to SendDocument).
func (b *Bot) SendVideo(threadID int, video []byte, filename, caption string, replyToTgMsgID int64) (int64, error) {
	if filename == "" {
		filename = "video.mp4"
	}
	return b.sendMedia("sendVideo", "video", filename, video, threadID, caption, replyToTgMsgID)
}

// SendVoice uploads a voice note. Telegram requires OGG/OPUS here; for other
// audio codecs the caller falls back to SendAudio/SendDocument.
func (b *Bot) SendVoice(threadID int, voice []byte, caption string, replyToTgMsgID int64) (int64, error) {
	return b.sendMedia("sendVoice", "voice", "voice.ogg", voice, threadID, caption, replyToTgMsgID)
}

// SendAudio uploads an audio file (shown in Telegram's music player).
func (b *Bot) SendAudio(threadID int, audio []byte, filename, caption string, replyToTgMsgID int64) (int64, error) {
	if filename == "" {
		filename = "audio.mp3"
	}
	return b.sendMedia("sendAudio", "audio", filename, audio, threadID, caption, replyToTgMsgID)
}

// SendSticker uploads a sticker file (.webp, .tgs or .webm — Telegram rejects
// other formats). Stickers carry no caption.
func (b *Bot) SendSticker(threadID int, data []byte, filename string, replyToTgMsgID int64) (int64, error) {
	return b.sendMedia("sendSticker", "sticker", filename, data, threadID, "", replyToTgMsgID)
}

// sentMessage extracts just the id from a Telegram send-method result.
type sentMessage struct {
	MessageID int64 `json:"message_id"`
}

// sendMedia posts a multipart file upload to the group/topic, throttled and
// retried the same way as call() (see doRequest) — a burst of photos/files
// (e.g. topic history backfill) hitting Telegram's per-chat rate limit used to
// make every single send fail with 429 and fall straight to the sendDocument
// fallback, which then also 429'd. Retrying in place fixes that instead of
// doubling the failed calls. replyToTgMsgID, if non-zero, makes this a native
// Telegram reply to that message (0 = no reply).
func (b *Bot) sendMedia(method, field, filename string, data []byte, threadID int, caption string, replyToTgMsgID int64) (int64, error) {
	body, err := b.doRequest(func() (*http.Request, error) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		_ = w.WriteField("chat_id", strconv.FormatInt(b.GroupID(), 10))
		if threadID != 0 {
			_ = w.WriteField("message_thread_id", strconv.Itoa(threadID))
		}
		if caption != "" {
			_ = w.WriteField("caption", caption)
		}
		if replyToTgMsgID != 0 {
			_ = w.WriteField("reply_to_message_id", strconv.FormatInt(replyToTgMsgID, 10))
		}
		fw, err := w.CreateFormFile(field, filename)
		if err != nil {
			return nil, err
		}
		if _, err := fw.Write(data); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		req, err := http.NewRequest(http.MethodPost, b.base+"/"+method, &buf)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", w.FormDataContentType())
		return req, nil
	})
	if err != nil {
		return 0, err
	}
	var ar apiResp
	if err := json.Unmarshal(body, &ar); err != nil {
		return 0, fmt.Errorf("%s: bad response: %s", method, string(body))
	}
	if !ar.OK {
		return 0, fmt.Errorf("%s: %s", method, ar.Description)
	}
	var sm sentMessage
	_ = json.Unmarshal(ar.Result, &sm)
	return sm.MessageID, nil
}

// Bot is a minimal Telegram Bot API client built on net/http.
type Bot struct {
	token string
	base  string
	http  *http.Client

	mu      sync.RWMutex
	groupID int64

	// rateMu/nextSend throttle every outgoing call (sendMessage/sendPhoto/...)
	// to a shared minimum interval — Telegram allows roughly one message per
	// second per chat, and this bridge fans out from several goroutines
	// (incoming MAX messages, reactions, command replies) into the SAME group,
	// so without a shared gate a burst (e.g. backfilling a new topic's history)
	// blows straight through the limit and every send in the burst 429s.
	rateMu   sync.Mutex
	nextSend time.Time
}

// tgMinInterval keeps sends to the group comfortably under Telegram's ~1
// message/second per-chat limit even with several goroutines sending at once.
const tgMinInterval = 1100 * time.Millisecond

// tgMaxRetries is how many additional attempts doRequest makes after a 429
// before giving up and returning the error to the caller.
const tgMaxRetries = 5

// throttle blocks until the shared minimum interval since the last send has
// elapsed, then reserves the next slot. Call this immediately before every
// outgoing HTTP request to Telegram.
func (b *Bot) throttle() {
	b.rateMu.Lock()
	defer b.rateMu.Unlock()
	if wait := time.Until(b.nextSend); wait > 0 {
		time.Sleep(wait)
	}
	b.nextSend = time.Now().Add(tgMinInterval)
}

// retryParams mirrors Telegram's 429 "parameters" object.
type retryParams struct {
	RetryAfter int `json:"retry_after"`
}

// retryAfterWait picks how long to sleep before retrying a 429, preferring
// the structured "parameters.retry_after" field and falling back to parsing
// it out of the description text ("... retry after 34") if that's absent.
func retryAfterWait(p *retryParams, description string) time.Duration {
	secs := 0
	if p != nil && p.RetryAfter > 0 {
		secs = p.RetryAfter
	} else if i := strings.Index(description, "retry after "); i >= 0 {
		fmt.Sscanf(description[i+len("retry after "):], "%d", &secs)
	}
	if secs <= 0 {
		secs = 5
	}
	// Telegram's countdown is to-the-second; pad slightly so we don't re-knock
	// right on the boundary and get told to wait again.
	return time.Duration(secs)*time.Second + 500*time.Millisecond
}

// doRequest throttles, builds (via newReq, called fresh on every attempt since
// a request body can only be read once), sends, and — on a 429 — sleeps for
// the server-given retry_after and tries again, up to tgMaxRetries times.
// Returns the raw response body on any non-429 response (including other
// Telegram-level errors, which the caller decodes from the body itself).
func (b *Bot) doRequest(newReq func() (*http.Request, error)) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= tgMaxRetries; attempt++ {
		b.throttle()
		req, err := newReq()
		if err != nil {
			return nil, err
		}
		resp, err := b.http.Do(req)
		if err != nil {
			return nil, err
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var probe struct {
			OK          bool         `json:"ok"`
			ErrorCode   int          `json:"error_code"`
			Description string       `json:"description"`
			Parameters  *retryParams `json:"parameters"`
		}
		if json.Unmarshal(data, &probe) == nil && !probe.OK && probe.ErrorCode == 429 && attempt < tgMaxRetries {
			wait := retryAfterWait(probe.Parameters, probe.Description)
			log.Printf("[tg] rate-limited (429), waiting %v before retry %d/%d", wait, attempt+1, tgMaxRetries)
			lastErr = fmt.Errorf("telegram error 429: %s", probe.Description)
			time.Sleep(wait)
			continue
		}
		return data, nil
	}
	return nil, lastErr
}

// New creates a Bot for a given supergroup. proxyURL, if non-empty, routes all
// Telegram traffic through a SOCKS5 proxy (MAX is unaffected вЂ” it has its own
// direct connection). groupID may be 0 (auto-detected later).
func New(token string, groupID int64, proxyURL string) (*Bot, error) {
	client, err := newHTTPClient(proxyURL)
	if err != nil {
		return nil, err
	}
	return &Bot{
		token:   token,
		base:    "https://api.telegram.org/bot" + token,
		http:    client,
		groupID: groupID,
	}, nil
}

// newHTTPClient builds an http.Client, optionally over a SOCKS5 proxy.
func newHTTPClient(proxyURL string) (*http.Client, error) {
	if proxyURL == "" {
		return &http.Client{Timeout: 65 * time.Second}, nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("bad telegram_proxy %q: %w", proxyURL, err)
	}
	if u.Scheme != "socks5" && u.Scheme != "socks5h" {
		return nil, fmt.Errorf("telegram_proxy must be socks5://..., got %q", u.Scheme)
	}
	var auth *proxy.Auth
	if u.User != nil {
		pass, _ := u.User.Password()
		auth = &proxy.Auth{User: u.User.Username(), Password: pass}
	}
	d, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("socks5 dialer: %w", err)
	}
	transport := &http.Transport{
		DialContext: func(_ context.Context, network, addr string) (net.Conn, error) {
			return d.Dial(network, addr)
		},
	}
	return &http.Client{Timeout: 65 * time.Second, Transport: transport}, nil
}

// GroupID returns the current target group id (0 if not set yet).
func (b *Bot) GroupID() int64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.groupID
}

// SetGroupID updates the target group id (used by auto-detection).
func (b *Bot) SetGroupID(id int64) {
	b.mu.Lock()
	b.groupID = id
	b.mu.Unlock()
}

// apiResp is the common Telegram envelope.
type apiResp struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
}

// call performs a POST with a JSON body and decodes result into out.
// Throttled and auto-retried on 429 via doRequest.
func (b *Bot) call(method string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	data, err := b.doRequest(func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, b.base+"/"+method, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	})
	if err != nil {
		return err
	}

	var ar apiResp
	if err := json.Unmarshal(data, &ar); err != nil {
		return fmt.Errorf("%s: bad response: %s", method, string(data))
	}
	if !ar.OK {
		return fmt.Errorf("%s: telegram error %d: %s", method, ar.ErrorCode, ar.Description)
	}
	if out != nil {
		return json.Unmarshal(ar.Result, out)
	}
	return nil
}

// GetMe calls getMe to verify the token and connectivity (incl. proxy).
// Returns the bot username on success.
func (b *Bot) GetMe() (string, error) {
	var res struct {
		Username  string `json:"username"`
		FirstName string `json:"first_name"`
	}
	if err := b.call("getMe", map[string]any{}, &res); err != nil {
		return "", err
	}
	if res.Username != "" {
		return "@" + res.Username, nil
	}
	return res.FirstName, nil
}

// CreateForumTopic creates a new topic in the group and returns its thread id.
func (b *Bot) CreateForumTopic(name string) (int, error) {
	var res struct {
		MessageThreadID int `json:"message_thread_id"`
	}
	err := b.call("createForumTopic", map[string]any{
		"chat_id": b.GroupID(),
		"name":    truncateName(name),
	}, &res)
	if err != nil {
		return 0, err
	}
	return res.MessageThreadID, nil
}

// SendMessage sends a text message to the group with NO topic thread вЂ” for a
// forum-enabled supergroup this lands in the default "General" topic. Used
// for service/diagnostic messages so they don't spawn a new topic each time.
func (b *Bot) SendMessage(text string) error {
	return b.call("sendMessage", map[string]any{
		"chat_id": b.GroupID(),
		"text":    text,
	}, nil)
}

// SendToTopic sends a text message into a topic thread. replyToTgMsgID, if
// non-zero, makes this a native Telegram reply to that message. Returns the
// created message id (0 on failure) for reaction/reply/delete linking.
func (b *Bot) SendToTopic(threadID int, text string, replyToTgMsgID int64) (int64, error) {
	return b.SendToTopicMarkup(threadID, text, replyToTgMsgID, nil)
}

// InlineKeyboardButton is a button under a message; pressing it sends a
// callback_query carrying CallbackData (max 64 bytes) back to the bot.
type InlineKeyboardButton struct {
	Text         string        `json:"text"`
	CallbackData string        `json:"callback_data,omitempty"`
	URL          string        `json:"url,omitempty"`
	CopyText     *CopyTextInfo `json:"copy_text,omitempty"` // Bot API 7.11: copies Text to the clipboard
}

// CopyTextInfo is the payload of a "copy to clipboard" inline button.
type CopyTextInfo struct {
	Text string `json:"text"`
}

type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

// SendToTopicMarkup is SendToTopic with an optional inline keyboard.
func (b *Bot) SendToTopicMarkup(threadID int, text string, replyToTgMsgID int64, markup *InlineKeyboardMarkup) (int64, error) {
	body := map[string]any{
		"chat_id":           b.GroupID(),
		"message_thread_id": threadID,
		"text":              text,
	}
	if replyToTgMsgID != 0 {
		body["reply_to_message_id"] = replyToTgMsgID
	}
	if markup != nil {
		body["reply_markup"] = markup
	}
	var sm sentMessage
	if err := b.call("sendMessage", body, &sm); err != nil {
		return 0, err
	}
	return sm.MessageID, nil
}

// Telegram's native poll limits (Bot API): question length, option count and
// option length. Polls outside them can't be sent with sendPoll.
const (
	PollMaxQuestion = 300
	PollMaxOptions  = 12
	PollMaxOption   = 100
)

// SendPoll posts a native, non-anonymous Telegram poll into a topic (so the
// bot receives poll_answer updates with the voter's id). Returns the message
// id and the Telegram poll id.
func (b *Bot) SendPoll(threadID int, question string, options []string, replyToTgMsgID int64) (int64, string, error) {
	opts := make([]map[string]string, len(options))
	for i, o := range options {
		opts[i] = map[string]string{"text": o}
	}
	body := map[string]any{
		"chat_id":           b.GroupID(),
		"message_thread_id": threadID,
		"question":          question,
		"options":           opts,
		"is_anonymous":      false,
	}
	if replyToTgMsgID != 0 {
		body["reply_to_message_id"] = replyToTgMsgID
	}
	var res struct {
		MessageID int64 `json:"message_id"`
		Poll      struct {
			ID string `json:"id"`
		} `json:"poll"`
	}
	if err := b.call("sendPoll", body, &res); err != nil {
		return 0, "", err
	}
	return res.MessageID, res.Poll.ID, nil
}

// EditMessageText replaces the text (and inline keyboard) of a message in the
// group. An edit that changes nothing is not treated as an error.
func (b *Bot) EditMessageText(tgMsgID int64, text string, markup *InlineKeyboardMarkup) error {
	return b.EditChatMessage(b.GroupID(), tgMsgID, text, markup)
}

// EditChatMessage is EditMessageText for any chat (the control panel also
// lives in private chats with admins).
func (b *Bot) EditChatMessage(chatID, tgMsgID int64, text string, markup *InlineKeyboardMarkup) error {
	body := map[string]any{"chat_id": chatID, "message_id": tgMsgID, "text": text}
	if markup != nil {
		body["reply_markup"] = markup
	}
	err := b.call("editMessageText", body, nil)
	if err != nil && strings.Contains(err.Error(), "message is not modified") {
		return nil
	}
	return err
}

// SendChat sends text (with an optional inline keyboard) to any chat; threadID
// 0 means no topic (a private chat, or the group's General topic).
func (b *Bot) SendChat(chatID int64, threadID int, text string, markup *InlineKeyboardMarkup) (int64, error) {
	body := map[string]any{"chat_id": chatID, "text": text}
	if threadID != 0 {
		body["message_thread_id"] = threadID
	}
	if markup != nil {
		body["reply_markup"] = markup
	}
	var sm sentMessage
	if err := b.call("sendMessage", body, &sm); err != nil {
		return 0, err
	}
	return sm.MessageID, nil
}

// PinMessage pins a message in the bridged group, silently.
func (b *Bot) PinMessage(tgMsgID int64) error {
	return b.call("pinChatMessage", map[string]any{
		"chat_id": b.GroupID(), "message_id": tgMsgID, "disable_notification": true,
	}, nil)
}

// SendLocation posts a map point into a topic.
func (b *Bot) SendLocation(threadID int, lat, lon float64, replyToTgMsgID int64) (int64, error) {
	body := map[string]any{
		"chat_id": b.GroupID(), "message_thread_id": threadID,
		"latitude": lat, "longitude": lon,
	}
	if replyToTgMsgID != 0 {
		body["reply_to_message_id"] = replyToTgMsgID
	}
	var sm sentMessage
	if err := b.call("sendLocation", body, &sm); err != nil {
		return 0, err
	}
	return sm.MessageID, nil
}

// SendContact posts a phone contact card into a topic.
func (b *Bot) SendContact(threadID int, phone, firstName, lastName string, replyToTgMsgID int64) (int64, error) {
	body := map[string]any{
		"chat_id": b.GroupID(), "message_thread_id": threadID,
		"phone_number": phone, "first_name": firstName,
	}
	if lastName != "" {
		body["last_name"] = lastName
	}
	if replyToTgMsgID != 0 {
		body["reply_to_message_id"] = replyToTgMsgID
	}
	var sm sentMessage
	if err := b.call("sendContact", body, &sm); err != nil {
		return 0, err
	}
	return sm.MessageID, nil
}

// DeleteForumTopic deletes a topic together with all its messages.
func (b *Bot) DeleteForumTopic(threadID int) error {
	return b.call("deleteForumTopic", map[string]any{
		"chat_id": b.GroupID(), "message_thread_id": threadID,
	}, nil)
}

// RenameForumTopic changes a topic's title.
func (b *Bot) RenameForumTopic(threadID int, name string) error {
	return b.call("editForumTopic", map[string]any{
		"chat_id": b.GroupID(), "message_thread_id": threadID, "name": truncateName(name),
	}, nil)
}

// AnswerCallbackQuery acknowledges an inline-button press, optionally showing
// text to the user (as a toast, or a modal alert when alert is true).
func (b *Bot) AnswerCallbackQuery(id, text string, alert bool) error {
	return b.call("answerCallbackQuery", map[string]any{
		"callback_query_id": id, "text": text, "show_alert": alert,
	}, nil)
}

// DeleteMessage deletes a message in the bridged group (used to mirror a MAX
// message deletion into Telegram).
func (b *Bot) DeleteMessage(tgMsgID int64) error {
	return b.call("deleteMessage", map[string]any{
		"chat_id": b.GroupID(), "message_id": tgMsgID,
	}, nil)
}

// Message is the subset of a Telegram message this bridge consumes. It is
// self-referential (ReplyToMessage) so it doubles as the shape of both
// Update.Message and Update.EditedMessage.
type Message struct {
	MessageID       int64  `json:"message_id"`
	MessageThreadID int    `json:"message_thread_id"`
	Text            string `json:"text"`
	Caption         string `json:"caption"`
	Photo           []struct {
		FileID   string `json:"file_id"`
		FileSize int    `json:"file_size"`
	} `json:"photo"`
	Document *struct {
		FileID   string `json:"file_id"`
		FileName string `json:"file_name"`
	} `json:"document"`
	Video *struct {
		FileID   string `json:"file_id"`
		FileName string `json:"file_name"`
		Duration int    `json:"duration"`
	} `json:"video"`
	Voice *struct {
		FileID   string `json:"file_id"`
		Duration int    `json:"duration"`
	} `json:"voice"`
	Audio *struct {
		FileID   string `json:"file_id"`
		FileName string `json:"file_name"`
		Duration int    `json:"duration"`
	} `json:"audio"`
	Sticker *struct {
		FileID     string `json:"file_id"`
		Emoji      string `json:"emoji"`
		IsAnimated bool   `json:"is_animated"` // TGS (gzipped Lottie JSON)
		IsVideo    bool   `json:"is_video"`    // WEBM
	} `json:"sticker"`
	Location *struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
	} `json:"location"`
	Contact *struct {
		PhoneNumber string `json:"phone_number"`
		FirstName   string `json:"first_name"`
		LastName    string `json:"last_name"`
	} `json:"contact"`
	// ReplyToMessage is set when this message replies to another one in the
	// same chat — used to attach a MAX REPLY link (via the store's tg<->max
	// message map) when forwarding Telegram -> MAX.
	ReplyToMessage *Message `json:"reply_to_message"`
	From           struct {
		ID    int64 `json:"id"`
		IsBot bool  `json:"is_bot"`
	} `json:"from"`
	Chat struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
		Type  string `json:"type"`
	} `json:"chat"`
}

// ReactionType is one entry of a message_reaction update's old/new reaction
// list (only the "emoji" variant is used by this bridge — custom emoji
// reactions have no MAX equivalent and are ignored).
type ReactionType struct {
	Type  string `json:"type"` // "emoji" | "custom_emoji"
	Emoji string `json:"emoji"`
}

// MessageReactionUpdated is Telegram's message_reaction update: a user added,
// changed, or removed their reaction on a message we track.
type MessageReactionUpdated struct {
	Chat struct {
		ID int64 `json:"id"`
	} `json:"chat"`
	MessageID int64 `json:"message_id"`
	User      *struct {
		ID    int64 `json:"id"`
		IsBot bool  `json:"is_bot"`
	} `json:"user"`
	OldReaction []ReactionType `json:"old_reaction"`
	NewReaction []ReactionType `json:"new_reaction"`
}

// CallbackQuery is an inline-keyboard button press.
type CallbackQuery struct {
	ID   string `json:"id"`
	From struct {
		ID        int64  `json:"id"`
		IsBot     bool   `json:"is_bot"`
		FirstName string `json:"first_name"`
		Username  string `json:"username"`
	} `json:"from"`
	Message *Message `json:"message"` // the message the button is attached to
	Data    string   `json:"data"`
}

// PollAnswer is a vote (or retraction, when OptionIDs is empty) in a
// non-anonymous poll the bot sent.
type PollAnswer struct {
	PollID string `json:"poll_id"`
	User   *struct {
		ID        int64  `json:"id"`
		IsBot     bool   `json:"is_bot"`
		FirstName string `json:"first_name"`
		Username  string `json:"username"`
	} `json:"user"`
	OptionIDs []int `json:"option_ids"` // 0-based indexes into the poll's options
}

// Update is the subset of a Telegram update this bridge consumes.
type Update struct {
	UpdateID        int64                   `json:"update_id"`
	Message         *Message                `json:"message"`
	EditedMessage   *Message                `json:"edited_message"`
	MessageReaction *MessageReactionUpdated `json:"message_reaction"`
	CallbackQuery   *CallbackQuery          `json:"callback_query"`
	PollAnswer      *PollAnswer             `json:"poll_answer"`
}

// DownloadFile fetches a Telegram file by file_id (getFile + download through the
// same proxied client). Returns the bytes and a best-effort filename.
func (b *Bot) DownloadFile(fileID string) ([]byte, string, error) {
	var res struct {
		FilePath string `json:"file_path"`
	}
	if err := b.call("getFile", map[string]any{"file_id": fileID}, &res); err != nil {
		return nil, "", err
	}
	resp, err := b.http.Get("https://api.telegram.org/file/bot" + b.token + "/" + res.FilePath)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	name := res.FilePath
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	return data, name, err
}

// GetUpdates long-polls for updates starting at offset.
func (b *Bot) GetUpdates(offset int64, timeoutSec int) ([]Update, error) {
	q := url.Values{}
	q.Set("offset", strconv.FormatInt(offset, 10))
	q.Set("timeout", strconv.Itoa(timeoutSec))
	q.Set("allowed_updates", `["message","edited_message","message_reaction","callback_query","poll_answer"]`)

	resp, err := b.http.Get(b.base + "/getUpdates?" + q.Encode())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	var ar struct {
		OK          bool     `json:"ok"`
		Result      []Update `json:"result"`
		Description string   `json:"description"`
	}
	if err := json.Unmarshal(data, &ar); err != nil {
		return nil, fmt.Errorf("getUpdates: bad response: %s", string(data))
	}
	if !ar.OK {
		return nil, fmt.Errorf("getUpdates: %s", ar.Description)
	}
	return ar.Result, nil
}

// truncateName keeps topic names within Telegram's 128-char limit.
func truncateName(s string) string {
	r := []rune(s)
	if len(r) > 128 {
		return string(r[:128])
	}
	return s
}

type KeyboardButton struct {
	Text   string      `json:"text"`
	WebApp *WebAppInfo `json:"web_app,omitempty"` // opens a Mini App (private chats only)
}

// WebAppInfo is the address of a Telegram Mini App.
type WebAppInfo struct {
	URL string `json:"url"`
}

// SetMenuButtonWebApp makes the bot's default menu button (next to the input
// field in every private chat) open the Mini App at url.
func (b *Bot) SetMenuButtonWebApp(text, url string) error {
	return b.call("setChatMenuButton", map[string]any{
		"menu_button": map[string]any{"type": "web_app", "text": text, "web_app": WebAppInfo{URL: url}},
	}, nil)
}

type ReplyKeyboardMarkup struct {
	Keyboard        [][]KeyboardButton `json:"keyboard"`
	ResizeKeyboard  bool               `json:"resize_keyboard"`
	OneTimeKeyboard bool               `json:"one_time_keyboard"`
}

func (b *Bot) SendMenu(chatID int64, text string, menu ReplyKeyboardMarkup) error {
	return b.call("sendMessage", map[string]any{
		"chat_id":      chatID,
		"text":         text,
		"reply_markup": menu,
	}, nil)
}

func (b *Bot) GetChatAdministrators(chatID int64) ([]int64, error) {
	var results []struct {
		User struct {
			ID int64 `json:"id"`
		} `json:"user"`
	}
	if err := b.call("getChatAdministrators", map[string]any{"chat_id": chatID}, &results); err != nil {
		return nil, err
	}
	ids := make([]int64, len(results))
	for i, u := range results {
		ids[i] = u.User.ID
	}
	return ids, nil
}

// DeleteChatMessage deletes a message in any chat. In a private chat the bot
// may delete the user's own messages (used to wipe SMS codes and passwords).
func (b *Bot) DeleteChatMessage(chatID, tgMsgID int64) error {
	return b.call("deleteMessage", map[string]any{"chat_id": chatID, "message_id": tgMsgID}, nil)
}
