package selfupdate

import (
	"strings"
	"testing"
)

func TestStewardUninstallMessagesNameTheSteward(t *testing.T) {
	if !strings.Contains(StewardUninstallMessage(StewardNix), "Nix") || !strings.Contains(StewardUninstallMessage(StewardDocker), "Docker") {
		t.Fatal("steward not named")
	}
	if !strings.Contains(StewardUninstallMessage("somepkg"), "somepkg") {
		t.Fatal("unknown steward not named")
	}
}

func TestAllRefusalsPointAtTheDataMode(t *testing.T) {
	for _, steward := range []string{StewardNix, StewardDocker, StewardAptTermux, "somepkg"} {
		if !strings.Contains(StewardUninstallMessage(steward), "icebath uninstall --data") {
			t.Fatalf("%s refusal has no data mode", steward)
		}
	}
}
