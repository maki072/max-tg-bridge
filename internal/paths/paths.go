// Package paths centralizes the shared data directory so the service (running
// as SYSTEM from Program Files) and the console (running as the user from
// anywhere) always read and write the SAME config, state and log files.
package paths

import (
	"os"
	"path/filepath"
	"runtime"
)

// DataDir returns the shared data directory, creating it if needed.
// On Windows this is %ProgramData%\MaxTgBridge (writable by both SYSTEM and the
// installing user once the installer grants access).
func DataDir() string {
	if runtime.GOOS != "windows" {
		// Linux: $MAXTG_DATA_DIR, else /var/lib/maxtg (what the systemd unit uses).
		d := os.Getenv("MAXTG_DATA_DIR")
		if d == "" {
			d = "/var/lib/maxtg"
		}
		_ = os.MkdirAll(d, 0o755)
		return d
	}
	base := os.Getenv("ProgramData")
	if base == "" {
		base = os.Getenv("PROGRAMDATA")
	}
	if base == "" {
		base = `C:\ProgramData`
	}
	d := filepath.Join(base, "MaxTgBridge")
	_ = os.MkdirAll(d, 0o755)
	return d
}

// Config is the shared config.json path.
func Config() string { return filepath.Join(DataDir(), "config.json") }

// State is the shared state.json path.
func State() string { return filepath.Join(DataDir(), "state.json") }

// Log is the shared log file path.
func Log() string { return filepath.Join(DataDir(), "maxtg.log") }
