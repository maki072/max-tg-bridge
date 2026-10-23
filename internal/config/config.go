package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"maxtg/internal/paths"
)

// Config holds all runtime settings. Values are loaded from a JSON file and
// may be overridden by environment variables (env wins).
type Config struct {
	// MAX
	MaxWSURL string `json:"max_ws_url"` // wss://ws-api.oneme.ru/websocket
	Phone    string `json:"phone"`      // +7XXXXXXXXXX, used for SMS auth

	// Telegram
	TelegramBotToken string `json:"telegram_bot_token"`
	TelegramGroupID  int64  `json:"telegram_group_id"` // supergroup with topics enabled (0 = auto-detect)

	// TelegramProxy is an optional SOCKS5 proxy for Telegram traffic ONLY,
	// e.g. "socks5://user:pass@host:1080" or "socks5://host:1080".
	// MAX always connects directly (never through this proxy).
	TelegramProxy string `json:"telegram_proxy"`

	// BrowserCDPURL, if set, enables automatic token refresh: the service reads
	// the current deviceId+token from a browser logged in to web.max.ru via the
	// Chrome DevTools Protocol (e.g. "http://127.0.0.1:9222"). Empty = disabled.
	BrowserCDPURL string `json:"browser_cdp_url"`
	// BrowserMatchURL is the page URL substring to look for (default web.max.ru).
	BrowserMatchURL string `json:"browser_match_url"`

	// WebAppListen is where the Mini App's HTTP server listens (a reverse proxy
	// with HTTPS sits in front of it); "-" turns the Mini App off. WebAppURL is
	// the public https address Telegram opens; empty = no menu button.
	WebAppListen string `json:"webapp_listen,omitempty"`
	WebAppURL    string `json:"webapp_url,omitempty"`

	// State file (token, deviceId, chat<->topic map, tg offset)
	StatePath string `json:"state_path"`

	// IncomingOpcode, when > 0, forces treating this push opcode as a new
	// message. 0 = rely on the structural heuristic only (recommended until
	// the real opcode is confirmed during Step 2 discovery).
	IncomingOpcode int `json:"incoming_opcode"`

	// HistoryCount: how many recent messages to backfill into a topic when it is
	// first created (0 = off). Kept modest by default — Telegram rate-limits and
	// a large dump floods the topic.
	HistoryCount int `json:"history_count"`

	// SyncCount: how many of each known chat's most recent messages to re-check
	// every time the MAX session (re)connects — this is what catches up on
	// messages sent while the service (or the machine) was off, since live WS
	// pushes only ever cover messages sent while actually connected. Already
	// delivered messages are skipped (dedup via the Seen set), so this can be
	// generous; 0 = off (no catch-up resync on reconnect).
	SyncCount int `json:"sync_count"`

	// MaxAppVersion pins the appVersion sent to MAX in INIT (e.g. the version shown
	// in the desktop app's "About"). Empty = automatic: when MAX answers "app is
	// outdated" the bridge tries newer versions and keeps the one that works in
	// MaxAppVersionAuto.
	MaxAppVersion     string `json:"max_app_version"`
	MaxAppVersionAuto string `json:"max_app_version_auto,omitempty"`

	// MaxBinary (default on) selects the binary msgpack transport the
	// web client uses; false = legacy JSON transport, where MAX hides polls behind a
	// bare UNSUPPORTED attach (over binary they arrive in full).
	MaxBinary *bool `json:"max_binary,omitempty"` // nil (absent) = on

	// MaxTransport picks how the bridge talks to MAX: "tcp" = the desktop
	// client's TCP endpoint (browserless sign-in; no browser, no cookies), "ws" =
	// the web client's WebSocket (token imported from a browser), ""/"auto" =
	// follow the stored token (a token minted by the bridge's own SMS sign-in →
	// tcp, anything else → ws).
	MaxTransport string `json:"max_transport,omitempty"`

	// Dinner: сайт обедов (dinner.016.help). Мост сам опрашивает сайт и
	// отправляет готовые сообщения в группу MAX (внешний IP не нужен).
	// Выключено, пока не заданы все три поля.
	DinnerURL     string `json:"dinner_url"`      // https://dinner.016.help
	DinnerToken   string `json:"dinner_token"`    // токен из админки сайта
	DinnerChatID  int64  `json:"dinner_chat_id"`  // id группы MAX (ключи chat_topic в state.json)
	DinnerPollSec int    `json:"dinner_poll_sec"` // период опроса, по умолчанию 30
}

// Default values.
const (
	defaultWSURL     = "wss://ws-api.oneme.ru/websocket"
	defaultStatePath = "state.json"
)

// load reads config.json (if present) and applies env overrides + defaults.
func load(path string) (*Config, error) {
	cfg := &Config{
		MaxWSURL:  defaultWSURL,
		StatePath: defaultStatePath,
	}

	if path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			// Tolerate a UTF-8 BOM (Notepad / PowerShell Set-Content add one).
			data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
			if err := json.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
	}

	applyEnv(cfg)

	if cfg.MaxWSURL == "" {
		cfg.MaxWSURL = defaultWSURL
	}
	// Resolve the state file to the shared data dir so the service and console
	// always use the same file. Empty or relative -> under the data dir.
	if cfg.StatePath == "" || cfg.StatePath == defaultStatePath {
		cfg.StatePath = paths.State()
	} else if !filepath.IsAbs(cfg.StatePath) {
		cfg.StatePath = filepath.Join(paths.DataDir(), cfg.StatePath)
	}
	if cfg.BrowserMatchURL == "" {
		cfg.BrowserMatchURL = "web.max.ru"
	}
	if cfg.WebAppListen == "" {
		cfg.WebAppListen = "127.0.0.1:8095"
	}
	// Default the CDP endpoint to the standard local debug port so browser
	// token refresh works out of the box (an empty field otherwise disabled it).
	if cfg.BrowserCDPURL == "" {
		cfg.BrowserCDPURL = "http://127.0.0.1:9222"
	}
	// 0 = unset -> use the confirmed NEW_MESSAGE opcode. Only opcode 128 frames
	// are treated as incoming messages (set a different value to override).
	if cfg.IncomingOpcode == 0 {
		cfg.IncomingOpcode = 128
	}
	if cfg.HistoryCount == 0 {
		cfg.HistoryCount = 20 // sensible default; set a negative value to disable
	}
	if cfg.HistoryCount < 0 {
		cfg.HistoryCount = 0
	}
	if cfg.HistoryCount > 200 {
		cfg.HistoryCount = 200 // hard cap to avoid flooding / rate limits
	}
	if cfg.SyncCount == 0 {
		cfg.SyncCount = 200 // generous catch-up window; dedup makes re-checking cheap
	}
	if cfg.SyncCount < 0 {
		cfg.SyncCount = 0
	}
	if cfg.SyncCount > 1000 {
		cfg.SyncCount = 1000 // upper bound of messages a catch-up delivers per chat
	}
	return cfg, nil
}

// Load returns a fully-validated config (all fields required to run the bridge).
func Load(path string) (*Config, error) {
	cfg, err := load(path)
	if err != nil {
		return nil, err
	}
	// The MAX phone is not required here: signing in to MAX happens from the
	// Telegram bot (/login), which asks for it.
	if cfg.TelegramBotToken == "" {
		return nil, fmt.Errorf("telegram_bot_token is required (TELEGRAM_BOT_TOKEN)")
	}
	// telegram_group_id may be 0: it is auto-detected from the first message
	// the bot sees in a group, then persisted back to config.json.
	return cfg, nil
}

// Save writes the config as pretty JSON (0600, secrets inside).
func Save(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// LoadRaw loads a config with defaults/env but no validation (for the wizard).
func LoadRaw(path string) (*Config, error) {
	return load(path)
}

// Complete reports whether the required fields are set. TelegramGroupID is not
// required here — it is auto-detected at runtime and persisted.
func (c *Config) Complete() bool {
	return c.TelegramBotToken != ""
}

// LoadPartial returns a config for the console's MAX sign-in (the phone may be
// empty — the console asks for it).
func LoadPartial(path string) (*Config, error) {
	return load(path)
}

// UseTCP reports whether the bridge should talk to MAX over the desktop TCP
// transport. Unless max_transport forces one, it follows the stored token: no
// token yet → TCP (the browserless sign-in is the default path); a token minted
// by that sign-in ("desktop") → TCP; any other token (browser import, or a
// state file from before token kinds existed) → the web WebSocket.
func (c *Config) UseTCP(hasToken bool, tokenKind string) bool {
	switch strings.ToLower(strings.TrimSpace(c.MaxTransport)) {
	case "tcp":
		return true
	case "ws":
		return false
	}
	return !hasToken || tokenKind == "desktop"
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("MAX_WS_URL"); v != "" {
		cfg.MaxWSURL = v
	}
	if v := os.Getenv("MAX_PHONE"); v != "" {
		cfg.Phone = v
	}
	if v := os.Getenv("TELEGRAM_BOT_TOKEN"); v != "" {
		cfg.TelegramBotToken = v
	}
	if v := os.Getenv("TELEGRAM_GROUP_ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.TelegramGroupID = n
		}
	}
	if v := os.Getenv("TELEGRAM_PROXY"); v != "" {
		cfg.TelegramProxy = v
	}
	if v := os.Getenv("BROWSER_CDP_URL"); v != "" {
		cfg.BrowserCDPURL = v
	}
	if v := os.Getenv("BROWSER_MATCH_URL"); v != "" {
		cfg.BrowserMatchURL = v
	}
	if v := os.Getenv("STATE_PATH"); v != "" {
		cfg.StatePath = v
	}
	if v := os.Getenv("INCOMING_OPCODE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.IncomingOpcode = n
		}
	}
}
