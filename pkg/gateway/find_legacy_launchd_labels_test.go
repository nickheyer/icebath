package gateway

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"testing"
)

// Writes one gateway LaunchAgent plist, pinning a home when one is given
func writeLegacyLaunchdPlist(t *testing.T, agentsDir, label, argv, pinnedHome string) {
	t.Helper()
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	environment := ""
	if pinnedHome != "" {
		environment = "<key>EnvironmentVariables</key><dict><key>ICEBATH_HOME</key><string>" + pinnedHome + "</string></dict>"
	}
	text := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>Label</key><string>` + label + `</string><key>ProgramArguments</key><array><string>` + argv + `</string></array>` + environment + `</dict></plist>
`
	if err := os.WriteFile(filepath.Join(agentsDir, label+".plist"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyLaunchdLabelsForInstallOnlyUnitsPinnedToThisInstallsHomesAreCredited(t *testing.T) {
	tmp := t.TempDir()
	savedLookup := serviceLookupAccount
	t.Cleanup(func() { serviceLookupAccount = savedLookup })
	serviceLookupAccount = func() (*user.User, error) { return &user.User{HomeDir: tmp}, nil }
	agents := filepath.Join(tmp, "Library", "LaunchAgents")
	root := filepath.Join(tmp, "icebath-root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", root)
	venvPython := filepath.Join(tmp, "install", ".venv", "bin", "python")
	writeLegacyLaunchdPlist(t, agents, "ai.icebath.gateway-398559f7", venvPython, filepath.Join(root, "profiles", "gopod"))
	writeLegacyLaunchdPlist(t, agents, "ai.icebath.gateway-1a2b3c4d", venvPython, root)
	writeLegacyLaunchdPlist(t, agents, "ai.icebath.gateway-gopod", venvPython, filepath.Join(root, "profiles", "gopod"))
	writeLegacyLaunchdPlist(t, agents, "ai.icebath.gateway-5db8084b", venvPython, filepath.Join(tmp, "other-root", "profiles", "gopod"))
	writeLegacyLaunchdPlist(t, agents, "ai.icebath.gateway-nohome", venvPython, "")
	if err := os.WriteFile(filepath.Join(agents, "ai.icebath.gateway-broken.plist"), []byte("not a plist at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := LegacyLaunchdLabelsForInstall(context.Background(), map[string]bool{"ai.icebath.gateway-gopod": true, "ai.icebath.gateway": true})
	if want := []string{"ai.icebath.gateway-1a2b3c4d", "ai.icebath.gateway-398559f7"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
}
