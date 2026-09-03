package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	"maxtg/internal/app"
	"maxtg/internal/config"
	"maxtg/internal/console"
	"maxtg/internal/installer"
	"maxtg/internal/paths"
	"maxtg/internal/store"
)

func main() {
	log.SetFlags(log.LstdFlags)

	cmd := "console" // default: double-click opens the console
	if len(os.Args) > 1 && !hasPrefix(os.Args[1], "-") {
		cmd = os.Args[1]
	}
	// Config lives in the shared data dir unless an explicit -config is given.
	configPath := argValue("-config", "")
	if configPath == "" {
		configPath = paths.Config()
	} else if !filepath.IsAbs(configPath) {
		configPath = filepath.Join(paths.DataDir(), configPath)
	}

	switch cmd {
	case "run":
		runService(configPath)
	case "console":
		console.Run(configPath)
	case "auth":
		// Non-interactive alias: open console straight into SMS auth is not
		// possible without a menu, so just open the console.
		console.Run(configPath)
	case "install":
		must(installer.Install())
	case "uninstall":
		must(installer.Uninstall())
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Printf("unknown command: %s\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

// runService loads config + state and runs the bridge.
func runService(configPath string) {
	// Mirror logs to a file in the shared data dir so a headless run is debuggable.
	if f, err := os.OpenFile(paths.Log(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		log.SetOutput(io.MultiWriter(os.Stderr, f))
	}
	log.Printf("[run] starting bridge (config %s)", configPath)

	// Single-instance guard: only one bridge may poll Telegram at a time
	// (two getUpdates consumers on one bot token conflict). Retry briefly so a
	// self-restart, whose old process is still exiting, can take over the port.
	ln, ok := acquireSingleInstance()
	if !ok {
		log.Printf("[run] another bridge instance is already running; exiting")
		return
	}
	defer ln.Close()

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	st, err := store.Load(cfg.StatePath)
	if err != nil {
		log.Fatalf("state: %v", err)
	}
	if err := app.Run(cfg, st, configPath); err != nil {
		log.Fatalf("service: %v", err)
	}
}

// acquireSingleInstance binds a fixed localhost port as a process-wide lock.
// Returns (listener, true) if we got it; (nil, false) if another instance holds
// it after a few seconds of retrying (covers a restart's brief overlap).
func acquireSingleInstance() (net.Listener, bool) {
	for i := 0; i < 12; i++ {
		if ln, err := net.Listen("tcp", "127.0.0.1:47654"); err == nil {
			return ln, true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil, false
}

func usage() {
	fmt.Print(`maxtg — MAX <-> Telegram bridge

Commands:
  (no args)     open the interactive console (auth, status, setup)
  console       same as above
  run           run the bridge (used by the logon autostart shortcut)
  install       register autostart at logon + desktop shortcut (no admin)
  uninstall     remove the autostart + desktop shortcuts
  help          this help

Flags:
  -config PATH  config file (default: config.json next to the exe)
`)
}

// resolvePath makes a relative path relative to the executable's directory,
// so the service works regardless of its working directory.
func resolvePath(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	exe, err := os.Executable()
	if err != nil {
		return p
	}
	return filepath.Join(filepath.Dir(exe), p)
}

// argValue returns the value following a flag in os.Args, or def.
func argValue(flag, def string) string {
	for i, a := range os.Args {
		if a == flag && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return def
}

func hasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}

func must(err error) {
	if err != nil {
		log.Fatalf("%v", err)
	}
}
