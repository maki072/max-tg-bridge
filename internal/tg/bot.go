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

// SendPhoto uploads a photo (JPEG/PNG bytes) into a topic thread.
func (b *Bot) SendPhoto(threadID int, photo []byte, caption string) error {
	return b.sendMedia("sendPhoto", "photo", "photo.jpg", photo, threadID, caption)
}

// SendDocument uploads any file into a topic thread.
func (b *Bot) SendDocument(threadID int, doc []byte, filename, caption string) error {
	if filename == "" {
		filename = "file"
	}
	return b.sendMedia("sendDocument", "document", filename, doc, threadID, caption)
}

// sendMedia posts a multipart file upload to the group/topic, throttled and
// retried the same way as call() (see doRequest) — a burst of photos/files
// (e.g. topic history backfill) hitting Telegram's per-chat rate limit used to
// make every single send fail with 429 and fall straight to the sendDocument
// fallback, which then also 429'd. Retrying in place fixes that instead of
// doubling the failed calls.
func (b *Bot) sendMedia(method, field, filename string, data []byte, threadID int, caption string) error {
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
		return err
	}
	var ar apiResp
	if json.Unmarshal(body, &ar) == nil && !ar.OK {
		return fmt.Errorf("%s: %s", method, ar.Description)
	}
	return nil
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

// SendToTopic sends a text message into a topic thread.
func (b *Bot) SendToTopic(threadID int, text string) error {
	return b.call("sendMessage", map[string]any{
		"chat_id":           b.GroupID(),
		"message_thread_id": threadID,
		"text":              text,
	}, nil)
}

// Update is the subset of a Telegram update this bridge consumes.
type Update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
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
		From struct {
			ID    int64 `json:"id"`
			IsBot bool  `json:"is_bot"`
		} `json:"from"`
		Chat struct {
			ID    int64  `json:"id"`
			Title string `json:"title"`
			Type  string `json:"type"`
		} `json:"chat"`
	} `json:"message"`
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
	q.Set("allowed_updates", `["message"]`)

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
	Text string `json:"text"`
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
