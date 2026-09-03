package installer

import (
	_ "embed"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"maxtg/internal/paths"
)

//go:embed config.template.json
var configTemplate []byte

// appName is the base name used for shortcuts.
const appName = "MaxTgBridge"

// InstallDir is the per-user install directory (no admin rights required).
func InstallDir() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = filepath.Join(os.Getenv("USERPROFILE"), `AppData\Local`)
	}
	return filepath.Join(base, "Programs", appName)
}

// Install copies the binary into the per-user Programs dir, drops a config
// template on first install, creates a Desktop console shortcut, and registers
// the bridge to autostart at logon IN THE USER'S SESSION (a Startup-folder
// shortcut running "maxtg.exe run" minimized). Running in the user session —
// not as a SYSTEM service — is what lets the bridge pop a visible Edge for the
// one-time MAX login. No administrator rights are needed.
func Install() error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("install is only supported on Windows")
	}

	dir := InstallDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	target := filepath.Join(dir, "maxtg.exe")
	configPath := paths.Config()

	// Replace the binary (skip if we are already the installed exe).
	if !sameFile(self, target) {
		if err := copyFile(self, target); err != nil {
			return fmt.Errorf("copy binary: %w", err)
		}
		log.Printf("[install] binary -> %s", target)
	}

	// Provide config on first install only (never overwrite a filled one),
	// migrating an existing one from older locations if present.
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		candidates := []string{
			filepath.Join(dir, "config.json"),
			filepath.Join(filepath.Dir(self), "config.json"),
		}
		migrated := false
		for _, src := range candidates {
			if _, e := os.Stat(src); e == nil && !sameFile(src, configPath) {
				if copyFile(src, configPath) == nil {
					log.Printf("[install] migrated config from %s -> %s", src, configPath)
					migrated = true
					break
				}
			}
		}
		if !migrated {
			_ = os.WriteFile(configPath, configTemplate, 0o600)
			log.Printf("[install] wrote config -> %s (edit phone/telegram before use)", configPath)
		}
		// Migrate an existing state (token) too, if present in an old location.
		if _, e := os.Stat(paths.State()); os.IsNotExist(e) {
			for _, src := range []string{filepath.Join(dir, "state.json"), filepath.Join(filepath.Dir(self), "state.json")} {
				if _, e := os.Stat(src); e == nil {
					_ = copyFile(src, paths.State())
					break
				}
			}
		}
	}

	if err := createConsoleShortcut(target, dir); err != nil {
		log.Printf("[install] desktop shortcut: %v", err)
	}

	// Register the bridge itself to autostart at logon (minimized), in the
	// user's interactive session.
	if err := createAppAutostart(target, dir); err != nil {
		log.Printf("[install] autostart shortcut: %v", err)
	} else {
		log.Printf("[install] autostart registered (runs '%s run' at logon)", target)
	}

	log.Printf("[install] done. Edit %s, then launch from the Desktop shortcut for first setup/auth.", configPath)
	log.Printf("[install] the bridge autostarts at your next logon (or start it now from the console).")
	return nil
}

// Uninstall removes the autostart + desktop shortcuts (data is left intact).
func Uninstall() error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("uninstall is only supported on Windows")
	}
	removed := 0
	if startup := os.Getenv("APPDATA"); startup != "" {
		lnk := filepath.Join(startup, `Microsoft\Windows\Start Menu\Programs\Startup`, appName+".lnk")
		if err := os.Remove(lnk); err == nil {
			removed++
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		if err := os.Remove(filepath.Join(home, "Desktop", "MAX-TG Console.lnk")); err == nil {
			removed++
		}
	}
	log.Printf("[uninstall] removed %d shortcut(s). Data dir left at %s.", removed, paths.DataDir())
	return nil
}

// createConsoleShortcut creates a Desktop .lnk that opens the interactive console.
func createConsoleShortcut(target, workDir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	lnk := filepath.Join(home, "Desktop", "MAX-TG Console.lnk")
	ps := fmt.Sprintf(
		`$w=New-Object -ComObject WScript.Shell; `+
			`$s=$w.CreateShortcut(%q); `+
			`$s.TargetPath=%q; $s.Arguments='console'; `+
			`$s.WorkingDirectory=%q; $s.IconLocation=%q; $s.Save()`,
		lnk, target, workDir, target)
	return exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps).Run()
}

// createAppAutostart puts a .lnk in the user's Startup folder that runs the
// bridge ("maxtg.exe run") minimized at logon.
func createAppAutostart(target, workDir string) error {
	ps := fmt.Sprintf(
		`$startup=[Environment]::GetFolderPath('Startup'); `+
			`$lnk=Join-Path $startup %q; `+
			`$w=New-Object -ComObject WScript.Shell; `+
			`$s=$w.CreateShortcut($lnk); `+
			`$s.TargetPath=%q; $s.Arguments='run'; `+
			`$s.WorkingDirectory=%q; $s.WindowStyle=7; $s.Save()`,
		appName+".lnk", target, workDir)
	return exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps).Run()
}

func sameFile(a, b string) bool {
	ap, err1 := filepath.Abs(a)
	bp, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return ap == bp
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
