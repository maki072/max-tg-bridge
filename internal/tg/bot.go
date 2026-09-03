package tg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// sendMedia posts a multipart file upload to the group/topic.
func (b *Bot) sendMedia(method, field, filename string, data []byte, threadID int, caption string) error {
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
		return err
	}
	if _, err := fw.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	resp, err := b.http.Post(b.base+"/"+method, w.FormDataContentType(), &buf)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
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
func (b *Bot) call(method string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := b.http.Post(b.base+"/"+method, "application/json", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

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
