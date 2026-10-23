package maxapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// Force ALPN to http/1.1 only. A real browser opening a WebSocket never
	// offers h2 on that connection (WS upgrade is HTTP/1.1-only); leaving ALPN
	// at the Firefox preset's default (h2 + http/1.1) lets MAX's edge pick h2
	// on some routes/times, which produces raw HTTP/2 frames on the wire that
	// gorilla/websocket's HTTP/1.1 Upgrade write/read chokes on with
	// "malformed HTTP response" (the bytes are literally an HTTP/2 SETTINGS
	// frame: 00 00 1e 04 00 ...). Pinning NextProtos avoids that entirely.
	uconn := utls.UClient(raw, &utls.Config{ServerName: host, NextProtos: []string{"http/1.1"}}, utls.HelloFirefox_Auto)
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
	uconn := utls.UClient(raw, &utls.Config{ServerName: hostname, NextProtos: []string{"http/1.1"}}, utls.HelloFirefox_Auto)
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
	binary    atomic.Bool // speak the binary msgpack transport (ver=10) instead of JSON
	binFails  int         // consecutive binary sessions that died before LOGIN (see session)
	deviceID  string
	token     string
	userAgent string
	cookie    string

	conn    *websocket.Conn
	writeMu sync.Mutex
	seq     int
	seqMu   sync.Mutex
	pending map[int]chan *Frame
	pendOp  map[int]int // seq -> opcode of in-flight requests (reply matching when the server doesn't echo seq)
	pendMu  sync.Mutex

	tcp              bool // desktop TCP transport (tcp.go) instead of a WebSocket
	appVersion       string // pinned desktop appVersion ("" = TCPVersion())
	verBumps         int    // consecutive auto version bumps since the last good LOGIN
	tcpConn         net.Conn
	tcpR             *bufio.Reader
	saveDesktopToken func(deviceID, token string)
	selfID           int64
	chatMu           sync.RWMutex
	chatName         map[int64]string
	userName         map[int64]string

	allowSMS  bool
	statMu    sync.RWMutex
	connected bool
	lastErr   string

	onMessage  func(IncomingMessage)
	onReaction func(IncomingReaction)
	onDelete   func(IncomingDelete)
	onConnect  func() // called after every successful LOGIN (initial connect AND every reconnect)
	onStatus   func(connected bool)
	saveToken  func(deviceID, token, userAgent, cookie string)
	loadToken  func() (deviceID, token, cookie string)
	readCode   func() string
	onCaptcha  func(link string) string
	refresh    func() (deviceID, token, userAgent, cookie string, err error)

	deliver     chan IncomingMessage  // decouples delivery (+ MAX lookups) from readLoop
	reactions   chan IncomingReaction // decouples reaction delivery from readLoop
	deletes     chan IncomingDelete   // decouples delete-event delivery from readLoop
	attachReady chan struct{}         // signalled on NOTIF_ATTACH (opcode 136)

	debugMu  sync.Mutex
	debugLog []string

	pause pauseState // see pause.go
}

type Options struct {
	URL         string
	Phone       string
	DeviceID    string
	Token       string
	UserAgent   string
	Cookie      string
	ForceOpcode int
	// Binary switches the WebSocket to the binary msgpack transport web.max.ru
	// itself uses (see binproto.go) — needed to receive polls in full.
	Binary bool
	// TCP selects the desktop-client TCP transport (api2.oneme.ru, DESKTOP
	// device type) — the browserless path; tokens minted by AuthSession work
	// with it. Takes precedence over URL/Binary.
	TCP bool
	// AppVersion pins the desktop appVersion for this client (default: TCPVersion()).
	AppVersion string
	// SaveDesktopToken persists a session token rotated by LOGIN over TCP.
	SaveDesktopToken func(deviceID, token string)
	AllowSMS         bool
	OnMessage        func(IncomingMessage)
	OnReaction       func(IncomingReaction)
	OnDelete         func(IncomingDelete)
	// OnConnect, if set, is called (in its own goroutine) after every successful
	// LOGIN — the initial connect AND every subsequent reconnect. Use it to
	// re-sync chat history: a live WS session only ever delivers messages sent
	// while it is actually connected, so anything sent while disconnected (the
	// service or the machine was off, a network blip, a token refresh) needs an
	// explicit catch-up pass, which this is the hook for.
	OnConnect func()
	// OnStatus, if set, is called whenever the connection flips between
	// connected and disconnected (never on a repeated state). It must not block.
	OnStatus    func(connected bool)
	SaveToken   func(deviceID, token, userAgent, cookie string)
	LoadToken   func() (deviceID, token, cookie string)
	ReadSMSCode func() string
	OnCaptcha   func(link string) string
	Refresh     func() (deviceID, token, userAgent, cookie string, err error)
}

func New(o Options) *Client {
	c := newClient(o)
	c.binary.Store(o.Binary)
	return c
}

// Binary reports whether the current session speaks the binary transport
// (false after an automatic fallback to JSON, see session).
func (c *Client) Binary() bool { return c.binary.Load() }

func newClient(o Options) *Client {
	return &Client{
		url:              o.URL,
		origin:           "https://web.max.ru",
		phone:            o.Phone,
		forceOp:          o.ForceOpcode,
		deviceID:         o.DeviceID,
		token:            o.Token,
		userAgent:        o.UserAgent,
		cookie:           o.Cookie,
		allowSMS:         o.AllowSMS,
		pending:          make(map[int]chan *Frame),
		pendOp:           make(map[int]int),
		tcp:              o.TCP,
		appVersion:       o.AppVersion,
		saveDesktopToken: o.SaveDesktopToken,
		chatName:         make(map[int64]string),
		userName:         make(map[int64]string),
		deliver:          make(chan IncomingMessage, 256),
		reactions:        make(chan IncomingReaction, 256),
		deletes:          make(chan IncomingDelete, 256),
		attachReady:      make(chan struct{}, 1),
		onMessage:        o.OnMessage,
		onReaction:       o.OnReaction,
		onDelete:         o.OnDelete,
		onConnect:        o.OnConnect,
		onStatus:         o.OnStatus,
		saveToken:        o.SaveToken,
		loadToken:        o.LoadToken,
		readCode:         o.ReadSMSCode,
		onCaptcha:        o.OnCaptcha,
		refresh:          o.Refresh,
	}
}

func (c *Client) Run() {
	go c.deliverLoop()
	go c.reactionLoop()
	go c.deleteLoop()
	attempt := 0
	for {
		c.waitWhilePaused()
		start := time.Now()
		err := c.sessionSafe()
		if errors.Is(err, errPaused) || c.Paused() {
			attempt = 0
			continue // a deliberate disconnect, not a failure — no backoff, no log noise
		}
		// A session that stayed up for a while was healthy — reset the backoff so
		// a flaky-but-working link reconnects promptly instead of ramping to 30s.
		if time.Since(start) > 60*time.Second {
			attempt = 0
		}
		if errors.Is(err, errVersionBumped) {
			log.Printf("[max] session ended: %v", err)
			attempt = 0
			continue // reconnect at once with the next app version
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
	closeConn, err := c.dial()
	if err != nil {
		return err
	}
	defer closeConn()
	if c.Paused() { // paused while we were dialing
		return errPaused
	}

	c.pendMu.Lock()
	c.pendOp = make(map[int]int)
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
		if c.bumpVersion(err) {
			return fmt.Errorf("init: %w", errVersionBumped)
		}
		c.noteBinaryFailure(err)
		return fmt.Errorf("init: %w", err)
	}
	log.Printf("[max] INIT ok")

	if c.token == "" {
		if err := c.smsLoginWithCaptcha(); err != nil {
			return fmt.Errorf("sms login: %w", err)
		}
	}

	if err := c.login(); err != nil {
		if c.bumpVersion(err) {
			return fmt.Errorf("login: %w", errVersionBumped)
		}
		c.noteBinaryFailure(err)
		c.token = ""
		return fmt.Errorf("login: %w", err)
	}
	if c.tcp && c.verBumps > 0 {
		c.verBumps = 0
		announceVersion(TCPVersion())
	}
	c.binFails = 0
	transport := "JSON"
	if c.binary.Load() {
		transport = "binary (msgpack)"
	}
	log.Printf("[max] LOGIN ok (selfID=%d); connected, transport=%s", c.selfID, transport)
	c.setStatus(true, "")
	if c.onConnect != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[max] onConnect panic: %v", r)
				}
			}()
			c.onConnect()
		}()
	}

	done := make(chan struct{})
	go c.heartbeat(done)

	err = <-readErr
	close(done)
	return err
}

// dial opens the transport for this session — the desktop TCP socket, or the
// (JSON / binary) WebSocket — and returns the function that closes it.
func (c *Client) dial() (func(), error) {
	if c.tcp {
		conn, err := dialMaxTCP(context.Background())
		if err != nil {
			return nil, fmt.Errorf("dial %s: %w", TCPAddr, err)
		}
		c.writeMu.Lock()
		c.tcpConn = conn
		c.tcpR = bufio.NewReaderSize(conn, 64<<10)
		c.writeMu.Unlock()
		return func() { _ = conn.Close() }, nil
	}
	wsURL := c.url
	if c.binary.Load() {
		wsURL = BinaryWSURL // the binary transport lives on a different host
	}
	conn, _, err := maxDialer.Dial(wsURL, http_headers(c.origin, "", ""))
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", wsURL, err)
	}
	c.writeMu.Lock()
	c.conn = conn
	c.writeMu.Unlock()
	return func() { _ = conn.Close() }, nil
}

// closeConn drops the live socket (whichever transport) so the read loop fails
// and the session unwinds.
func (c *Client) closeConn() {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close()
	}
	if c.tcpConn != nil {
		_ = c.tcpConn.Close()
	}
}

// noteBinaryFailure counts binary sessions that died before LOGIN completed;
// after two in a row the client falls back to the JSON transport for good
// (until restart), so a server-side change to the binary protocol can't keep
// the bridge offline — it just loses full polls again.
func (c *Client) noteBinaryFailure(err error) {
	// A server error reply means the binary frame was understood (e.g. an
	// expired token) — only silent drops/timeouts point at the transport.
	if !c.binary.Load() || strings.Contains(err.Error(), "server error") {
		return
	}
	c.binFails++
	log.Printf("[max] binary transport: session failed before LOGIN (%d/2): %v", c.binFails, err)
	if c.binFails >= 2 {
		c.binary.Store(false)
		log.Printf("[max] binary transport: FALLING BACK TO JSON — polls will arrive as placeholders until restart. Please send this log to the developer.")
	}
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
	changed := c.connected != connected
	c.connected = connected
	if lastErr != "" {
		c.lastErr = lastErr
	}
	c.statMu.Unlock()
	if changed && c.onStatus != nil {
		c.onStatus(connected)
	}
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
	var r struct {
		Link string `json:"link"`
	}
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
	if c.tcp {
		ver := c.appVersion
		if ver == "" {
			ver = TCPVersion()
		}
		log.Printf("[max] INIT as DESKTOP over TCP (%s) appVersion=%s", TCPAddr, ver)
		_, err := c.request(OpInit, map[string]any{
			"userAgent":       tcpUserAgent(ver),
			"deviceId":        c.deviceID,
			"clientSessionId": time.Now().UnixNano() % 1000,
		}, 15*time.Second)
		return err
	}
	ua := defaultUserAgent()
	log.Printf("[max] INIT appVersion=%q deviceType=%s binary=%t", ua.AppVersion, ua.DeviceType, c.binary.Load())
	_, err := c.request(OpInit, map[string]any{
		"userAgent": ua,
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
	var vr struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(resp.Payload, &vr); err != nil {
		return err
	}
	enter, err := c.request(OpCodeEnter, map[string]any{"code": c.readCode(), "token": vr.Token}, 30*time.Second)
	if err != nil {
		return err
	}

	type loginStruct struct {
		Token string `json:"token"`
	}
	type pLoginStruct struct {
		Login loginStruct `json:"LOGIN"`
	}
	type ceStruct struct {
		TokenAttrs pLoginStruct `json:"tokenAttrs"`
	}

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
	req := map[string]any{
		"interactive":  true,
		"token":        c.token,
		"chatsCount":   100,
		"chatsSync":    100,
		"contactsSync": 0,
		"presenceSync": 0,
		"draftsSync":   0,
	}
	if c.tcp {
		// The desktop endpoint answers an internal error above 50 chats.
		req = map[string]any{"token": c.token, "interactive": true, "chatsCount": 50}
	}
	resp, err := c.request(OpLogin, req, 30*time.Second)
	if err != nil {
		return err
	}
	if c.tcp {
		// A successful LOGIN only sometimes rotates the session token; when it
		// does, the new one must be kept or the next start is rejected.
		if tok := findLongToken(resp.Payload); tok != "" && tok != c.token {
			c.token = tok
			if c.saveDesktopToken != nil {
				c.saveDesktopToken(c.deviceID, tok)
			}
		}
	}
	type idStruct struct {
		ID json.Number `json:"id"`
	}
	type TitleStruct struct {
		ID    json.Number `json:"id"`
		Title string      `json:"title"`
	}
	type profileStruct struct {
		Contact idStruct `json:"contact"`
	}
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

// ChatIcon returns the chat's avatar URL (baseIconUrl / picture.url) or "".
func (c *Client) ChatIcon(chatID int64) string {
	resp, err := c.request(OpGetChatInfo, map[string]any{"chatId": chatID}, 10*time.Second)
	if err != nil {
		return ""
	}
	var r struct {
		Chat struct {
			BaseIconURL string `json:"baseIconUrl"`
			Picture     struct {
				URL string `json:"url"`
			} `json:"picture"`
		} `json:"chat"`
	}
	if json.Unmarshal(resp.Payload, &r) != nil {
		return ""
	}
	if r.Chat.BaseIconURL != "" {
		return r.Chat.BaseIconURL
	}
	return r.Chat.Picture.URL
}

// History returns up to count recent messages of a chat (oldest-first), enriched
// with names, for backfilling a freshly created topic.
func (c *Client) History(chatID int64, count int) []IncomingMessage {
	if count <= 0 {
		return nil
	}
	resp, err := c.request(OpGetHistory, map[string]any{
		"chatId": chatID, "forward": 0, "backward": count,
		"backwardTime": 0, "forwardTime": 0, "getChat": false,
		"from": time.Now().UnixMilli(), "getMessages": true, "interactive": false,
	}, 20*time.Second)
	if err != nil {
		return nil
	}
	var r struct {
		Messages []msgBody `json:"messages"`
	}
	if json.Unmarshal(resp.Payload, &r) != nil {
		return nil
	}
	out := make([]IncomingMessage, 0, len(r.Messages))
	for _, m := range r.Messages {
		text, att, forwarded, replyToID := parseLink(m.Link, m.Text, toAttachments(m.Attaches))
		if strings.TrimSpace(text) == "" && len(att) == 0 {
			continue
		}
		sid := numToInt64(m.Sender)
		im := IncomingMessage{
			MessageID:   m.ID.String(),
			ChatID:      chatID,
			SenderID:    sid,
			Text:        text,
			Attachments: att,
			Outgoing:    sid != 0 && sid == c.selfID,
			Forwarded:   forwarded,
			ReplyToID:   replyToID,
			Time:        m.Time,
		}
		// Attachment URLs are NOT resolved here: a resync re-reads history on
		// every reconnect and most of it is already delivered, so the caller
		// resolves (ResolveAttachments) only what it actually posts.
		c.enrich(&im)
		out = append(out, im)
	}
	// Sort chronologically (oldest-first) by the message's own Time rather than
	// assuming a fixed wire order — GET_HISTORY's actual ordering here is
	// reverse-engineered and unconfirmed (a blind reverse-in-place previously
	// used here produced newest-first output live, backwards from what a
	// backfill needs), so sorting by real timestamp is the only way to be sure
	// regardless of whatever order the server happens to send.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time < out[j].Time })
	return out
}

// SendFile uploads a file to MAX storage and posts it as a message attachment.
// Returns the created message id (for echo dedupe).
func (c *Client) SendFile(chatID int64, name string, data []byte, caption string) (string, error) {
	return c.SendFileReply(chatID, name, data, caption, "")
}

// SendFileReply is SendFile, optionally posted as a reply to an existing MAX
// message (replyToMaxID == "" means no reply link).
func (c *Client) SendFileReply(chatID int64, name string, data []byte, caption string, replyToMaxID string) (string, error) {
	if name == "" {
		name = "file"
	}
	ext := ""
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		ext = strings.ToLower(name[i+1:])
	}
	resp, err := c.request(OpFileUpload, map[string]any{
		"name": name, "size": len(data), "ext": ext, "count": 1,
	}, 15*time.Second)
	if err != nil {
		return "", err
	}
	var up struct {
		Info []struct {
			URL    string      `json:"url"`
			FileID json.Number `json:"fileId"` // numeric on the wire
			Token  string      `json:"token"`
		} `json:"info"`
	}
	if json.Unmarshal(resp.Payload, &up) != nil || len(up.Info) == 0 {
		log.Printf("[max] FILE_UPLOAD raw response: %s", string(resp.Payload))
		return "", fmt.Errorf("no upload slot in FILE_UPLOAD response")
	}
	slot := up.Info[0]
	// Drain any stale signal, upload, then wait for NOTIF_ATTACH (the server
	// confirms the upload is attached to our session before MSG_SEND).
	select {
	case <-c.attachReady:
	default:
	}
	if err := httpUpload(slot.URL, name, data); err != nil {
		return "", err
	}
	select {
	case <-c.attachReady:
	case <-time.After(15 * time.Second):
		// proceed anyway; some uploads don't emit a push
	}
	return c.sendAttachMessage(chatID, caption, replyToMaxID, map[string]any{
		"_type": "FILE", "fileId": slot.FileID, "token": slot.Token,
		"name": name, "size": len(data),
	})
}

// uploadSlot is the common {url, id, token} shape returned by the
// PHOTO/VIDEO/FILE upload opcodes (the id field name varies by type).
type uploadSlot struct {
	URL   string
	ID    int64
	Token string
}

// parseUploadSlot decodes the {"info":[{"url":...,"videoId"|"fileId":N,"token":...}]}
// response shape shared by the VIDEO_UPLOAD/FILE_UPLOAD opcodes.
func parseUploadSlot(payload json.RawMessage) (uploadSlot, error) {
	var up struct {
		Info []struct {
			URL     string      `json:"url"`
			VideoID json.Number `json:"videoId"`
			FileID  json.Number `json:"fileId"`
			Token   string      `json:"token"`
		} `json:"info"`
	}
	if json.Unmarshal(payload, &up) != nil || len(up.Info) == 0 {
		return uploadSlot{}, fmt.Errorf("no upload slot in response: %s", string(payload))
	}
	s := up.Info[0]
	id := numToInt64(s.VideoID)
	if id == 0 {
		id = numToInt64(s.FileID)
	}
	return uploadSlot{URL: s.URL, ID: id, Token: s.Token}, nil
}

// photoIDFromUploadURL extracts the "photoIds" query parameter MAX embeds in
// the PHOTO_UPLOAD (opcode 80) response URL — the HTTP upload response later
// keys its token map by this same id.
func photoIDFromUploadURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	ids := u.Query()["photoIds"]
	if len(ids) == 0 || ids[0] == "" {
		return "", fmt.Errorf("upload url has no photoIds query param: %s", rawURL)
	}
	return ids[0], nil
}

// SendPhoto uploads a photo to MAX via the dedicated photo-upload flow
// (opcode 80: request an upload slot -> HTTP multipart POST -> the HTTP
// response body itself, not a socket push, carries the photo token) and posts
// it as a PHOTO attachment — unlike SendFile's generic FILE attach, this
// shows up inline like a real photo, matching the web/mobile clients.
func (c *Client) SendPhoto(chatID int64, data []byte, caption string, replyToMaxID string) (string, error) {
	resp, err := c.request(OpImageUploadURL, map[string]any{
		"count": 1, "type": 0, "uploaderType": 0, "profile": false,
	}, 15*time.Second)
	if err != nil {
		return "", err
	}
	var up struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(resp.Payload, &up) != nil || up.URL == "" {
		return "", fmt.Errorf("no upload url in PHOTO_UPLOAD response: %s", string(resp.Payload))
	}
	// Newer MAX builds hand out an opaque upload URL without a photoIds query
	// param; the upload response is then read by whatever id it carries.
	photoID, _ := photoIDFromUploadURL(up.URL)
	token, err := httpUploadPhoto(up.URL, data, photoID)
	if err != nil {
		return "", err
	}
	return c.sendAttachMessage(chatID, caption, replyToMaxID, map[string]any{
		"_type": "PHOTO", "photoToken": token,
	})
}

// SendVideo uploads a video to MAX via VIDEO_UPLOAD (opcode 82, type=0) and
// posts it as a VIDEO attachment. Like SendFile, the server confirms the
// upload asynchronously via NOTIF_ATTACH (opcode 136) — reuses attachReady.
func (c *Client) SendVideo(chatID int64, name string, data []byte, caption string, replyToMaxID string) (string, error) {
	resp, err := c.request(OpVideoUpload, map[string]any{
		"count": 1, "type": 0, "uploaderType": 0, "profile": false,
	}, 15*time.Second)
	if err != nil {
		return "", err
	}
	slot, err := parseUploadSlot(resp.Payload)
	if err != nil {
		return "", err
	}
	select {
	case <-c.attachReady:
	default:
	}
	if err := httpUploadRaw(slot.URL, name, data, ""); err != nil {
		return "", err
	}
	select {
	case <-c.attachReady:
	case <-time.After(30 * time.Second):
		// proceed anyway; matches SendFile's tolerant behavior
	}
	return c.sendAttachMessage(chatID, caption, replyToMaxID, map[string]any{
		"_type": "VIDEO", "videoId": slot.ID, "token": slot.Token, "videoType": 0,
	})
}

// SendVoice uploads a voice note to MAX via VIDEO_UPLOAD (opcode 82, type=2,
// uploaderType=1) and posts it as an AUDIO attachment. Telegram voice notes
// are already OGG/Opus — the only format MAX's own client accepts — so no
// transcoding is needed. Unlike video, MAX attaches a voice note right after
// the HTTP upload with no NOTIF_ATTACH wait (matches the reference client).
func (c *Client) SendVoice(chatID int64, data []byte, durationMS int64, caption string, replyToMaxID string) (string, error) {
	resp, err := c.request(OpVideoUpload, map[string]any{
		"count": 1, "type": 2, "uploaderType": 1, "profile": false,
	}, 15*time.Second)
	if err != nil {
		return "", err
	}
	slot, err := parseUploadSlot(resp.Payload)
	if err != nil {
		return "", err
	}
	if err := httpUploadRaw(slot.URL, "voice.ogg", data, "application/octet-stream"); err != nil {
		return "", err
	}
	// "wave" is a waveform preview the real client always attaches; MAX does
	// not appear to validate its content, so a fixed all-zero placeholder
	// (matching what other reverse-engineered clients send when they have no
	// real waveform data) is used rather than computing one.
	wave := base64.StdEncoding.EncodeToString(make([]byte, 80))
	return c.sendAttachMessage(chatID, caption, replyToMaxID, map[string]any{
		"_type": "AUDIO", "token": slot.Token, "duration": durationMS, "wave": wave,
	})
}

// sendAttachMessage posts a single-attachment message (photo/video/voice/file)
// via MSG_SEND, optionally as a reply, and returns the created message id.
func (c *Client) sendAttachMessage(chatID int64, caption, replyToMaxID string, attach map[string]any) (string, error) {
	return c.sendAttach(chatID, caption, replyToMaxID, attach)
}

// sendAttach is sendAttachMessage with the text as `any`, so attach-only
// messages (location, contact) can send a JSON null — MAX wants null there,
// not an empty string.
func (c *Client) sendAttach(chatID int64, text any, replyToMaxID string, attach map[string]any) (string, error) {
	msg := map[string]any{
		"text": text, "cid": time.Now().UnixNano(), "elements": []any{},
		"attaches": []map[string]any{attach},
	}
	if replyToMaxID != "" {
		msg["link"] = map[string]any{"type": "REPLY", "messageId": msgIDArg(replyToMaxID)}
	}
	resp, err := c.request(OpMsgSend, map[string]any{
		"chatId": chatID, "message": msg, "notify": true,
	}, 20*time.Second)
	if err != nil {
		return "", err
	}
	var r struct {
		Message struct {
			ID json.Number `json:"id"`
		} `json:"message"`
	}
	if json.Unmarshal(resp.Payload, &r) == nil && r.Message.ID != "" {
		return r.Message.ID.String(), nil
	}
	return "", nil
}

// msgIDArg encodes a MAX message id for a request payload as a JSON number
// when possible (most opcodes — reactions, edit, delete, reply links —
// declare message_id as a strict int on the wire) and falls back to the raw
// string only if it isn't numeric.
func msgIDArg(id string) any {
	if n, err := strconv.ParseInt(id, 10, 64); err == nil {
		return n
	}
	return id
}

// resolveAttachments fills in URL for any attachment that only carries a lazy
// Resolve* id (a VIDEO/FILE/AUDIO attach MAX sent without a direct URL). MUST
// be called off the read loop — it makes MAX requests and blocks for replies.
// ResolveAttachments fills in the media URLs of a message returned by History
// (which leaves them unresolved). Makes MAX requests; never call it on the
// read loop.
func (c *Client) ResolveAttachments(m *IncomingMessage) {
	c.resolveAttachments(m.ChatID, m.MessageID, m.Attachments)
}

func (c *Client) resolveAttachments(chatID int64, messageID string, atts []Attachment) {
	for i := range atts {
		if atts[i].URL != "" || atts[i].ResolveKind == "" {
			continue
		}
		var resolved string
		switch atts[i].ResolveKind {
		case "video":
			resolved = c.getVideoURL(chatID, messageID, atts[i].ResolveID)
		case "file":
			resolved = c.getFileURL(chatID, messageID, atts[i].ResolveID)
		case "audio":
			resolved = c.getAudioURL(chatID, messageID, atts[i].ResolveID)
		}
		if resolved != "" {
			atts[i].URL = resolved
		}
	}
}

// getVideoURL resolves a playable URL for a VIDEO attach via VIDEO_PLAY
// (opcode 83). Mirrors the confirmed client behavior: prefer the
// highest-numbered "MP4_<n>" key, fall back to dynamicUrl/dynamic_url, then
// EXTERNAL (a web-player link — still useful as an "open to watch" fallback).
func (c *Client) getVideoURL(chatID int64, messageID string, videoID int64) string {
	resp, err := c.request(OpVideoPlay, map[string]any{
		"chatId": chatID, "messageId": msgIDArg(messageID), "videoId": videoID,
	}, 15*time.Second)
	if err != nil {
		log.Printf("[max] VIDEO_PLAY failed for videoId=%d: %v", videoID, err)
		return ""
	}
	var r map[string]any
	if json.Unmarshal(resp.Payload, &r) != nil {
		return ""
	}
	bestQuality, bestURL := -1, ""
	for k, v := range r {
		s, ok := v.(string)
		if !ok || s == "" {
			continue
		}
		up := strings.ToUpper(k)
		if !strings.HasPrefix(up, "MP4_") {
			continue
		}
		q, err := strconv.Atoi(strings.TrimPrefix(up, "MP4_"))
		if err != nil || q <= bestQuality {
			continue
		}
		bestQuality, bestURL = q, s
	}
	if bestURL != "" {
		return bestURL
	}
	for _, key := range []string{"dynamicUrl", "dynamic_url", "EXTERNAL"} {
		if v, ok := r[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// getFileURL resolves a temporary download URL for a FILE attach via
// FILE_DOWNLOAD (opcode 88): {chatId,messageId,fileId} -> {url,unsafe}.
func (c *Client) getFileURL(chatID int64, messageID string, fileID int64) string {
	resp, err := c.request(OpFileDownload, map[string]any{
		"chatId": chatID, "messageId": msgIDArg(messageID), "fileId": fileID,
	}, 15*time.Second)
	if err != nil {
		log.Printf("[max] FILE_DOWNLOAD failed for fileId=%d: %v", fileID, err)
		return ""
	}
	var r struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(resp.Payload, &r) != nil {
		return ""
	}
	return r.URL
}

// getAudioURL is a best-effort attempt to resolve a voice-note URL via
// AUDIO_PLAY (opcode 301). This opcode is confirmed to exist in the
// reverse-engineered opcode table, but its request/response shape is NOT
// documented anywhere and no known client actually calls it. This mirrors the
// well-confirmed VIDEO_PLAY/FILE_DOWNLOAD request shape
// ({chatId,messageId,audioId}) and accepts a flat {"url":...} response or the
// MP4_*/EXTERNAL shape VIDEO_PLAY uses, in case the server treats audio the
// same way. If the server rejects the opcode or returns nothing usable, the
// raw response is logged so a real sample can be captured to fix this.
func (c *Client) getAudioURL(chatID int64, messageID string, audioID int64) string {
	resp, err := c.request(OpAudioPlay, map[string]any{
		"chatId": chatID, "messageId": msgIDArg(messageID), "audioId": audioID,
	}, 15*time.Second)
	if err != nil {
		log.Printf("[max] AUDIO_PLAY (best-effort) failed for audioId=%d: %v", audioID, err)
		return ""
	}
	var r map[string]any
	if json.Unmarshal(resp.Payload, &r) != nil {
		return ""
	}
	// Confirmed live: the response is {"opus":"https://a.oneme.ru/audio?..."}.
	for _, key := range []string{"url", "URL", "opus"} {
		if v, ok := r[key].(string); ok && v != "" {
			return v
		}
	}
	for k, v := range r {
		if strings.HasPrefix(strings.ToUpper(k), "MP4_") {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	if v, ok := r["EXTERNAL"].(string); ok && v != "" {
		return v
	}
	log.Printf("[max] AUDIO_PLAY (best-effort) returned no usable URL for audioId=%d, raw=%s", audioID, string(resp.Payload))
	return ""
}

// AddReaction sets/replaces our reaction on a message via MSG_REACTION
// (opcode 178).
func (c *Client) AddReaction(chatID int64, messageID string, reaction string) error {
	_, err := c.request(OpMsgReaction, map[string]any{
		"chatId": chatID, "messageId": msgIDArg(messageID),
		"reaction": map[string]any{"reactionType": "EMOJI", "id": reaction},
	}, 15*time.Second)
	return err
}

// RemoveReaction removes our reaction via MSG_CANCEL_REACTION (opcode 179).
func (c *Client) RemoveReaction(chatID int64, messageID string) error {
	_, err := c.request(OpMsgCancelReaction, map[string]any{
		"chatId": chatID, "messageId": msgIDArg(messageID),
	}, 15*time.Second)
	return err
}

// EditMessage edits the text of our own MAX message via MSG_EDIT (opcode 67).
func (c *Client) EditMessage(chatID int64, messageID string, text string) error {
	_, err := c.request(OpMsgEdit, map[string]any{
		"chatId": chatID, "messageId": msgIDArg(messageID),
		"text": text, "elements": []any{}, "attachments": []any{},
	}, 15*time.Second)
	return err
}

// DeleteMessage deletes a message via MSG_DELETE (opcode 66). forMe=false
// deletes it for everyone (requires it being our own message or admin rights
// in the chat — MAX, like any messenger, rejects deleting others' messages).
func (c *Client) DeleteMessage(chatID int64, messageID string, forMe bool) error {
	_, err := c.request(OpMsgDelete, map[string]any{
		"chatId": chatID, "messageIds": []any{msgIDArg(messageID)}, "forMe": forMe,
	}, 15*time.Second)
	return err
}

// httpUpload POSTs a file to a MAX storage URL via multipart/form-data.
func httpUpload(url, name string, data []byte) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", name)
	if err != nil {
		return err
	}
	if _, err := fw.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	req, err := http.NewRequest("POST", url, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("upload http %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// httpUploadPhoto POSTs photo bytes to a MAX PHOTO_UPLOAD URL (multipart,
// field "file") and returns the token MAX assigns, read from the JSON BODY of
// this same HTTP response (unlike SendFile's generic upload, the photo token
// never arrives via a websocket push).
func httpUploadPhoto(uploadURL string, data []byte, photoID string) (string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", "image.jpg")
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(data); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequest("POST", uploadURL, &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("photo upload http %d: %s", resp.StatusCode, string(body))
	}
	var pr struct {
		Photos map[string]struct {
			Token string `json:"token"`
		} `json:"photos"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return "", fmt.Errorf("photo upload: bad response json: %s", string(body))
	}
	if p, ok := pr.Photos[photoID]; ok && p.Token != "" {
		return p.Token, nil
	}
	// A response keyed by an id that doesn't exactly match the upload URL's
	// photoIds value would be surprising, but fall back to any token present
	// rather than failing outright.
	for _, p := range pr.Photos {
		if p.Token != "" {
			return p.Token, nil
		}
	}
	return "", fmt.Errorf("photo upload response has no token: %s", string(body))
}

// httpUploadRaw POSTs raw bytes (not multipart) to a MAX VIDEO_UPLOAD slot
// URL with a Content-Range header spanning the whole file in one shot,
// matching the reference client's behavior for video/voice uploads.
// contentType, if non-empty, overrides the default octet-stream.
func httpUploadRaw(uploadURL, name string, data []byte, contentType string) error {
	if len(data) == 0 {
		return fmt.Errorf("empty upload body")
	}
	req, err := http.NewRequest("POST", uploadURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	req.Header.Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(data)-1, len(data)))
	req.Header.Set("Content-Length", strconv.Itoa(len(data)))
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("upload http %d: %s", resp.StatusCode, string(b))
	}
	return nil
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

func (c *Client) SendMessage(chatID int64, text string) (string, error) {
	return c.SendMessageReply(chatID, text, "")
}

// SendMessageReply sends a text message, optionally as a reply to an
// existing MAX message in the same chat (replyToMaxID == "" means no reply
// link — behaves exactly like SendMessage).
func (c *Client) SendMessageReply(chatID int64, text string, replyToMaxID string) (string, error) {
	// MAX expects the message wrapped: {chatId, message:{text,cid,elements,attaches}, notify}
	// (matches the web client / MaxBridge). A flat {text} payload is ignored.
	msg := map[string]any{
		"text": text, "cid": time.Now().UnixNano(), "elements": []any{}, "attaches": []any{},
	}
	if replyToMaxID != "" {
		msg["link"] = map[string]any{"type": "REPLY", "messageId": msgIDArg(replyToMaxID)}
	}
	resp, err := c.request(OpMsgSend, map[string]any{
		"chatId": chatID, "message": msg, "notify": true,
	}, 15*time.Second)
	if err != nil {
		return "", err
	}
	// The response echoes the stored message; return its id so the caller can
	// mark it seen and skip the self-echo push that follows (avoids a duplicate).
	var r struct {
		Message struct {
			ID json.Number `json:"id"`
		} `json:"message"`
	}
	if json.Unmarshal(resp.Payload, &r) == nil && r.Message.ID != "" {
		return r.Message.ID.String(), nil
	}
	return "", nil
}

func (c *Client) request(opcode int, payload any, timeout time.Duration) (*Frame, error) {
	c.seqMu.Lock()
	c.seq++
	if c.seq > 0xFFFF { // the binary header carries seq as u16
		c.seq = 1
	}
	seq := c.seq
	c.seqMu.Unlock()
	ch := make(chan *Frame, 1)
	c.pendMu.Lock()
	c.pending[seq] = ch
	c.pendOp[seq] = opcode
	c.pendMu.Unlock()
	forget := func() {
		c.pendMu.Lock()
		delete(c.pending, seq)
		delete(c.pendOp, seq)
		c.pendMu.Unlock()
	}
	raw, _ := json.Marshal(payload)
	f := Frame{Ver: ProtocolVersion, Cmd: CmdRequest, Seq: seq, Opcode: opcode, Payload: raw}
	if err := c.writeFrame(f); err != nil {
		forget()
		return nil, err
	}
	select {
	case res := <-ch:
		if res.Cmd == CmdError {
			return nil, fmt.Errorf("server error (opcode %d): %s", opcode, string(res.Payload))
		}
		return res, nil
	case <-time.After(timeout):
		forget()
		return nil, fmt.Errorf("timeout waiting for reply to opcode %d (seq %d)", opcode, seq)
	}
}

func (c *Client) writeFrame(f Frame) error {
	if c.tcp {
		raw, err := encodeBinFrameVer(f, tcpProtocolVersion)
		if err != nil {
			return err
		}
		c.writeMu.Lock()
		defer c.writeMu.Unlock()
		if c.tcpConn == nil {
			return fmt.Errorf("not connected")
		}
		_ = c.tcpConn.SetWriteDeadline(time.Now().Add(20 * time.Second))
		_, err = c.tcpConn.Write(raw)
		return err
	}
	msgType := websocket.TextMessage
	raw, err := json.Marshal(f)
	if c.binary.Load() {
		msgType = websocket.BinaryMessage
		raw, err = encodeBinFrame(f)
	}
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}
	return c.conn.WriteMessage(msgType, raw)
}

// takePending finds the request a reply answers: by echoed seq, or — MAX does
// not always echo it — the oldest in-flight request with the same opcode.
func (c *Client) takePending(f Frame) chan *Frame {
	c.pendMu.Lock()
	defer c.pendMu.Unlock()
	seq := f.Seq
	ch := c.pending[seq]
	if ch == nil {
		best := -1
		for s, op := range c.pendOp {
			if op == f.Opcode && (best < 0 || s < best) {
				best = s
			}
		}
		if best < 0 {
			return nil
		}
		seq, ch = best, c.pending[best]
	}
	delete(c.pending, seq)
	delete(c.pendOp, seq)
	return ch
}

// nextFrame reads and decodes the next frame off whichever transport is live.
// A decode problem is returned as skip=true (the connection is fine); an error
// with skip=false ends the session.
func (c *Client) nextFrame() (f Frame, skip bool, err error) {
	if c.tcp {
		_ = c.tcpConn.SetReadDeadline(time.Now().Add(tcpReadIdle))
		data, err := readTCPRaw(c.tcpR)
		if err != nil {
			return Frame{}, false, err
		}
		f, err = decodeBinFrame(data)
		if err != nil {
			log.Printf("[max] bad tcp frame: %v", err)
			return Frame{}, true, nil
		}
		return f, false, nil
	}
	msgType, data, err := c.conn.ReadMessage()
	if err != nil {
		return Frame{}, false, err
	}
	if msgType == websocket.BinaryMessage {
		if f, err = decodeBinFrame(data); err != nil {
			log.Printf("[max] bad binary frame: %v", err)
			return Frame{}, true, nil
		}
		return f, false, nil
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return Frame{}, true, nil
	}
	return f, false, nil
}

func (c *Client) readLoop() error {
	for {
		f, skip, err := c.nextFrame()
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		if f.Cmd == CmdAck || f.Cmd == CmdError {
			if ch := c.takePending(f); ch != nil {
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
	if len(s) > 4000 {
		s = s[:4000] + "..."
	}
	c.debugLog = append(c.debugLog, fmt.Sprintf("OPCODE=%d PAYLOAD=%s", f.Opcode, s))
	c.debugMu.Unlock()

	if f.Opcode == OpPing {
		_ = c.writeFrame(c.pingFrame())
		return
	}
	if f.Opcode == OpPushAttach { // NOTIF_ATTACH: an upload finished
		select {
		case c.attachReady <- struct{}{}:
		default:
		}
		return
	}
	if f.Opcode == OpPushReaction || f.Opcode == OpPushReactionFlat { // someone (re)reacted
		c.tryReaction(f)
		return
	}
	if f.Opcode == OpPushMsgDelete { // a message was deleted
		c.tryDelete(f)
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
	if pm.Message == nil {
		return false
	}
	chatID := numToInt64(pm.ChatID)
	// A chat deleted in MAX arrives as a CONTROL "Чат закрыт" marker — hand it
	// on so the bridge can drop the chat's topic.
	if chatID != 0 && isChatClosed(pm.Message.Attaches) {
		select {
		case c.deliver <- IncomingMessage{ChatID: chatID, ChatClosed: true}:
		default:
			log.Printf("[max] deliver queue full; dropping chat-closed marker for chat %d", chatID)
		}
		return true
	}
	// A repeat push of an existing id with status REMOVED is a deletion.
	if chatID != 0 && pm.Message.Status == "REMOVED" && pm.Message.ID != "" {
		select {
		case c.deletes <- IncomingDelete{ChatID: chatID, MessageIDs: []string{pm.Message.ID.String()}}:
		default:
			log.Printf("[max] delete queue full; dropping REMOVED push for chat %d", chatID)
		}
		return true
	}
	text, attachments, forwarded, replyToID := parseLink(pm.Message.Link, pm.Message.Text, toAttachments(pm.Message.Attaches))
	// Skip pure service pushes (no text AND no media); keep media-only messages.
	if strings.TrimSpace(text) == "" && len(attachments) == 0 {
		return false
	}
	senderID := numToInt64(pm.Message.Sender)
	if chatID == 0 {
		return false
	}
	if c.onMessage == nil {
		return true
	}
	msg := IncomingMessage{
		MessageID:   pm.Message.ID.String(),
		ChatID:      chatID,
		SenderID:    senderID,
		Text:        text,
		Attachments: attachments,
		Outgoing:    senderID != 0 && senderID == c.selfID,
		Forwarded:   forwarded,
		ReplyToID:   replyToID,
		Time:        pm.Message.Time,
	}
	// Hand off to the delivery worker (never block or make MAX requests on the
	// read loop — that would deadlock, since replies arrive on this same loop).
	select {
	case c.deliver <- msg:
	default:
		log.Printf("[max] deliver queue full; dropping message %s", msg.MessageID)
	}
	return true
}

// tryReaction parses a NOTIF_REACTION push and queues it for delivery.
func (c *Client) tryReaction(f Frame) {
	var rp reactionPush
	if err := json.Unmarshal(f.Payload, &rp); err != nil {
		return
	}
	chatID := numToInt64(rp.ChatID)
	if chatID == 0 || c.onReaction == nil {
		return
	}
	counters, total := rp.Counters, rp.TotalCount
	if rp.ReactionInfo != nil {
		counters, total = rp.ReactionInfo.Counters, rp.ReactionInfo.TotalCount
	}
	r := IncomingReaction{
		ChatID:     chatID,
		MessageID:  rp.MessageID.String(),
		Counters:   counters,
		TotalCount: total,
	}
	log.Printf("[max] reaction push (opcode %d): chat=%d msg=%s counters=%v", f.Opcode, chatID, r.MessageID, counters)
	select {
	case c.reactions <- r:
	default:
		log.Printf("[max] reaction queue full; dropping reaction on message %s", r.MessageID)
	}
}

// reactionLoop hands queued reactions to onReaction, off the read loop (the
// bridge's Telegram call must never block frame reads).
func (c *Client) reactionLoop() {
	for r := range c.reactions {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("[max] onReaction panic: %v", rec)
				}
			}()
			c.onReaction(r)
		}()
	}
}

// tryDelete parses a NOTIF_MSG_DELETE push and queues it for delivery.
func (c *Client) tryDelete(f Frame) {
	var dp deletePush
	if err := json.Unmarshal(f.Payload, &dp); err != nil {
		return
	}
	chatID := numToInt64(dp.ChatID)
	if chatID == 0 || c.onDelete == nil {
		return
	}
	var ids []string
	if dp.MessageID != "" {
		ids = append(ids, dp.MessageID.String())
	}
	for _, n := range dp.MessageIDs {
		if n != "" {
			ids = append(ids, n.String())
		}
	}
	if len(ids) == 0 {
		return
	}
	select {
	case c.deletes <- IncomingDelete{ChatID: chatID, MessageIDs: ids}:
	default:
		log.Printf("[max] delete queue full; dropping delete event for chat %d", chatID)
	}
}

// deleteLoop hands queued delete events to onDelete, off the read loop.
func (c *Client) deleteLoop() {
	for d := range c.deletes {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("[max] onDelete panic: %v", rec)
				}
			}()
			c.onDelete(d)
		}()
	}
}

// deliverLoop enriches queued messages (chat title, sender name — via MAX
// lookups that are safe here, off the read loop) and hands them to onMessage,
// preserving order.
func (c *Client) deliverLoop() {
	for msg := range c.deliver {
		if !msg.ChatClosed { // a closed chat has nothing to resolve or look up
			c.resolveAttachments(msg.ChatID, msg.MessageID, msg.Attachments)
			c.enrich(&msg)
		}
		if c.onMessage != nil {
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("[max] onMessage panic: %v", r)
					}
				}()
				c.onMessage(msg)
			}()
		}
	}
}

func (c *Client) enrich(msg *IncomingMessage) {
	c.chatMu.RLock()
	title := c.chatName[msg.ChatID]
	sender := c.userName[msg.SenderID]
	c.chatMu.RUnlock()

	if title == "" {
		if t := c.fetchChatTitle(msg.ChatID); t != "" {
			title = t
			c.chatMu.Lock()
			c.chatName[msg.ChatID] = t
			c.chatMu.Unlock()
		} else {
			title = fmt.Sprintf("Chat %d", msg.ChatID)
		}
	}
	if msg.Outgoing {
		sender = "Вы"
	} else if sender == "" && msg.SenderID != 0 {
		if n := c.fetchUserName(msg.SenderID); n != "" {
			sender = n
			c.chatMu.Lock()
			c.userName[msg.SenderID] = n
			c.chatMu.Unlock()
		} else {
			sender = fmt.Sprintf("User %d", msg.SenderID)
		}
	}
	msg.ChatTitle = title
	msg.SenderName = sender
}

func (c *Client) heartbeat(done chan struct{}) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			_ = c.writeFrame(c.pingFrame())
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

// pingFrame is a keep-alive PING: the web transports send {"interactive":true},
// the desktop TCP client a bare, payload-less frame.
func (c *Client) pingFrame() Frame {
	f := Frame{Ver: ProtocolVersion, Cmd: CmdRequest, Seq: 0, Opcode: OpPing}
	if !c.tcp {
		f.Payload = json.RawMessage(`{"interactive":true}`)
	}
	return f
}
