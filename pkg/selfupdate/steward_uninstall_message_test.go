package selfupdate

import (
	"strings"
	"testing"
)

func TestStewardUninstallMessagesNameTheSteward(t *testing.T) {
	if !strings.Contains(StewardUninstallMessage(StewardNix, nil), "Nix") || !strings.Contains(StewardUninstallMessage(StewardDocker, nil), "Docker") {
		t.Fatal("steward not named")
	}
	if !strings.Contains(StewardUninstallMessage("somepkg", nil), "somepkg") {
		t.Fatal("unknown steward not named")
	}
}

func TestStewardDesktopMessageIsPerPlatform(t *testing.T) {
	platform := func(p string) *string { return &p }
	if !strings.Contains(StewardUninstallMessage(StewardDesktop, platform("win32")), "Installed apps") ||
		!strings.Contains(StewardUninstallMessage(StewardDesktop, platform("darwin")), "Trash") ||
		!strings.Contains(StewardUninstallMessage(StewardDesktop, platform("linux")), "AppImage") {
		t.Fatal("desktop message not per platform")
	}
}

func TestAllRefusalsPointAtTheDataMode(t *testing.T) {
	for _, steward := range []string{StewardNix, StewardDocker, "somepkg"} {
		if !strings.Contains(StewardUninstallMessage(steward, nil), "icebath uninstall --data") {
			t.Fatalf("%s refusal has no data mode", steward)
		}
	}
	for _, p := range []string{"win32", "darwin", "linux"} {
		if !strings.Contains(StewardUninstallMessage(StewardDesktop, &p), "Settings -> About") {
			t.Fatalf("%s desktop refusal has no data path", p)
		}
	}
}
