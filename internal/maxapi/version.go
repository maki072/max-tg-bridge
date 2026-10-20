package maxapi

import (
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// MAX refuses a desktop client whose appVersion is below its current minimum
// with "Приложение устарело, пожалуйста, обновитесь". The version is either set
// by hand (config "max_app_version") or found automatically: on that error the
// minor version is bumped one step at a time until MAX accepts it, and the
// working version is remembered (config "max_app_version_auto").

const (
	defaultTCPVersion = "26.24.0"
	maxVersionProbes  = 40 // how many minor versions above the starting one auto-detect tries
)

var (
	verMu             sync.RWMutex
	autoVersionOn     bool
	autoVersion       string // last version MAX accepted (auto mode)
	onAppVersionFound func(string)

	versionRe = regexp.MustCompile(`^\d{1,3}\.\d{1,3}\.\d{1,3}$`)

	// errVersionBumped tells Run to reconnect at once with the next version.
	errVersionBumped = errors.New("app version outdated, trying a newer one")
)

// ConfigureVersion applies the version settings: manual (non-empty) pins the
// version and disables probing; empty means auto, starting from found (the
// last version that worked, or the built-in default). onFound is called with a
// newly found version so the caller can persist it.
func ConfigureVersion(manual, found string, onFound func(string)) {
	verMu.Lock()
	defer verMu.Unlock()
	AppVersionOverride = manual
	autoVersionOn = manual == ""
	autoVersion = found
	onAppVersionFound = onFound
}

// ValidVersion reports whether v looks like a MAX app version ("26.24.0").
func ValidVersion(v string) bool { return versionRe.MatchString(v) }

// TCPVersion is the appVersion the desktop transport presents right now.
func TCPVersion() string {
	verMu.RLock()
	defer verMu.RUnlock()
	switch {
	case AppVersionOverride != "":
		return AppVersionOverride
	case autoVersion != "":
		return autoVersion
	}
	return defaultTCPVersion
}

func autoVersionEnabled() bool {
	verMu.RLock()
	defer verMu.RUnlock()
	return autoVersionOn && AppVersionOverride == ""
}

// bumpVersion handles a connect failure on the desktop transport: when MAX
// calls the app outdated and auto mode is on, it moves to the next version and
// reports true so the caller reconnects at once.
func (c *Client) bumpVersion(err error) bool {
	if !c.tcp || c.appVersion != "" || !autoVersionEnabled() || !IsOutdatedError(err) || c.verBumps >= maxVersionProbes {
		return false
	}
	next := NextVersion(TCPVersion())
	if next == "" {
		return false
	}
	log.Printf("[max] version %s refused as outdated; trying %s", TCPVersion(), next)
	verMu.Lock()
	autoVersion = next
	verMu.Unlock()
	c.verBumps++
	return true
}

// announceVersion persists a version that MAX accepted after probing.
func announceVersion(v string) {
	verMu.RLock()
	cb := onAppVersionFound
	verMu.RUnlock()
	log.Printf("[max] app version %s accepted by MAX — remembered", v)
	if cb != nil {
		cb(v)
	}
}

// rememberVersion stores a version MAX accepted (auto mode only) and tells the
// caller to persist it when it changed.
func rememberVersion(v string) {
	verMu.Lock()
	changed := autoVersionOn && AppVersionOverride == "" && autoVersion != v
	if changed {
		autoVersion = v
	}
	cb := onAppVersionFound
	verMu.Unlock()
	if changed {
		log.Printf("[max] app version %s accepted by MAX — remembered", v)
		if cb != nil {
			cb(v)
		}
	}
}

// IsOutdatedError recognises MAX's "app is outdated, please update" refusal.
func IsOutdatedError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, m := range []string{"устарел", "обновитесь", "обновите", "outdated", "update.required", "version.not.supported", "unsupported.version"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// NextVersion bumps the minor part: "26.24.0" -> "26.25.0" ("" if v is malformed).
func NextVersion(v string) string {
	if !ValidVersion(v) {
		return ""
	}
	p := strings.Split(v, ".")
	minor, _ := strconv.Atoi(p[1])
	return fmt.Sprintf("%s.%d.0", p[0], minor+1)
}

// StartSMSAuth opens a sign-in connection and requests the SMS code. In auto
// mode, when MAX calls the app outdated it retries with newer versions (each
// refused attempt sends no SMS; only the accepted one does) and remembers the
// one that worked. progress, if set, is told about each version tried.
func StartSMSAuth(deviceID, phone string, progress func(version string)) (*AuthSession, error) {
	v := TCPVersion()
	for i := 0; ; i++ {
		sess, err := newAuthSession(deviceID, v)
		if err == nil {
			if err = sess.RequestSMS(phone); err == nil {
				rememberVersion(v)
				return sess, nil
			}
			sess.Close()
		}
		next := NextVersion(v)
		if !autoVersionEnabled() || !IsOutdatedError(err) || next == "" || i >= maxVersionProbes {
			return nil, err
		}
		log.Printf("[max] version %s refused as outdated; trying %s", v, next)
		v = next
		if progress != nil {
			progress(v)
		}
	}
}
