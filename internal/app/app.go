package app

import (
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/kardianos/service"

	"maxtg/internal/bridge"
	"maxtg/internal/browsergrab"
	"maxtg/internal/config"
	"maxtg/internal/dinner"
	"maxtg/internal/maxapi"
	"maxtg/internal/store"
	"maxtg/internal/tg"
)

const (
	SvcName        = "MaxTgBridge"
	SvcDisplayName = "MAX Telegram Bridge"
	SvcDescription = "Bridges MAX messenger messages to a Telegram bot (topics), two-way."
)

type Program struct {
	cfg        *config.Config
	st         *store.Store
	configPath string
}

func BuildProgram(cfg *config.Config, st *store.Store, configPath string) *Program {
	return &Program{cfg: cfg, st: st, configPath: configPath}
}

func (p *Program) Start(s service.Service) error {
	go p.run()
	return nil
}

func (p *Program) Stop(s service.Service) error {
	return nil
}

func (p *Program) run() {
	st := p.st
	bot, err := tg.New(p.cfg.TelegramBotToken, p.cfg.TelegramGroupID, p.cfg.TelegramProxy)
	if err != nil {
		log.Fatalf("[app] telegram init: %v", err)
	}

	var br *bridge.Bridge
	// Always use OUR OWN persistent deviceId (never the browser's) so the service
	// and a logged-in browser tab are different devices and never kick each other.
	deviceID := st.EnsureDeviceID()
	_, token, userAgent, cookie := st.GetCredentials()

	// Transport: the browserless desktop/TCP one when there is no token yet or
	// the token came from the bridge's own sign-in; the web WebSocket for a
	// token imported from a browser (see config.UseTCP).
	useTCP := p.cfg.UseTCP(token != "", st.TokenKindOf())

	var refresh func() (string, string, string, string, error)
	if p.cfg.BrowserCDPURL != "" && !useTCP { // a browser token is a web token: no use over TCP
		cdp := p.cfg.BrowserCDPURL
		match := p.cfg.BrowserMatchURL
		refresh = func() (string, string, string, string, error) {
			return browsergrab.Grab(cdp, match, 25*time.Second)
		}
		log.Printf("[app] auto token refresh via browser CDP at %s", cdp)
	}
	if useTCP {
		log.Printf("[app] MAX transport: desktop TCP (browserless)")
	} else {
		log.Printf("[app] MAX transport: web WebSocket (token from a browser)")
	}

	maxapi.ConfigureVersion(p.cfg.MaxAppVersion, p.cfg.MaxAppVersionAuto, func(v string) {
		p.cfg.MaxAppVersionAuto = v
		if err := config.Save(p.configPath, p.cfg); err != nil {
			log.Printf("[app] saving the found app version failed: %v", err)
		}
	})
	maxClient := maxapi.New(maxapi.Options{
		URL:              p.cfg.MaxWSURL,
		Phone:            p.cfg.Phone,
		DeviceID:         deviceID,
		Token:            token,
		UserAgent:        userAgent,
		Cookie:           cookie,
		ForceOpcode:      p.cfg.IncomingOpcode,
		Binary:           p.cfg.MaxBinary == nil || *p.cfg.MaxBinary, // binary unless explicitly disabled
		TCP:              useTCP,
		SaveDesktopToken: func(d, t string) { st.SetDesktopCredentials(d, t) },
		AllowSMS:         false,
		OnMessage: func(m maxapi.IncomingMessage) {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[app] recovered from panic handling MAX message: %v", r)
					br.ReportPanic("сообщение из MAX", r)
				}
			}()
			br.OnMaxMessage(m)
		},
		OnReaction: func(r maxapi.IncomingReaction) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("[app] recovered from panic handling MAX reaction: %v", rec)
				}
			}()
			br.OnMaxReaction(r)
		},
		OnDelete: func(d maxapi.IncomingDelete) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("[app] recovered from panic handling MAX delete: %v", rec)
				}
			}()
			br.OnMaxDelete(d)
		},
		OnConnect: func() {
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("[app] recovered from panic handling MAX connect: %v", rec)
					br.ReportPanic("синхронизация после подключения", rec)
				}
			}()
			br.SyncAllChats()
		},
		OnStatus:  func(connected bool) { br.OnMaxStatus(connected) },
		SaveToken: func(d, t, ua, ck string) { st.SetCredentials(d, t, ua, ck) },
		LoadToken: func() (string, string, string) { d, t, _, ck := st.GetCredentials(); return d, t, ck },
		Refresh:   refresh,
	})
	br = bridge.New(maxClient, bot, st, p.cfg, p.configPath)

	log.Printf("[app] bridge starting (tg group %d)", p.cfg.TelegramGroupID)
	go maxClient.Run()
	go br.RunPollRefresher()
	if p.cfg.DinnerURL != "" && p.cfg.DinnerToken != "" {
		if p.cfg.DinnerChatID == 0 {
			log.Printf("[dinner] dinner_chat_id не задан — автоотправка обедов выключена")
		} else {
			go dinner.Run(dinner.Options{
				URL: p.cfg.DinnerURL, Token: p.cfg.DinnerToken, ChatID: p.cfg.DinnerChatID,
				Interval: time.Duration(p.cfg.DinnerPollSec) * time.Second,
			}, maxClient)
		}
	}
	br.RunTelegram()
}

func SvcConfig(execPath, workDir, configPath string) *service.Config {
	return &service.Config{
		Name:             SvcName,
		DisplayName:      SvcDisplayName,
		Description:      SvcDescription,
		Executable:       execPath,
		Arguments:        []string{"run", "-config", configPath},
		WorkingDirectory: workDir,
		// Recovery options in case this is ever installed as a Windows service.
		// The default deployment is a user-session autostart app that restarts
		// itself by re-exec (see bridge.restart), so these are a harmless
		// safety net rather than the primary restart mechanism.
		Option: service.KeyValue{
			"OnFailure":              "restart",
			"OnFailureDelayDuration": "5s",
			"OnFailureResetPeriod":   60,
		},
	}
}

func NewService(cfg *config.Config, st *store.Store, scfg *service.Config, configPath string) (service.Service, error) {
	prg := BuildProgram(cfg, st, configPath)
	return service.New(prg, scfg)
}

func Run(cfg *config.Config, st *store.Store, configPath string) error {
	prg := BuildProgram(cfg, st, configPath)

	exe, err := os.Executable()
	if err != nil {
		exe = ""
	}
	workDir := ""
	if exe != "" {
		workDir = filepath.Dir(exe)
	}
	svcCfg := SvcConfig(exe, workDir, configPath)

	s, err := service.New(prg, svcCfg)
	if err != nil {
		return err
	}
	// s.Run() performs the Windows Service Control Manager handshake: it calls
	// Program.Start (which returns immediately after spawning the bridge) so the
	// SCM sees the service as "running", then blocks until the service is
	// stopped. Running the bridge logic directly here previously skipped that
	// handshake, so the SCM timed out after 30s (error 1053) and killed us.
	// When launched from a console (interactive) it simply runs until Ctrl+C.
	return s.Run()
}
