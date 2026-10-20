package maxapi

import (
	"fmt"
	"testing"
)

func TestNextVersion(t *testing.T) {
	if got := NextVersion("26.24.3"); got != "26.25.0" {
		t.Errorf("got %q", got)
	}
	if NextVersion("abc") != "" || NextVersion("26.24") != "" {
		t.Error("malformed versions have no successor")
	}
}

func TestIsOutdatedError(t *testing.T) {
	if !IsOutdatedError(fmt.Errorf("❌ Приложение устарело, пожалуйста, обновитесь")) {
		t.Error("outdated message not recognised")
	}
	if IsOutdatedError(fmt.Errorf("Неверный номер")) || IsOutdatedError(nil) {
		t.Error("false positive")
	}
}

func TestConfigureVersion(t *testing.T) {
	defer ConfigureVersion("", "", nil)
	ConfigureVersion("", "", nil)
	if TCPVersion() != defaultTCPVersion || !autoVersionEnabled() {
		t.Error("auto mode should start from the default")
	}
	ConfigureVersion("", "26.30.0", nil)
	if TCPVersion() != "26.30.0" {
		t.Error("remembered version not used")
	}
	ConfigureVersion("27.1.0", "26.30.0", nil)
	if TCPVersion() != "27.1.0" || autoVersionEnabled() {
		t.Error("manual version must win and disable probing")
	}
}
