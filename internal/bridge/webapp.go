package bridge

import (
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"maxtg/internal/maxapi"
	"maxtg/internal/paths"
	"maxtg/internal/store"
	"maxtg/internal/tg"
)

// The Telegram Mini App: the panel's buttons as a web page. Telegram opens it
// at cfg.WebAppURL (an HTTPS reverse proxy in front of cfg.WebAppListen); every
// API call carries the launch's signed initData, which is checked against the
// bot token, and the user must be an admin of the bridged group — the same rule
// as for the panel's buttons.

//go:embed webapp/index.html
var webappFS embed.FS

const (
	webInitDataMaxAge = 24 * time.Hour
	webAdminTTL       = time.Minute
)

type webAuthState struct {
	mu     sync.Mutex
	admins map[int64]time.Time // user id -> when the "is admin" verdict expires
	act    sync.Mutex          // serializes state-changing actions
}

// RunWebApp serves the Mini App until the process exits. It returns at once
// when the Mini App is switched off (webapp_listen = "-").
func (b *Bridge) RunWebApp() {
	addr := b.cfg.WebAppListen
	if addr == "" || addr == "-" {
		return
	}
	if u := b.cfg.WebAppURL; u != "" {
		go b.setMenuButton(u)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", b.webIndex)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") })
	mux.HandleFunc("/api/state", b.webAPI(b.webState))
	mux.HandleFunc("/api/action", b.webAPI(b.webAction))
	mux.HandleFunc("/api/log", b.webAPI(b.webLog))
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second}
	log.Printf("[webapp] listening on %s (public url %q)", addr, b.cfg.WebAppURL)
	if err := srv.ListenAndServe(); err != nil {
		log.Printf("[webapp] stopped: %v", err)
	}
}

// setMenuButton points the bot's menu button at the Mini App, retrying while
// Telegram is not reachable yet (the proxy may come up a moment after us).
func (b *Bridge) setMenuButton(u string) {
	for i := 0; i < 6; i++ {
		err := b.bot.SetMenuButtonWebApp("Управление", u)
		if err == nil {
			log.Printf("[webapp] menu button set to %s", u)
			return
		}
		log.Printf("[webapp] setting the menu button failed: %v", err)
		time.Sleep(15 * time.Second)
	}
}

func (b *Bridge) webIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := webappFS.ReadFile("webapp/index.html")
	if err != nil {
		http.Error(w, "no page", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self' 'unsafe-inline' https://telegram.org; style-src 'unsafe-inline'; connect-src 'self'; img-src data:")
	_, _ = w.Write(data)
}

// ---- auth ----

// validateInitData checks a Mini App's initData string against the bot token
// (https://core.telegram.org/bots/webapps#validating-data-received-via-the-mini-app)
// and returns the launching user's id.
func validateInitData(initData, botToken string, now time.Time) (int64, error) {
	vals, err := url.ParseQuery(initData)
	if err != nil || initData == "" {
		return 0, errors.New("no init data")
	}
	hash := vals.Get("hash")
	vals.Del("hash")
	if hash == "" {
		return 0, errors.New("no hash")
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := make([]string, len(keys))
	for i, k := range keys {
		lines[i] = k + "=" + vals.Get(k)
	}
	secret := hmacSHA256([]byte("WebAppData"), []byte(botToken))
	want := hex.EncodeToString(hmacSHA256(secret, []byte(strings.Join(lines, "\n"))))
	if !hmac.Equal([]byte(want), []byte(strings.ToLower(hash))) {
		return 0, errors.New("bad signature")
	}
	secs, err := strconv.ParseInt(vals.Get("auth_date"), 10, 64)
	if err != nil {
		return 0, errors.New("no auth_date")
	}
	if age := now.Sub(time.Unix(secs, 0)); age > webInitDataMaxAge || age < -5*time.Minute {
		return 0, errors.New("init data expired")
	}
	var u struct {
		ID int64 `json:"id"`
	}
	if json.Unmarshal([]byte(vals.Get("user")), &u) != nil || u.ID == 0 {
		return 0, errors.New("no user")
	}
	return u.ID, nil
}

func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

// webAdmin is isAdmin with a short cache — the page polls, and every poll must
// not cost a getChatAdministrators call.
func (b *Bridge) webAdmin(userID int64) bool {
	b.web.mu.Lock()
	if b.web.admins == nil {
		b.web.admins = make(map[int64]time.Time)
	}
	if exp, ok := b.web.admins[userID]; ok && time.Now().Before(exp) {
		b.web.mu.Unlock()
		return true
	}
	b.web.mu.Unlock()
	if !b.isAdmin(userID) {
		return false
	}
	b.web.mu.Lock()
	b.web.admins[userID] = time.Now().Add(webAdminTTL)
	b.web.mu.Unlock()
	return true
}

// webAPI wraps an API handler with the auth check, panic recovery and JSON
// plumbing.
func (b *Bridge) webAPI(h func(r *http.Request) (any, int, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				b.ReportPanic("веб-приложение", rec)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "внутренняя ошибка"})
			}
		}()
		w.Header().Set("Cache-Control", "no-store")
		uid, err := validateInitData(r.Header.Get("X-Init-Data"), b.cfg.TelegramBotToken, time.Now())
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Откройте приложение из Telegram (" + err.Error() + ")"})
			return
		}
		if !b.webAdmin(uid) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "Только для администраторов группы"})
			return
		}
		out, code, err := h(r)
		if err != nil {
			writeJSON(w, code, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ---- state ----

type webMuted struct {
	Key   string `json:"key"`
	Title string `json:"title"`
}

type webStateResp struct {
	Status      string     `json:"status"`
	Level       string     `json:"level"` // ok | warn | bad
	Connected   bool       `json:"connected"`
	Paused      string     `json:"paused"`
	SelfID      int64      `json:"selfId"`
	LastErr     string     `json:"lastErr"`
	HasToken    bool       `json:"hasToken"`
	Group       int64      `json:"group"`
	Chats       int        `json:"chats"`
	Muted       []webMuted `json:"muted"`
	History     int        `json:"history"`
	Sync        int        `json:"sync"`
	Proxy       string     `json:"proxy"`
	Transport   string     `json:"transport"` // "" (auto) | tcp | ws
	TransportOn string     `json:"transportLabel"`
	Version     string     `json:"version"`
	VersionSet  string     `json:"versionManual"`
	Syncing     bool       `json:"syncing"`
	Login       string     `json:"loginLink"`
}

func (b *Bridge) webState(r *http.Request) (any, int, error) {
	chats := 0
	for k := range b.st.AllTopics() {
		if !strings.HasPrefix(k, store.PendingPrefix) {
			chats++
		}
	}
	bans := b.st.Bans()
	muted := make([]webMuted, 0, len(bans))
	for k, t := range bans {
		if t == "" {
			t = "Чат " + k
		}
		muted = append(muted, webMuted{Key: k, Title: t})
	}
	sort.Slice(muted, func(i, j int) bool { return muted[i].Title < muted[j].Title })

	_, token, _, _ := b.st.GetCredentials()
	connected, selfID, lastErr := b.max.Status()
	paused := b.pauseLabel()
	level := "bad"
	switch {
	case paused != "":
		level = "warn"
	case connected:
		level = "ok"
	}
	proxy := ""
	if b.cfg.TelegramProxy != "" {
		proxy = maskProxy(b.cfg.TelegramProxy)
	}
	tr := strings.ToLower(b.cfg.MaxTransport)
	if tr != "tcp" && tr != "ws" {
		tr = ""
	}
	return webStateResp{
		Status: b.statusLine(), Level: level, Connected: connected, Paused: paused, SelfID: selfID,
		LastErr: truncateRunes(lastErr, 300), HasToken: token != "", Group: b.bot.GroupID(), Chats: chats, Muted: muted,
		History: b.cfg.HistoryCount, Sync: b.cfg.SyncCount, Proxy: proxy, Transport: tr,
		TransportOn: b.transportLabel(), Version: b.versionLabel(), VersionSet: b.cfg.MaxAppVersion,
		Syncing: b.syncing.Load(), Login: b.loginLink(),
	}, 0, nil
}

// ---- actions ----

type webActionReq struct {
	Action  string `json:"action"`
	Seconds int    `json:"seconds"`
	Key     string `json:"key"`
	N       int    `json:"n"`
	Value   string `json:"value"`
}

type webActionResp struct {
	OK         bool   `json:"ok"`
	Message    string `json:"message"`
	Restarting bool   `json:"restarting"`
}

func (b *Bridge) webAction(r *http.Request) (any, int, error) {
	if r.Method != http.MethodPost {
		return nil, http.StatusMethodNotAllowed, errors.New("нужен POST")
	}
	var req webActionReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		return nil, http.StatusBadRequest, errors.New("неверный запрос")
	}
	b.web.act.Lock()
	defer b.web.act.Unlock()

	ok := func(msg string) (any, int, error) { return webActionResp{OK: true, Message: msg}, 0, nil }
	restart := func(msg, why string) (any, int, error) {
		b.notify(why + " — мост перезапускается.")
		go b.restart()
		return webActionResp{OK: true, Message: msg, Restarting: true}, 0, nil
	}
	bad := func(msg string) (any, int, error) { return nil, http.StatusBadRequest, errors.New(msg) }

	switch req.Action {
	case "pause":
		if req.Seconds < 0 || req.Seconds > 30*24*3600 {
			return bad("неверная длительность")
		}
		b.pauseMax(time.Duration(req.Seconds) * time.Second)
		return ok("MAX на паузе " + b.pauseLabel())
	case "resume":
		if b.max.Resume() {
			return ok("MAX возобновлён")
		}
		return ok("MAX не на паузе")
	case "resync":
		if b.syncing.Load() {
			return ok("Пересинхронизация уже идёт")
		}
		go b.SyncAllChats()
		return ok("Пересинхронизация запущена")
	case "restart":
		return restart("Перезапускаюсь…", "🔄 Перезапуск из веб-приложения")
	case "unban":
		title, was := b.st.Unban(req.Key)
		if !was {
			return ok("Уже не заглушён")
		}
		if title == "" {
			title = req.Key
		}
		return ok("«" + truncateRunes(title, 40) + "» снова пересылается")
	case "history", "sync":
		if req.N < 0 || req.N > 200 {
			return bad("от 0 до 200")
		}
		if req.Action == "history" {
			b.cfg.HistoryCount = req.N
		} else {
			b.cfg.SyncCount = req.N
		}
		b.saveConfig()
		return ok("Сохранено")
	case "transport":
		v := strings.ToLower(req.Value)
		if v != "" && v != "tcp" && v != "ws" {
			return bad("транспорт: auto, tcp или ws")
		}
		b.cfg.MaxTransport = v
		b.saveConfig()
		return restart("Транспорт сохранён, перезапускаюсь…", "🔁 Транспорт MAX изменён")
	case "version":
		v := strings.TrimSpace(req.Value)
		if v == "" {
			b.cfg.MaxAppVersion, b.cfg.MaxAppVersionAuto = "", ""
		} else if !maxapi.ValidVersion(v) {
			return bad("не похоже на версию, пример: 26.24.0")
		} else {
			b.cfg.MaxAppVersion = v
		}
		b.saveConfig()
		return restart("Версия сохранена, перезапускаюсь…", "🧬 Версия MAX изменена")
	case "proxy":
		proxy := strings.TrimSpace(req.Value)
		if proxy == "-" {
			proxy = ""
		}
		if proxy != "" {
			u, err := url.Parse(proxy)
			if err != nil || u.Host == "" || !strings.Contains("socks5 socks5h http https", u.Scheme) {
				return bad("не похоже на адрес прокси, пример: socks5://127.0.0.1:1080")
			}
			test, err := tg.New(b.cfg.TelegramBotToken, b.bot.GroupID(), proxy)
			if err == nil {
				_, err = test.GetMe()
			}
			if err != nil {
				return bad("через этот прокси Telegram не отвечает: " + shortErr(err))
			}
		}
		b.cfg.TelegramProxy = proxy
		b.saveConfig()
		return restart("Прокси сохранён, перезапускаюсь…", "🌐 Прокси Telegram изменён")
	}
	return bad("неизвестное действие")
}

// ---- log ----

func (b *Bridge) webLog(r *http.Request) (any, int, error) {
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 || n > 300 {
		n = 80
	}
	return map[string]string{"log": tailFile(paths.Log(), n, 64<<10)}, 0, nil
}

// tailFile returns the last n lines of path, reading at most the final maxBytes.
func tailFile(path string, n int, maxBytes int64) string {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("Лога пока нет (%s): %v", path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err.Error()
	}
	start := st.Size() - maxBytes
	if start < 0 {
		start = 0
	}
	buf := make([]byte, st.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return err.Error()
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if start > 0 && len(lines) > 1 {
		lines = lines[1:] // the first line is probably cut in half
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
