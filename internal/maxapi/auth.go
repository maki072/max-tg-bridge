package maxapi

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Browserless sign-in. AuthSession drives the same flow the MAX desktop client
// uses — START_AUTH (SMS) → CHECK_CODE → optionally CHECK_PASSWORD (2FA) →
// LOGIN — over the TCP transport, with the steps split so a chat bot can ask
// the person for the phone, the code and the password one message at a time.
// The sequence and payload shapes follow the Telemax bridge
// (github.com/Trollobot/Telemax, AGPL-3.0); the implementation is ours.

const (
	// OpCheckPassword is the second factor after CHECK_CODE: {trackId, password}.
	OpCheckPassword = 115

	sessionTokenLen = 663 // login/session tokens are this long (seen live by Telemax)
)

var tokenAlphabet = regexp.MustCompile(`^[A-Za-z0-9\-._~+/]+=*$`)

// snapshotKeys are the account-snapshot branches of a LOGIN reply; user content
// (a channel post, a description) lives there and must never be mistaken for a
// token.
var snapshotKeys = map[string]bool{
	"profile": true, "chats": true, "messages": true, "contacts": true,
	"presence": true, "config": true, "updates": true,
}

// findLongToken returns the first token-shaped string (exactly 663 chars of the
// bearer-token alphabet; failing that, 663 chars of non-text) outside the
// snapshot branches of a reply, or "".
func findLongToken(payload json.RawMessage) string {
	var v any
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return ""
	}
	if s := walkToken(v, func(s string) bool { return len(s) == sessionTokenLen && tokenAlphabet.MatchString(s) }); s != "" {
		return s
	}
	return walkToken(v, func(s string) bool {
		return len(s) == sessionTokenLen && !strings.ContainsAny(s, " \t\r\n") && !hasCyrillic(s)
	})
}

func hasCyrillic(s string) bool {
	for _, r := range s {
		if r >= 0x0400 && r <= 0x04FF {
			return true
		}
	}
	return false
}

func walkToken(v any, ok func(string) bool) string {
	switch x := v.(type) {
	case string:
		if ok(x) {
			return x
		}
	case map[string]any:
		for k, child := range x {
			if snapshotKeys[k] {
				continue
			}
			if s := walkToken(child, ok); s != "" {
				return s
			}
		}
	case []any:
		for _, child := range x {
			if s := walkToken(child, ok); s != "" {
				return s
			}
		}
	}
	return ""
}

// DescribeError turns a "server error (opcode N): {json}" error from the client
// into the server's own human-readable message when it carries one.
func DescribeError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if i := strings.Index(s, "{"); i >= 0 {
		var p map[string]any
		if json.Unmarshal([]byte(s[i:]), &p) == nil {
			for _, k := range []string{"localizedMessage", "message", "title", "error"} {
				if m, ok := p[k].(string); ok && m != "" {
					return m
				}
			}
		}
	}
	return s
}

// AuthSession is one in-progress browserless sign-in. Create it, call
// RequestSMS, then VerifyCode (and CheckPassword if asked), then Close.
type AuthSession struct {
	c         *Client
	closeFn   func()
	authToken string // START_AUTH → used by CHECK_CODE
	trackID   string // password challenge → used by CHECK_PASSWORD
}

// AuthResult is a finished sign-in.
type AuthResult struct {
	Token  string // the session token to store
	SelfID int64  // the signed-in MAX account's id
}

// NewAuthSession connects to MAX as the desktop client and completes INIT.
func NewAuthSession(deviceID string) (*AuthSession, error) {
	c := newClient(Options{TCP: true, DeviceID: deviceID})
	closeFn, err := c.dial()
	if err != nil {
		return nil, err
	}
	go func() { _ = c.readLoop() }()
	go func() { // keep the socket alive while a person types the code
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for range t.C {
			if c.writeFrame(c.pingFrame()) != nil {
				return
			}
		}
	}()
	if err := c.init(); err != nil {
		closeFn()
		return nil, fmt.Errorf("MAX rejected the connection: %s", DescribeError(err))
	}
	return &AuthSession{c: c, closeFn: closeFn}, nil
}

// Close drops the connection.
func (a *AuthSession) Close() { a.closeFn() }

// RequestSMS asks MAX to text a code to phone ("+7…").
func (a *AuthSession) RequestSMS(phone string) error {
	resp, err := a.c.request(OpVerificationRequest, map[string]any{"phone": phone, "type": "START_AUTH"}, 30*time.Second)
	if err != nil {
		return fmt.Errorf("%s", DescribeError(err))
	}
	var r struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(resp.Payload, &r) != nil || r.Token == "" {
		return fmt.Errorf("MAX did not start the sign-in (no auth token in the reply)")
	}
	a.authToken = r.Token
	a.c.phone = phone
	return nil
}

// VerifyCode submits the SMS code. When the account has a password (2FA) it
// returns needPassword=true with the account's hint; otherwise the finished
// sign-in.
func (a *AuthSession) VerifyCode(code string) (res *AuthResult, needPassword bool, hint string, err error) {
	resp, err := a.c.request(OpCodeEnter, map[string]any{"token": a.authToken, "verifyCode": strings.TrimSpace(code)}, 30*time.Second)
	if err != nil {
		return nil, false, "", fmt.Errorf("%s", DescribeError(err))
	}
	if tok := loginTokenFromEnter(resp.Payload); tok != "" {
		res, err = a.finish(tok)
		return res, false, "", err
	}
	var pc struct {
		PasswordChallenge struct {
			TrackID string `json:"trackId"`
			Hint    string `json:"hint"`
		} `json:"passwordChallenge"`
	}
	if json.Unmarshal(resp.Payload, &pc) == nil && pc.PasswordChallenge.TrackID != "" {
		a.trackID = pc.PasswordChallenge.TrackID
		return nil, true, pc.PasswordChallenge.Hint, nil
	}
	return nil, false, "", fmt.Errorf("код не подтверждён: проверьте, что он свежий, и что номер уже зарегистрирован в MAX (мост подключает существующий аккаунт, а не создаёт новый)")
}

// CheckPassword submits the 2FA password after VerifyCode asked for it. A wrong
// password can be retried; a successful one finishes the sign-in.
func (a *AuthSession) CheckPassword(password string) (*AuthResult, error) {
	resp, err := a.c.request(OpCheckPassword, map[string]any{"trackId": a.trackID, "password": password}, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("%s", DescribeError(err))
	}
	tok := loginTokenFromEnter(resp.Payload)
	if tok == "" {
		return nil, fmt.Errorf("MAX не вернул токен — пароль верный?")
	}
	return a.finish(tok)
}

// loginTokenFromEnter extracts the login token from a CHECK_CODE /
// CHECK_PASSWORD reply: the documented tokenAttrs.LOGIN.token, else any
// token-shaped string.
func loginTokenFromEnter(payload json.RawMessage) string {
	var ce struct {
		TokenAttrs struct {
			Login struct {
				Token string `json:"token"`
			} `json:"LOGIN"`
		} `json:"tokenAttrs"`
	}
	if json.Unmarshal(payload, &ce) == nil && ce.TokenAttrs.Login.Token != "" {
		return ce.TokenAttrs.Login.Token
	}
	return findLongToken(payload)
}

// finish exchanges the login token for a session with LOGIN on this very
// connection, keeping a rotated token if MAX hands one out.
func (a *AuthSession) finish(loginToken string) (*AuthResult, error) {
	a.c.token = loginToken
	if err := a.c.login(); err != nil {
		return nil, fmt.Errorf("вход не удался: %s", DescribeError(err))
	}
	return &AuthResult{Token: a.c.token, SelfID: a.c.selfID}, nil
}
