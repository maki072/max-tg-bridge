package maxapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	utls "github.com/refraction-networking/utls"
)

// maxDialer connects to MAX ALWAYS directly (never via HTTP(S)_PROXY env — that
// would tunnel MAX through the Telegram-only proxy). It performs the TLS
// handshake with a real Firefox ClientHello via uTLS: MAX's edge/WAF completes
// the WebSocket upgrade for any client but only services frames (replies to
// INIT) for browser-fingerprinted TLS — a plain Go TLS hello gets a silent
// "timeout waiting for opcode 6". Mimicking Firefox makes us look like the
// working web client.
var maxDialer = &websocket.Dialer{
	Proxy:             nil,
	HandshakeTimeout:  20 * time.Second,
	NetDialTLSContext: maxTLSDial,
}

// maxTLSDial opens a TCP connection and completes a Firefox-shaped TLS handshake.
func maxTLSDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	d := net.Dialer{Timeout: 15 * time.Second}
	raw, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	uconn := utls.UClient(raw, &utls.Config{ServerName: host}, utls.HelloFirefox_Auto)
	if err := uconn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("utls handshake: %w", err)
	}
	return uconn, nil
}

// Diagnose runs a staged connectivity probe to the MAX websocket and returns a
// human-readable report: DNS, TCP, TLS (incl. cert issuer to spot MITM/DPI),
// WebSocket upgrade, and whether INIT (opcode 6) gets a reply. Runs wherever the
// bridge runs (e.g. the server), so it shows that host's real reachability.
func Diagnose(wsURL string) string {
	var sb strings.Builder
	host := strings.TrimPrefix(strings.TrimPrefix(wsURL, "wss://"), "ws://")
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	hostname := host
	addr := host
	if strings.Contains(host, ":") {
		hostname, _, _ = net.SplitHostPort(host)
	} else {
		addr = host + ":443"
	}
	fmt.Fprintf(&sb, "🩺 MAX diag → %s\n", addr)

	ips, err := net.LookupHost(hostname)
	if err != nil {
		fmt.Fprintf(&sb, "DNS: FAIL %v\n", err)
		return sb.String()
	}
	fmt.Fprintf(&sb, "DNS: %v\n", ips)

	t0 := time.Now()
	raw, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		fmt.Fprintf(&sb, "TCP: FAIL %v\n", err)
		return sb.String()
	}
	fmt.Fprintf(&sb, "TCP: ok (%v)\n", time.Since(t0).Round(time.Millisecond))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	uconn := utls.UClient(raw, &utls.Config{ServerName: hostname}, utls.HelloFirefox_Auto)
	if err := uconn.HandshakeContext(ctx); err != nil {
		fmt.Fprintf(&sb, "TLS: FAIL %v\n", err)
		raw.Close()
		return sb.String()
	}
	cs := uconn.ConnectionState()
	if len(cs.PeerCertificates) > 0 {
		fmt.Fprintf(&sb, "TLS: ok alpn=%q\n cert-subj: %s\n cert-issuer: %s\n",
			cs.NegotiatedProtocol, cs.PeerCertificates[0].Subject.String(), cs.PeerCertificates[0].Issuer.String())
	} else {
		fmt.Fprintf(&sb, "TLS: ok alpn=%q (no peer cert!)\n", cs.NegotiatedProtocol)
	}
	uconn.Close()

	conn, resp, err := maxDialer.Dial(wsURL, http_headers("https://web.max.ru", "", ""))
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		fmt.Fprintf(&sb, "WS upgrade: FAIL %v (http %d)\n", err, code)
		return sb.String()
	}
	defer conn.Close()
	fmt.Fprintf(&sb, "WS upgrade: ok\n")

	dev, _ := newUUID()
	payload, _ := json.Marshal(map[string]any{"userAgent": defaultUserAgent(), "deviceId": dev})
	frame, _ := json.Marshal(Frame{Ver: ProtocolVersion, Cmd: CmdRequest, Seq: 1, Opcode: OpInit, Payload: payload})
	_ = conn.WriteMessage(websocket.TextMessage, frame)
	_ = conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		fmt.Fprintf(&sb, "INIT reply: NONE (%v)\n", err)
		return sb.String()
	}
	s := string(data)
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	fmt.Fprintf(&sb, "INIT reply: %s\n", s)
	return sb.String()
}

func http_headers(origin, userAgent, cookie string) http.Header {
	h := http.Header{}
	h.Set("Origin", origin)
	if userAgent == "" {
		userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:141.0) Gecko/20100101 Firefox/141.0"
	}
	h.Set("User-Agent", userAgent)
	// Browser-like fetch metadata. The MAX edge/WAF accepts the socket but does
	// not service frames (INIT gets no reply) unless these are present — matches
	// the working web client and the MaxBridge reference implementation.
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "websocket")
	h.Set("Sec-Fetch-Site", "cross-site")
	if cookie != "" {
		h.Set("Cookie", cookie)
	}
	return h
}

type Client struct {
	url       string
	origin    string
	phone     string
	forceOp   int
	deviceID  string
	token     string
	userAgent string
	cookie    string

	conn     *websocket.Conn
	writeMu  sync.Mutex
	seq      int
	seqMu    sync.Mutex
	pending  map[int]chan *Frame
	pendMu   sync.Mutex
	selfID   int64
	chatMu   sync.RWMutex
	chatName map[int64]string
	userName map[int64]string

	allowSMS  bool
	statMu    sync.RWMutex
	connected bool
	lastErr   string

	onMessage func(IncomingMessage)
	saveToken func(deviceID, token, userAgent, cookie string)
	loadToken func() (deviceID, token, cookie string)
	readCode  func() string
	onCaptcha func(link string) string
	refresh   func() (deviceID, token, userAgent, cookie string, err error)

	debugMu  sync.Mutex
	debugLog []string
}

type Options struct {
	URL         string
	Phone       string
	DeviceID    string
	Token       string
	UserAgent   string
	Cookie      string
	ForceOpcode int
	AllowSMS    bool
	OnMessage   func(IncomingMessage)
	SaveToken   func(deviceID, token, userAgent, cookie string)
	LoadToken   func() (deviceID, token, cookie string)
	ReadSMSCode func() string
	OnCaptcha   func(link string) string
	Refresh     func() (deviceID, token, userAgent, cookie string, err error)
}

func New(o Options) *Client {
	return &Client{
		url:       o.URL,
		origin:    "https://web.max.ru",
		phone:     o.Phone,
		forceOp:   o.ForceOpcode,
		deviceID:  o.DeviceID,
		token:     o.Token,
		userAgent: o.UserAgent,
		cookie:    o.Cookie,
		allowSMS:  o.AllowSMS,
		pending:   make(map[int]chan *Frame),
		chatName:  make(map[int64]string),
		userName:  make(map[int64]string),
		onMessage: o.OnMessage,
		saveToken: o.SaveToken,
		loadToken: o.LoadToken,
		readCode:  o.ReadSMSCode,
		onCaptcha: o.OnCaptcha,
		refresh:   o.Refresh,
	}
}

func (c *Client) Run() {
	attempt := 0
	for {
		start := time.Now()
		err := c.sessionSafe()
		// A session that stayed up for a while was healthy — reset the backoff so
		// a flaky-but-working link reconnects promptly instead of ramping to 30s.
		if time.Since(start) > 60*time.Second {
			attempt = 0
		}
		if err != nil {
			attempt++
			wait := time.Duration(1<<attempt) * time.Second
			if wait > 30*time.Second {
				wait = 30 * time.Second
			}
			log.Printf("[max] session ended: %v; reconnect in %v", err, wait)
			time.Sleep(wait)
			continue
		}
		attempt = 0
	}
}

// sessionSafe runs one session, turning any panic into an error so the reconnect
// loop keeps the process alive instead of crashing (nothing else restarts it).
func (c *Client) sessionSafe() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in session: %v", r)
		}
	}()
	return c.session()
}

func (c *Client) session() (sessErr error) {
	c.setStatus(false, "")
	defer func() {
		if sessErr != nil {
			c.setStatus(false, sessErr.Error())
		}
	}()

	if c.token == "" && c.refresh != nil {
		if d, t, ua, ck, err := c.refresh(); err == nil {
			c.mergeRefresh(d, t, ua, ck)
		}
	}
	if c.token == "" && c.loadToken != nil {
		if d, t, ck := c.loadToken(); t != "" {
			c.deviceID = d
			c.token = t
			c.cookie = ck
		}
	}
	if c.token == "" && !c.allowSMS {
		if c.refresh != nil {
			return fmt.Errorf("no MAX token; browser refresh yielded nothing")
		}
		return fmt.Errorf("no MAX token available")
	}

	// Handshake must stay consistent with our uTLS Firefox fingerprint: a default
	// Firefox User-Agent and NO cookies. Sending the browser's captured Edge UA or
	// its web.max.ru cookies makes the WAF drop the socket (1006 EOF) — the probe
	// in Diagnose (no cookie, default UA) connects and gets an INIT reply fine.
	conn, _, err := maxDialer.Dial(c.url, http_headers(c.origin, "", ""))
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	c.conn = conn

	c.pendMu.Lock()
	c.pending = make(map[int]chan *Frame)
	c.seq = 0
	c.pendMu.Unlock()

	readErr := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				select {
				case readErr <- fmt.Errorf("panic in readLoop: %v", r):
				default:
				}
			}
		}()
		readErr <- c.readLoop()
	}()

	if err := c.init(); err != nil {
		return fmt.Errorf("init: %w", err)
	}
	log.Printf("[max] INIT ok")

	if c.token == "" {
		if err := c.smsLoginWithCaptcha(); err != nil {
			return fmt.Errorf("sms login: %w", err)
		}
	}

	if err := c.login(); err != nil {
		c.token = ""
		return fmt.Errorf("login: %w", err)
	}
	log.Printf("[max] LOGIN ok (selfID=%d); connected", c.selfID)
	c.setStatus(true, "")

	done := make(chan struct{})
	go c.heartbeat(done)

	err = <-readErr
	close(done)
	return err
}

// mergeRefresh applies a browser-grabbed session WITHOUT adopting the browser's
// deviceId. The MAX token is account-scoped and works with our own deviceId;
// reusing the browser's live deviceId makes MAX treat us as the same device as
// the open tab and kicks one of the two sessions (the "MAX disconnected" churn).
// Keeping our own deviceId lets the service and the logged-in browser coexist,
// so the browser never has to be closed.
func (c *Client) mergeRefresh(browserDeviceID, token, userAgent, cookie string) {
	if token == "" {
		return
	}
	// Never share the browser's deviceId. If ours ever equals it (e.g. a legacy
	// state.json that saved the browser's id), mint a fresh one so the server
	// doesn't treat us as the same device and close one of the two sessions.
	if browserDeviceID != "" && browserDeviceID == c.deviceID {
		if id, err := newUUID(); err == nil {
			c.deviceID = id
		}
	}
	c.token = token
	c.userAgent = userAgent
	c.cookie = cookie
	if c.saveToken != nil {
		c.saveToken(c.deviceID, c.token, c.userAgent, c.cookie)
	}
}

func (c *Client) setStatus(connected bool, lastErr string) {
	c.statMu.Lock()
	c.connected = connected
	if lastErr != "" {
		c.lastErr = lastErr
	}
	c.statMu.Unlock()
}

func (c *Client) Status() (connected bool, selfID int64, lastErr string) {
	c.statMu.RLock()
	defer c.statMu.RUnlock()
	return c.connected, c.selfID, c.lastErr
}

func (c *Client) AuthOnce() error {
	// Handshake must stay consistent with our uTLS Firefox fingerprint: a default
	// Firefox User-Agent and NO cookies. Sending the browser's captured Edge UA or
	// its web.max.ru cookies makes the WAF drop the socket (1006 EOF) — the probe
	// in Diagnose (no cookie, default UA) connects and gets an INIT reply fine.
	conn, _, err := maxDialer.Dial(c.url, http_headers(c.origin, "", ""))
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	c.conn = conn
	c.pendMu.Lock()
	c.pending = make(map[int]chan *Frame)
	c.pendMu.Unlock()
	readErr := make(chan error, 1)
	go func() { readErr <- c.readLoop() }()
	if err := c.init(); err != nil {
		return fmt.Errorf("init: %w", err)
	}
	c.token = ""
	if err := c.smsLoginWithCaptcha(); err != nil {
		return fmt.Errorf("sms login: %w", err)
	}
	if err := c.login(); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	return nil
}

func (c *Client) smsLoginWithCaptcha() error {
	err := c.smsLogin("")
	if err == nil {
		return nil
	}
	if c.onCaptcha == nil {
		return err
	}
	link, cerr := c.requestCaptcha()
	if cerr != nil {
		return cerr
	}
	return c.smsLogin(c.onCaptcha(link))
}

func (c *Client) requestCaptcha() (string, error) {
	resp, err := c.request(OpCaptchaRequest, map[string]any{"source": "auth", "identifier": c.phone}, 30*time.Second)
	if err != nil {
		return "", err
	}
	var r struct{ Link string `json:"link"` }
	if err := json.Unmarshal(resp.Payload, &r); err != nil {
		return "", err
	}
	return r.Link, nil
}

func (c *Client) init() error {
	// The server ignores INIT (no ACK -> "timeout waiting for reply to opcode 6")
	// unless userAgent matches the documented web-client shape. Use that shape
	// (defaultUserAgent) and stamp in the real captured browser UA if we have it.
	// Keep the INIT userAgent consistent with the Firefox handshake (do not stamp
	// in the captured Edge UA) — this matches the Diagnose probe that gets ACKed.
	_, err := c.request(OpInit, map[string]any{
		"userAgent": defaultUserAgent(),
		"deviceId":  c.deviceID,
	}, 15*time.Second)
	return err
}

func (c *Client) smsLogin(captchaToken string) error {
	req := map[string]any{"phone": c.phone, "type": "START_AUTH", "language": "ru"}
	if captchaToken != "" {
		req["captchaToken"] = captchaToken
	}
	resp, err := c.request(OpVerificationRequest, req, 30*time.Second)
	if err != nil {
		return err
	}
	var vr struct{ Token string `json:"token"` }
	if err := json.Unmarshal(resp.Payload, &vr); err != nil {
		return err
	}
	enter, err := c.request(OpCodeEnter, map[string]any{"code": c.readCode(), "token": vr.Token}, 30*time.Second)
	if err != nil {
		return err
	}
	
	type loginStruct struct { Token string `json:"token"` }
	type pLoginStruct struct { Login loginStruct `json:"LOGIN"` }
	type ceStruct struct { TokenAttrs pLoginStruct `json:"tokenAttrs"` }
	
	var ce ceStruct
	if err := json.Unmarshal(enter.Payload, &ce); err != nil {
		return err
	}
	c.token = ce.TokenAttrs.Login.Token
	if c.saveToken != nil {
		c.saveToken(c.deviceID, c.token, c.userAgent, c.cookie)
	}
	return nil
}

func (c *Client) login() error {
	resp, err := c.request(OpLogin, map[string]any{
		"interactive":  true,
		"token":        c.token,
		"chatsCount":   100,
		"chatsSync":    100,
		"contactsSync": 0,
		"presenceSync": 0,
		"draftsSync":   0,
	}, 30*time.Second)
	if err != nil {
		return err
	}
	type idStruct struct { ID json.Number `json:"id"` }
	type TitleStruct struct { ID json.Number `json:"id"`; Title string `json:"title"` }
	type profileStruct struct { Contact idStruct `json:"contact"` }
	type lrStruct struct {
		Profile profileStruct `json:"profile"`
		Chats   []TitleStruct `json:"chats"`
		Users   []contactInfo `json:"users"`
	}

	var lr lrStruct
	if err := json.Unmarshal(resp.Payload, &lr); err != nil {
		return err
	}
	c.selfID = numToInt64(lr.Profile.Contact.ID)
	c.chatMu.Lock()
	for _, ch := range lr.Chats {
		if ch.Title != "" {
			c.chatName[numToInt64(ch.ID)] = ch.Title
		}
	}
	for _, u := range lr.Users {
		// MAX names are {name,type}, not first/last — use the FULL_NAME entry.
		c.userName[numToInt64(u.ID)] = pickName(u.Names)
	}
	c.chatMu.Unlock()
	return nil
}

// fetchChatTitle resolves a chat's display title via GET_CHAT_INFO (opcode 61).
// For a DIALOG this is the peer's name; for CHAT/CHANNEL it is the group title.
// MUST be called off the read loop (it waits for a reply the read loop delivers).
func (c *Client) fetchChatTitle(chatID int64) string {
	resp, err := c.request(OpGetChatInfo, map[string]any{"chatId": chatID}, 10*time.Second)
	if err != nil {
		return ""
	}
	var r struct {
		Chat struct {
			Title string `json:"title"`
			Type  string `json:"type"`
		} `json:"chat"`
	}
	if json.Unmarshal(resp.Payload, &r) != nil {
		return ""
	}
	return r.Chat.Title
}

// fetchUserName resolves a user's display name via GET_CONTACTS (opcode 32).
// MUST be called off the read loop.
func (c *Client) fetchUserName(userID int64) string {
	resp, err := c.request(OpGetContacts, map[string]any{"contactIds": []int64{userID}}, 10*time.Second)
	if err != nil {
		return ""
	}
	var r struct {
		Contacts []contactInfo `json:"contacts"`
	}
	if json.Unmarshal(resp.Payload, &r) != nil {
		return ""
	}
	for _, ct := range r.Contacts {
		if numToInt64(ct.ID) == userID {
			return pickName(ct.Names)
		}
	}
	if len(r.Contacts) > 0 {
		return pickName(r.Contacts[0].Names)
	}
	return ""
}

func (c *Client) SendMessage(chatID int64, text string) error {
	// MAX expects the message wrapped: {chatId, message:{text,cid,elements,attaches}, notify}
	// (matches the web client / MaxBridge). A flat {text} payload is ignored.
	cid := time.Now().UnixNano()
	_, err := c.request(OpMsgSend, map[string]any{
		"chatId": chatID,
		"message": map[string]any{
			"text":     text,
			"cid":      cid,
			"elements": []any{},
			"attaches": []any{},
		},
		"notify": true,
	}, 15*time.Second)
	return err
}

func (c *Client) request(opcode int, payload any, timeout time.Duration) (*Frame, error) {
	c.seqMu.Lock()
	c.seq++
	seq := c.seq
	c.seqMu.Unlock()
	ch := make(chan *Frame, 1)
	c.pendMu.Lock()
	c.pending[seq] = ch
	c.pendMu.Unlock()
	raw, _ := json.Marshal(payload)
	f := Frame{Ver: ProtocolVersion, Cmd: CmdRequest, Seq: seq, Opcode: opcode, Payload: raw}
	if err := c.writeFrame(f); err != nil {
		c.pendMu.Lock()
		delete(c.pending, seq)
		c.pendMu.Unlock()
		return nil, err
	}
	select {
	case res := <-ch:
		if res.Cmd == CmdError {
			return nil, fmt.Errorf("server error (opcode %d): %s", opcode, string(res.Payload))
		}
		return res, nil
	case <-time.After(timeout):
		c.pendMu.Lock()
		delete(c.pending, seq)
		c.pendMu.Unlock()
		return nil, fmt.Errorf("timeout waiting for reply to opcode %d (seq %d)", opcode, seq)
	}
}

func (c *Client) writeFrame(f Frame) error {
	raw, _ := json.Marshal(f)
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}
	return c.conn.WriteMessage(websocket.TextMessage, raw)
}

func (c *Client) readLoop() error {
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return err
		}
		var f Frame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		if f.Cmd == CmdAck || f.Cmd == CmdError {
			c.pendMu.Lock()
			ch := c.pending[f.Seq]
			delete(c.pending, f.Seq)
			c.pendMu.Unlock()
			if ch != nil {
				ch <- &f
			}
			continue
		}
		c.handlePush(f)
	}
}

func (c *Client) handlePush(f Frame) {
	c.debugMu.Lock()
	if len(c.debugLog) > 50 {
		c.debugLog = c.debugLog[1:]
	}
	s := string(f.Payload)
	if len(s) > 1000 {
		s = s[:1000] + "..."
	}
	c.debugLog = append(c.debugLog, fmt.Sprintf("OPCODE=%d PAYLOAD=%s", f.Opcode, s))
	c.debugMu.Unlock()

	if f.Opcode == OpPing {
		_ = c.writeFrame(Frame{Ver: ProtocolVersion, Cmd: CmdRequest, Seq: 0, Opcode: OpPing, Payload: json.RawMessage(`{"interactive":true}`)})
		return
	}
	if c.tryMessage(f) {
		return
	}
	if !ignoredPushOpcodes[f.Opcode] {
		log.Printf("[max][push] unknown opcode=%d payload=%s", f.Opcode, s)
	}
}

func (c *Client) tryMessage(f Frame) bool {
	if c.forceOp > 0 {
		if f.Opcode != c.forceOp {
			return false
		}
	} else if ignoredPushOpcodes[f.Opcode] {
		return false
	}
	var pm pushMessage
	if err := json.Unmarshal(f.Payload, &pm); err != nil {
		return false
	}
	if pm.Message == nil || strings.TrimSpace(pm.Message.Text) == "" {
		return false
	}
	chatID := numToInt64(pm.ChatID)
	senderID := numToInt64(pm.Message.Sender)
	if chatID == 0 {
		return false
	}
	if senderID != 0 && senderID == c.selfID {
		return true
	}
	if c.onMessage == nil {
		return true
	}
	c.chatMu.RLock()
	title := c.chatName[chatID]
	sender := c.userName[senderID]
	c.chatMu.RUnlock()

	msg := IncomingMessage{
		MessageID:  pm.Message.ID.String(),
		ChatID:     chatID,
		SenderID:   senderID,
		Text:       pm.Message.Text,
	}

	if title != "" && (sender != "" || senderID == 0) {
		msg.ChatTitle = title
		msg.SenderName = sender
		c.onMessage(msg)
		return true
	}

	// Chat title and/or sender name unknown — resolve them off the read loop
	// (GET_CHAT_INFO / GET_CONTACTS), cache, then forward with proper names.
	go func() {
		if title == "" {
			if t := c.fetchChatTitle(chatID); t != "" {
				title = t
				c.chatMu.Lock(); c.chatName[chatID] = t; c.chatMu.Unlock()
			}
		}
		if sender == "" && senderID != 0 {
			if n := c.fetchUserName(senderID); n != "" {
				sender = n
				c.chatMu.Lock(); c.userName[senderID] = n; c.chatMu.Unlock()
			}
		}
		if title == "" {
			title = fmt.Sprintf("Chat %d", chatID)
		}
		if sender == "" && senderID != 0 {
			sender = fmt.Sprintf("User %d", senderID)
		}
		msg.ChatTitle = title
		msg.SenderName = sender
		c.onMessage(msg)
	}()
	return true
}

func (c *Client) heartbeat(done chan struct{}) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			_ = c.writeFrame(Frame{Ver: ProtocolVersion, Cmd: CmdRequest, Seq: 0, Opcode: OpPing, Payload: json.RawMessage(`{"interactive":true}`)})
		}
	}
}

func (c *Client) DumpDebugLog() []string {
	c.debugMu.Lock()
	defer c.debugMu.Unlock()
	logCopy := make([]string, len(c.debugLog))
	copy(logCopy, c.debugLog)
	return logCopy
}