// Package browsergrab pulls the current MAX deviceId + access token + cookies
// out of a running Edge/Chrome that is logged in to web.max.ru, via the Chrome
// DevTools Protocol (CDP). If no debugging browser is reachable it launches
// Edge (or Chrome) itself with a dedicated persistent profile.
package browsergrab

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type target struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	Title                string `json:"title"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// Grab is the fully automated extraction of the MAX web session.
func Grab(cdpBase, matchURL string, timeout time.Duration) (deviceID, token, userAgent, cookie string, err error) {
	if cdpBase == "" {
		return "", "", "", "", fmt.Errorf("browser_cdp_url not set")
	}
	if matchURL == "" {
		matchURL = "web.max.ru"
	}
	base := strings.TrimRight(cdpBase, "/")

	// 0. Make sure a debugging browser is actually up; launch Edge/Chrome if not.
	if !cdpReachable(base) {
		if lerr := launchBrowser(base, matchURL); lerr != nil {
			return "", "", "", "", fmt.Errorf("no debugging browser and launch failed: %w", lerr)
		}
	}

	// 1. Find or create the page
	start := time.Now()
	var t target
	for time.Since(start) < 20*time.Second {
		t, err = findOrCreatePage(base, matchURL)
		if err == nil && t.WebSocketDebuggerURL != "" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if t.WebSocketDebuggerURL == "" {
		return "", "", "", "", fmt.Errorf("could not find or open page: %v", err)
	}

	// Connect to the page's CDP WebSocket
	conn, _, derr := websocket.DefaultDialer.Dial(t.WebSocketDebuggerURL, nil)
	if derr != nil {
		return "", "", "", "", fmt.Errorf("cdp dial: %w", derr)
	}
	defer conn.Close()

	_ = send(conn, 10, "Runtime.enable", nil)
	_ = send(conn, 12, "Network.enable", nil)

	// 2. Fetch User-Agent
	userAgent, err = getUserAgent(conn)
	if err != nil {
		userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
	}

	// 3. Wait for localStorage to populate with the auth token + deviceId.
	const authKey = "__oneme_auth"
	const deviceIDKey = "__oneme_device_id"
	expr := `(function(){try{var a=JSON.parse(localStorage.getItem('` + authKey + `')||'{}'); return JSON.stringify({token:(a&&a.token)||'',deviceId:localStorage.getItem('` + deviceIDKey + `')||''});}catch(e){return '{}';}})()`

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		res, rerr := sendAndWait(conn, 100, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true}, 5*time.Second)
		if rerr == nil {
			var wrap struct {
				Result struct {
					Result struct {
						Value string `json:"value"`
					} `json:"result"`
				} `json:"result"`
			}
			if json.Unmarshal(res, &wrap) == nil {
				var vals struct {
					Token    string `json:"token"`
					DeviceID string `json:"deviceId"`
				}
				if json.Unmarshal([]byte(wrap.Result.Result.Value), &vals) == nil && vals.Token != "" {
					deviceID = vals.DeviceID
					token = vals.Token
					break
				}
			}
		}
		time.Sleep(1 * time.Second)
	}

	if token == "" {
		return "", "", "", "", fmt.Errorf("timeout waiting for MAX token. Is the profile logged in?")
	}

	// 4. Grab cookies for the max.ru / oneme domains (best effort).
	cookie = getCookies(conn)

	// NOTE: we deliberately DO NOT close the page here. The dedicated debug
	// browser stays open with its logged-in profile so the next refresh is
	// instant, and we keep our OWN persistent deviceId (see store.EnsureDeviceID)
	// so this reader never collides with the browser's live push session.
	return deviceID, token, userAgent, cookie, nil
}

// getCookies returns a "name=value; ..." Cookie header built from the browser's
// max.ru / oneme cookies (empty string if none / on error).
func getCookies(conn *websocket.Conn) string {
	res, err := sendAndWait(conn, 200, "Network.getAllCookies", nil, 5*time.Second)
	if err != nil {
		return ""
	}
	var wrap struct {
		Result struct {
			Cookies []struct {
				Name   string `json:"name"`
				Value  string `json:"value"`
				Domain string `json:"domain"`
			} `json:"cookies"`
		} `json:"result"`
	}
	if json.Unmarshal(res, &wrap) != nil {
		return ""
	}
	var parts []string
	for _, ck := range wrap.Result.Cookies {
		d := strings.ToLower(ck.Domain)
		if strings.Contains(d, "max.ru") || strings.Contains(d, "oneme") {
			parts = append(parts, ck.Name+"="+ck.Value)
		}
	}
	return strings.Join(parts, "; ")
}

// cdpReachable reports whether a CDP endpoint is answering on the base URL.
func cdpReachable(base string) bool {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(base + "/json/version")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// launchBrowser starts Edge (preferred) or Chrome with remote debugging on the
// port from cdpBase and a dedicated persistent profile, opened at matchURL, then
// waits for the CDP endpoint to come up.
func launchBrowser(cdpBase, matchURL string) error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("auto-launch is only supported on Windows")
	}
	exe := findBrowser()
	if exe == "" {
		return fmt.Errorf("Edge/Chrome executable not found")
	}
	port := portFromBase(cdpBase)
	profile := filepath.Join(os.Getenv("LOCALAPPDATA"), "MaxTgBrowserProfile")
	page := matchURL
	if !strings.HasPrefix(page, "http") {
		page = "https://" + page
	}
	cmd := exec.Command(exe,
		"--remote-debugging-port="+port,
		"--remote-allow-origins=*",
		"--user-data-dir="+profile,
		"--no-first-run",
		"--no-default-browser-check",
		page,
	)
	if err := cmd.Start(); err != nil {
		return err
	}
	// Wait up to 25s for the debugging endpoint to accept connections.
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if cdpReachable(cdpBase) {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("browser launched but CDP port %s never came up", port)
}

// findBrowser returns the path to msedge.exe or chrome.exe, preferring Edge.
func findBrowser() string {
	candidates := []string{
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `Google\Chrome\Application\chrome.exe`),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `Google\Chrome\Application\chrome.exe`),
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// portFromBase extracts the port from a CDP base URL (default 9222).
func portFromBase(base string) string {
	if u, err := url.Parse(base); err == nil {
		if _, p, err := net.SplitHostPort(u.Host); err == nil && p != "" {
			return p
		}
	}
	return "9222"
}

func findOrCreatePage(base, matchURL string) (target, error) {
	resp, err := http.Get(base + "/json/list")
	if err != nil {
		return target{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var targets []target
	if err := json.Unmarshal(body, &targets); err != nil {
		return target{}, err
	}

	for _, t := range targets {
		if t.Type == "page" && strings.Contains(t.URL, matchURL) && t.WebSocketDebuggerURL != "" {
			return t, nil
		}
	}

	newUrl := base + "/json/new?" + url.QueryEscape("https://"+matchURL)
	resp2, err2 := http.Get(newUrl)
	if err2 != nil {
		return target{}, err2
	}
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	var t target
	if err := json.Unmarshal(body2, &t); err != nil {
		return target{}, err
	}
	return t, nil
}

func getUserAgent(conn *websocket.Conn) (string, error) {
	res, err := sendAndWait(conn, 11, "Runtime.evaluate", map[string]any{"expression": "navigator.userAgent", "returnByValue": true}, 3*time.Second)
	if err != nil {
		return "", err
	}
	var wrap struct {
		Result struct {
			Result struct {
				Value string `json:"value"`
			} `json:"result"`
		} `json:"result"`
	}
	if err := json.Unmarshal(res, &wrap); err != nil {
		return "", err
	}
	return wrap.Result.Result.Value, nil
}

type cdpCommand struct {
	ID     int            `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params,omitempty"`
}

func send(conn *websocket.Conn, id int, method string, params map[string]any) error {
	return conn.WriteJSON(cdpCommand{ID: id, Method: method, Params: params})
}

func sendAndWait(conn *websocket.Conn, id int, method string, params map[string]any, timeout time.Duration) (json.RawMessage, error) {
	if err := send(conn, id, method, params); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		_, data, err := conn.ReadMessage()
		if err != nil {
			return nil, err
		}
		var head struct {
			ID int `json:"id"`
		}
		if err := json.Unmarshal(data, &head); err == nil && head.ID == id {
			return data, nil
		}
	}
	return nil, fmt.Errorf("cdp response %d timed out", id)
}
