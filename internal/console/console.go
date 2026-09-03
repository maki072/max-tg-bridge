package console

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"maxtg/internal/browsergrab"
	"maxtg/internal/config"
	"maxtg/internal/installer"
	"maxtg/internal/maxapi"
	"maxtg/internal/paths"
	"maxtg/internal/store"
	"maxtg/internal/tg"
)

// Run shows an interactive menu. On first run (config incomplete) it launches
// the setup wizard automatically.
func Run(configPath string) {
	in := bufio.NewReader(os.Stdin)

	if cfg, err := config.LoadRaw(configPath); err != nil || !cfg.Complete() {
		fmt.Println("First run: let's configure the bridge.")
		if _, err := setupWizard(configPath, in); err != nil {
			fmt.Printf("Setup error: %v\n", err)
			return
		}
		// Get a MAX token: prefer browser CDP (zero-touch) if configured.
		if cfg, _ := config.LoadRaw(configPath); cfg != nil && cfg.BrowserCDPURL != "" {
			if strings.EqualFold(ask(in, "Pull MAX token from browser now? (y/n)", "y"), "y") {
				pullFromBrowser(configPath)
			}
		} else if strings.EqualFold(ask(in, "Authorize MAX via SMS now? (y/n)", "y"), "y") {
			smsAuth(configPath, in)
		}
		// Offer to register autostart at logon (user session, no admin).
		if strings.EqualFold(ask(in, "Register autostart at logon now? (y/n)", "y"), "y") {
			if err := installer.Install(); err != nil {
				fmt.Printf("Autostart setup failed: %v\n", err)
			}
		}
	}

	for {
		fmt.Println()
		fmt.Println("=== MAX <-> Telegram bridge ===")
		fmt.Println("  1) SMS authorization (mint MAX token)")
		fmt.Println("  2) Pull MAX token from browser now (CDP)")
		fmt.Println("  3) Paste a ready MAX token (manual import)")
		fmt.Println("  4) Check Telegram connectivity (getMe, incl. proxy)")
		fmt.Println("  5) Detect Telegram group id")
		fmt.Println("  6) Edit settings (re-run wizard)")
		fmt.Println("  7) Show status")
		fmt.Println("  8) START the bridge now (background) — needed for the bot to reply")
		fmt.Println("  9) Install / update autostart (runs at logon)")
		fmt.Println("  10) Show recent log   11) Send test message to Telegram")
		fmt.Println("  0) Exit")
		fmt.Print("Choose: ")

		line, _ := in.ReadString('\n')
		switch strings.TrimSpace(line) {
		case "1":
			smsAuth(configPath, in)
		case "2":
			pullFromBrowser(configPath)
		case "3":
			manualToken(configPath, in)
		case "4":
			checkTelegram(configPath)
		case "5":
			detectGroupID(configPath, in)
		case "6":
			setupWizard(configPath, in)
		case "7":
			showStatus(configPath)
		case "8":
			startBridgeBackground(configPath)
		case "9":
			if err := installer.Install(); err != nil {
				fmt.Printf("Autostart setup failed: %v\n", err)
			} else {
				fmt.Println("Autostart registered. It runs at your next logon.")
			}
		case "10":
			showLog()
		case "11":
			selfTest(configPath)
		case "0", "":
			return
		default:
			fmt.Println("Unknown choice.")
		}
	}
}

// checkTelegram verifies Telegram reachability via getMe (through the proxy if set).
func checkTelegram(configPath string) {
	cfg, err := config.LoadRaw(configPath)
	if err != nil || cfg.TelegramBotToken == "" {
		fmt.Println("Set the bot token first (option 6).")
		return
	}
	if cfg.TelegramProxy != "" {
		fmt.Printf("Checking Telegram via proxy %s ...\n", cfg.TelegramProxy)
	} else {
		fmt.Println("Checking Telegram (direct) ...")
	}
	bot, err := tg.New(cfg.TelegramBotToken, cfg.TelegramGroupID, cfg.TelegramProxy)
	if err != nil {
		fmt.Printf("Telegram: FAIL (init): %v\n", err)
		return
	}
	name, err := bot.GetMe()
	if err != nil {
		fmt.Printf("Telegram: FAIL: %v\n", err)
		return
	}
	fmt.Printf("Telegram: OK вЂ” bot %s reachable\n", name)
}

// pullFromBrowser grabs deviceId+token from a CDP browser and stores them.
func pullFromBrowser(configPath string) {
	cfg, err := config.LoadRaw(configPath)
	if err != nil {
		fmt.Printf("Config error: %v\n", err)
		return
	}
	if cfg.BrowserCDPURL == "" {
		fmt.Println("browser_cdp_url is not set. Set it in the wizard (option 6),")
		fmt.Println("and start the browser with --remote-debugging-port, e.g.:")
		fmt.Println(`  chrome.exe --remote-debugging-port=9222 --user-data-dir=C:\max-profile https://web.max.ru`)
		return
	}
	fmt.Printf("Reading token from browser at %s ...\n", cfg.BrowserCDPURL)
	browserDeviceID, token, userAgent, cookie, err := browsergrab.Grab(cfg.BrowserCDPURL, cfg.BrowserMatchURL, 25*time.Second)
	if err != nil {
		fmt.Printf("Browser grab FAIL: %v\n", err)
		return
	}
	if token == "" {
		fmt.Println("Browser grab returned no token (unexpected).")
		return
	}
	st, err := store.Load(cfg.StatePath)
	if err != nil {
		fmt.Printf("State error: %v\n", err)
		return
	}
	// Deliberately keep OUR OWN persistent deviceId вЂ” never adopt the
	// browser's. Reusing the browser's live deviceId makes MAX treat us as
	// the same device as the open browser tab and can invalidate one of the
	// two sessions (this is what caused "token present" flipping back to
	// "MISSING" a moment later).
	ourDeviceID := st.EnsureDeviceID()
	// Keep the captured userAgent + cookies, but our OWN persistent deviceId.
	st.SetCredentials(ourDeviceID, token, userAgent, cookie)
	if browserDeviceID != "" && browserDeviceID != ourDeviceID {
		fmt.Printf("OK вЂ” token pulled from browser (browser deviceId %s ignored;\n", browserDeviceID)
		fmt.Printf("     using our own stable deviceId %s to avoid session collisions).\n", ourDeviceID)
	} else {
		fmt.Println("OK вЂ” token pulled from browser and saved.")
	}
}

// setupWizard prompts for the required settings and writes config.json.
func setupWizard(configPath string, in *bufio.Reader) (*config.Config, error) {
	cfg, _ := config.LoadRaw(configPath)

	cfg.Phone = ask(in, "MAX phone (+7XXXXXXXXXX)", cfg.Phone)
	cfg.TelegramBotToken = ask(in, "Telegram bot token (from @BotFather)", cfg.TelegramBotToken)
	cfg.TelegramProxy = ask(in, "Telegram SOCKS5 proxy (empty = direct; socks5://host:1080)", cfg.TelegramProxy)
	cfg.BrowserCDPURL = ask(in, "Browser CDP url for auto token refresh (empty = off; http://127.0.0.1:9222)", cfg.BrowserCDPURL)

	// Group id: leave empty to auto-detect at runtime, or set explicitly.
	fmt.Println("Group id: leave empty to auto-detect (send a message in the group later),")
	fmt.Println("or type 'detect' to find it now, or paste a numeric id.")
	for {
		v := ask(in, "Telegram group id (empty=auto / 'detect' / -100...)", idStr(cfg.TelegramGroupID))
		if v == "" {
			cfg.TelegramGroupID = 0 // auto-detect at runtime
			break
		}
		if strings.EqualFold(v, "detect") {
			if cfg.TelegramBotToken == "" {
				fmt.Println("Enter the bot token first.")
				continue
			}
			listGroupChats(cfg.TelegramBotToken, cfg.TelegramProxy, in)
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil || n == 0 {
			fmt.Println("Please enter a numeric id like -1001234567890 (or empty for auto).")
			continue
		}
		cfg.TelegramGroupID = n
		break
	}

	if err := config.Save(configPath, cfg); err != nil {
		return nil, err
	}
	fmt.Printf("Saved %s\n", configPath)
	return cfg, nil
}

// smsAuth performs a one-shot SMS login (with optional captcha) and stores the token.
func smsAuth(configPath string, in *bufio.Reader) {
	cfg, err := config.LoadPartial(configPath)
	if err != nil {
		fmt.Printf("Config error: %v\n", err)
		return
	}
	st, err := store.Load(cfg.StatePath)
	if err != nil {
		fmt.Printf("State error: %v\n", err)
		return
	}
	deviceID, _, _, _ := st.GetCredentials()

	client := maxapi.New(maxapi.Options{
		URL:       cfg.MaxWSURL,
		Phone:     cfg.Phone,
		DeviceID:  deviceID,
		AllowSMS:  true,
		SaveToken: func(d, t, ua, ck string) { st.SetCredentials(d, t, ua, ck) },
		LoadToken: func() (string, string, string) { d, t, _, ck := st.GetCredentials(); return d, t, ck },
		ReadSMSCode: func() string {
			return ask(in, "Enter MAX SMS code", "")
		},
		OnCaptcha: func(link string) string {
			fmt.Println("\nMAX requires a captcha. Opening it in your browser...")
			openInBrowser(link)
			fmt.Println("Link:", link)
			fmt.Println("Solve it in the browser. If you can extract the captchaToken")
			fmt.Println("(VK ID flow), paste it below; otherwise leave empty and use")
			fmt.Println("option 3 (paste a ready MAX token) instead.")
			return ask(in, "captchaToken (or empty to abort)", "")
		},
	})

	fmt.Printf("Requesting SMS for %s ...\n", cfg.Phone)
	if err := client.AuthOnce(); err != nil {
		fmt.Printf("Auth failed: %v\n", err)
		return
	}
	fmt.Println("Auth OK. Token saved. The service picks it up on its next reconnect.")
}

// manualToken imports a token+deviceId obtained from a logged-in browser.
func manualToken(configPath string, in *bufio.Reader) {
	cfg, err := config.LoadPartial(configPath)
	if err != nil {
		fmt.Printf("Config error: %v\n", err)
		return
	}
	st, err := store.Load(cfg.StatePath)
	if err != nil {
		fmt.Printf("State error: %v\n", err)
		return
	}
	fmt.Println("Only the token is required. Get it from web.max.ru DevTools ->")
	fmt.Println("Console: copy(JSON.parse(localStorage.__oneme_auth).token)")
	fmt.Println("then paste it below (Ctrl+V). Leave deviceId empty вЂ” we use our")
	fmt.Println("own persistent one so we never collide with the browser's live")
	fmt.Println("session on the same deviceId (that causes token to look present")
	fmt.Println("then flip back to missing a moment later).")
	token := ask(in, "token", "")
	if token == "" {
		fmt.Println("Token is empty, aborted.")
		return
	}
	ourDeviceID := st.EnsureDeviceID()
	_, _, _, cookie := st.GetCredentials()
	st.SetCredentials(ourDeviceID, token, "", cookie)
	fmt.Println("Saved. The service picks it up on its next reconnect.")
}

// detectGroupID / listGroupChats print chats the bot can currently see.
func detectGroupID(configPath string, in *bufio.Reader) {
	cfg, err := config.LoadRaw(configPath)
	if err != nil || cfg.TelegramBotToken == "" {
		fmt.Println("Set the bot token first (option 6).")
		return
	}
	listGroupChats(cfg.TelegramBotToken, cfg.TelegramProxy, in)
}

func listGroupChats(token, proxyURL string, in *bufio.Reader) {
	fmt.Println("\nHow to get the group id:")
	fmt.Println("  1) Add your bot to the group as an admin.")
	fmt.Println("  2) Send any message in that group.")
	fmt.Println("  3) Press Enter here to read it back.")
	ask(in, "Press Enter when done", "")

	bot, err := tg.New(token, 0, proxyURL)
	if err != nil {
		fmt.Printf("Telegram init failed: %v\n", err)
		return
	}
	updates, err := bot.GetUpdates(0, 0)
	if err != nil {
		fmt.Printf("getUpdates failed: %v\n", err)
		return
	}
	seen := map[int64]bool{}
	found := false
	for _, u := range updates {
		if u.Message == nil {
			continue
		}
		c := u.Message.Chat
		if seen[c.ID] {
			continue
		}
		seen[c.ID] = true
		found = true
		fmt.Printf("  id=%d  type=%s  title=%q\n", c.ID, c.Type, c.Title)
	}
	if !found {
		fmt.Println("No chats seen yet. Send a message in the group and retry.")
		fmt.Println("(Supergroup ids look like -1001234567890.)")
	}
}

// showStatus prints token presence and service state.
func showStatus(configPath string) {
	cfg, err := config.LoadRaw(configPath)
	if err != nil {
		fmt.Printf("Config error: %v\n", err)
		return
	}
	if st, err := store.Load(cfg.StatePath); err == nil {
		if _, token, _, _ := st.GetCredentials(); token != "" {
			fmt.Println("MAX token: present")
		} else {
			fmt.Println("MAX token: MISSING (option 1 or 3)")
		}
	}
	fmt.Printf("Config complete: %v\n", cfg.Complete())
	if cfg.TelegramGroupID != 0 {
		fmt.Printf("Telegram group id: %d\n", cfg.TelegramGroupID)
	} else {
		fmt.Println("Telegram group id: not set (auto-detect on first group message)")
	}
	if cfg.BrowserCDPURL != "" {
		fmt.Printf("Browser CDP: %s\n", cfg.BrowserCDPURL)
	}
	// Telegram reachability (incl. proxy).
	if cfg.TelegramBotToken != "" {
		if bot, err := tg.New(cfg.TelegramBotToken, cfg.TelegramGroupID, cfg.TelegramProxy); err == nil {
			if name, err := bot.GetMe(); err == nil {
				via := "direct"
				if cfg.TelegramProxy != "" {
					via = "proxy"
				}
				fmt.Printf("Telegram: OK (%s) вЂ” bot %s\n", via, name)
			} else {
				fmt.Printf("Telegram: FAIL вЂ” %v\n", err)
			}
		}
	}
	// Autostart-at-logon presence (Startup-folder shortcut).
	if startup := os.Getenv("APPDATA"); startup != "" {
		lnk := filepath.Join(startup, `Microsoft\Windows\Start Menu\Programs\Startup`, "MaxTgBridge.lnk")
		if _, err := os.Stat(lnk); err == nil {
			fmt.Println("Autostart: registered (runs at logon)")
		} else {
			fmt.Println("Autostart: NOT registered (option 8)")
		}
	}
}

// openInBrowser opens a URL with the OS default handler.
func openInBrowser(url string) {
	switch runtime.GOOS {
	case "windows":
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		_ = exec.Command("open", url).Start()
	default:
		_ = exec.Command("xdg-open", url).Start()
	}
}

// ask prompts with an optional default and returns the trimmed answer.
func ask(in *bufio.Reader, label, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", label, def)
	} else {
		fmt.Printf("%s: ", label)
	}
	line, _ := in.ReadString('\n')
	v := strings.TrimSpace(line)
	if v == "" {
		return def
	}
	return v
}

// selfTest posts a test message into the group's General topic (no new topic
// is created), verifying the bot token, proxy and group id.
func selfTest(configPath string) {
	cfg, err := config.LoadRaw(configPath)
	if err != nil || cfg.TelegramBotToken == "" {
		fmt.Println("Set the bot token first (option 6).")
		return
	}
	if cfg.TelegramGroupID == 0 {
		fmt.Println("Telegram group id is not set yet. Send any message in your")
		fmt.Println("group (any topic), wait ~30s for the service to auto-detect it,")
		fmt.Println("then check option 7. After that, run this test again.")
		return
	}
	bot, err := tg.New(cfg.TelegramBotToken, cfg.TelegramGroupID, cfg.TelegramProxy)
	if err != nil {
		fmt.Printf("Telegram init failed: %v\n", err)
		return
	}
	if err := bot.SendMessage("self-test OK вЂ” MAX<->TG delivery path works"); err != nil {
		fmt.Printf("sendMessage FAIL: %v\n", err)
		return
	}
	fmt.Println("OK вЂ” posted to the General topic. Check the group.")
}

// startBridgeBackground launches "maxtg.exe run" as a detached, minimized
// process so the bot starts polling immediately without waiting for the next
// logon. The single-instance guard makes a second copy a no-op.
func startBridgeBackground(configPath string) {
	exe, err := os.Executable()
	if err != nil {
		fmt.Printf("cannot locate exe: %v\n", err)
		return
	}
	// `start "" /MIN <exe> ...` detaches from this console (empty title first).
	cmd := exec.Command("cmd", "/C", "start", "", "/MIN", exe, "run", "-config", configPath)
	if err := cmd.Start(); err != nil {
		fmt.Printf("failed to start bridge: %v\n", err)
		return
	}
	fmt.Println("Bridge started in background. Message the bot now — it should reply with the menu.")
	fmt.Println("(Check option 10 'recent log' in a few seconds to confirm it is polling.)")
}

// showLog prints the last lines of maxtg.log (written by the bridge).
func showLog() {
	path := paths.Log()
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Printf("No log yet (%s): %v\n", path, err)
		return
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	n := 40
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	fmt.Printf("--- %s (last %d lines) ---\n", path, len(lines))
	for _, l := range lines {
		fmt.Println(l)
	}
	fmt.Println("--- end ---")
}

func idStr(n int64) string {
	if n == 0 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}


