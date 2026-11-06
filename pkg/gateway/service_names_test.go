package gateway

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/profiles"
)

// Pins the platform, effective uid and system unit dir seams for one test
func pinServiceSeams(t *testing.T, goos string, euid int, unitDir string) {
	t.Helper()
	oldGOOS, oldEuid, oldDir, oldLookup := serviceGOOS, serviceGeteuid, systemUnitDir, serviceLookupAccount
	t.Cleanup(func() {
		serviceGOOS, serviceGeteuid, systemUnitDir, serviceLookupAccount = oldGOOS, oldEuid, oldDir, oldLookup
	})
	serviceGOOS, serviceGeteuid = goos, func() int { return euid }
	if unitDir != "" {
		systemUnitDir = unitDir
	}
}

func writeUnit(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdirs(t *testing.T, p string) string {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUserUnitDirFollowsTheAccountHomeNotAProfilePinnedProcessHome(t *testing.T) {
	tmp := t.TempDir()
	account, active := filepath.Join(tmp, "account"), filepath.Join(tmp, "active-profile-root")
	process := mkdirs(t, filepath.Join(active, "home"))
	t.Setenv("ICEBATH_HOME", active)
	t.Setenv("HOME", process)
	t.Setenv("ICEBATH_REAL_HOME", account)
	t.Setenv("XDG_CONFIG_HOME", "")
	unit, err := GetSystemdUnitPath(context.Background(), false)
	if err != nil || filepath.Dir(unit) != filepath.Join(account, ".config", "systemd", "user") || strings.HasPrefix(unit, process) {
		t.Fatalf("unit %q %v", unit, err)
	}
}

func TestBareUserUnitPinningThisCustomHomeIsAdoptedByLifecycleCommands(t *testing.T) {
	tmp := t.TempDir()
	custom := mkdirs(t, filepath.Join(tmp, "icebath"))
	unitDir := mkdirs(t, filepath.Join(tmp, "xdg", "systemd", "user"))
	t.Setenv("ICEBATH_HOME", custom)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "xdg"))
	pinServiceSeams(t, "linux", 1000, "")
	legacy := filepath.Join(unitDir, "icebath-gateway.service")
	writeUnit(t, legacy, "[Service]\nEnvironment=\"ICEBATH_HOME="+filepath.Join(tmp, "other")+"\"\nExecStart=/usr/bin/icebath gateway run\n")
	ctx := context.Background()
	if name, err := GetServiceName(ctx); err != nil || name == "icebath-gateway" {
		t.Fatalf("foreign bare unit adopted %q %v", name, err)
	}
	writeUnit(t, legacy, "[Service]\nEnvironment=\"ICEBATH_HOME="+custom+"\"\nRestart=always\nExecStart=/usr/bin/icebath gateway run\n")
	if name, err := GetServiceName(ctx); err != nil || name != "icebath-gateway" {
		t.Fatalf("own bare unit not adopted %q %v", name, err)
	}
	if p, _ := GetSystemdUnitPath(ctx, false); p != legacy {
		t.Fatalf("unit path %q", p)
	}
}

func TestHomeNotPinnedByUnitKeepsItsSuffix(t *testing.T) {
	tmp := t.TempDir()
	alice := mkdirs(t, filepath.Join(tmp, "alice", ".icebath"))
	bob := mkdirs(t, filepath.Join(tmp, "bob", ".icebath"))
	unitDir := mkdirs(t, filepath.Join(tmp, "systemd"))
	writeUnit(t, filepath.Join(unitDir, "icebath-gateway.service"), "[Service]\nEnvironment=\"ICEBATH_HOME="+alice+"\"\n")
	pinServiceSeams(t, "linux", 0, unitDir)
	t.Setenv("ICEBATH_HOME", bob)
	name, err := GetServiceName(context.Background())
	if err != nil || name == "icebath-gateway" || !strings.HasPrefix(name, "icebath-gateway-") || len(name) != len("icebath-gateway-")+8 {
		t.Fatalf("name %q %v", name, err)
	}
}

func TestUnprivilegedProfileCommandIgnoresTheSystemUnit(t *testing.T) {
	tmp := t.TempDir()
	profile := mkdirs(t, filepath.Join(tmp, "alice", ".icebath", "profiles", "kimi"))
	unitDir := mkdirs(t, filepath.Join(tmp, "systemd"))
	writeUnit(t, filepath.Join(unitDir, "icebath-gateway.service"), "[Service]\nEnvironment=\"ICEBATH_HOME="+profile+"\"\n")
	pinServiceSeams(t, "linux", 1000, unitDir)
	t.Setenv("ICEBATH_REAL_HOME", filepath.Join(tmp, "alice"))
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("ICEBATH_HOME", profile)
	if name, err := GetServiceName(context.Background()); err != nil || name != "icebath-gateway-kimi" {
		t.Fatalf("name %q %v", name, err)
	}
}

func TestBareUnitPinningANamedProfileHomeKeepsTheBareName(t *testing.T) {
	tmp := t.TempDir()
	profile := mkdirs(t, filepath.Join(tmp, "alice", ".icebath", "profiles", "kimi"))
	unitDir := mkdirs(t, filepath.Join(tmp, "systemd"))
	writeUnit(t, filepath.Join(unitDir, "icebath-gateway.service"), "[Service]\nEnvironment=\"ICEBATH_HOME="+profile+"\"\n")
	pinServiceSeams(t, "linux", 0, unitDir)
	t.Setenv("ICEBATH_HOME", profile)
	if name, err := GetServiceName(context.Background()); err != nil || name != "icebath-gateway" {
		t.Fatalf("name %q %v", name, err)
	}
	if got := profileNameFromHome(profile, filepath.Dir(filepath.Dir(profile))); got != "kimi" {
		t.Fatalf("profile branch %q", got)
	}
}

func TestBareUnitIsNoIdentityAuthorityOffLinux(t *testing.T) {
	tmp := t.TempDir()
	custom := mkdirs(t, filepath.Join(tmp, "icebath"))
	unitDir := mkdirs(t, filepath.Join(tmp, "systemd"))
	writeUnit(t, filepath.Join(unitDir, "icebath-gateway.service"), "Environment=\"ICEBATH_HOME="+custom+"\"\n")
	pinServiceSeams(t, "darwin", 0, unitDir)
	t.Setenv("ICEBATH_HOME", custom)
	if name, _ := GetServiceName(context.Background()); name == "icebath-gateway" {
		t.Fatal("a systemd unit named the launchd label")
	}
}

func TestProfileNameFromHomeRules(t *testing.T) {
	root := t.TempDir()
	for home, want := range map[string]string{
		filepath.Join(root, "profiles", "work"):                  "work",
		filepath.Join(root, "profiles", "w_k-1"):                 "w_k-1",
		filepath.Join(root, "profiles", "Work"):                  "",
		filepath.Join(root, "profiles", "_x"):                    "",
		filepath.Join(root, "profiles", "a", "b"):                "",
		filepath.Join(root, "profiles"):                          "",
		filepath.Join(root, "other", "work"):                     "",
		filepath.Join(root, "profiles", "kimi\n"):                "kimi\n",
		filepath.Join(root, "profiles", strings.Repeat("a", 65)): "",
	} {
		if got := profileNameFromHome(home, root); got != want {
			t.Errorf("%q gave %q want %q", home, got, want)
		}
	}
}

func TestProfileArgAndCurrentProfileName(t *testing.T) {
	root := t.TempDir()
	profile := mkdirs(t, filepath.Join(root, "profiles", "coder"))
	ctx := context.Background()
	if got, _ := profileArg(ctx, profile, root); got != "--profile coder" {
		t.Fatalf("profile arg %q", got)
	}
	if got, _ := profileArg(ctx, root, root); got != "" {
		t.Fatalf("root arg %q", got)
	}
	if got, _ := profileArg(ctx, filepath.Join(root, "elsewhere"), root); got != "" {
		t.Fatalf("foreign arg %q", got)
	}
	t.Setenv("ICEBATH_HOME", profile)
	if got, err := currentProfileName(ctx); err != nil || got != "coder" {
		t.Fatalf("current profile %q %v", got, err)
	}
}

func TestUnitEnvironmentValueUndoesSystemdQuoting(t *testing.T) {
	dir := t.TempDir()
	unit := filepath.Join(dir, "u.service")
	writeUnit(t, unit, "\ufeff[Service]\n  Environment=\"OTHER=1\"\nEnvironment=\"ICEBATH_HOME=/a \\\"b\\\" \\\\c 100%%\"\nEnvironment=ICEBATH_HOME=/later\n")
	if v, err := hermesHomePinnedByUnit(unit); err != nil || v != `/a "b" \c 100%` {
		t.Fatalf("value %q %v", v, err)
	}
	writeUnit(t, unit, "Environment=ICEBATH_HOME=   \nEnvironment=\"\n")
	if v, err := hermesHomePinnedByUnit(unit); err != nil || v != "" {
		t.Fatalf("blank value %q %v", v, err)
	}
	if v, err := hermesHomePinnedByUnit(filepath.Join(dir, "missing.service")); err != nil || v != "" {
		t.Fatalf("missing unit %q %v", v, err)
	}
	writeUnit(t, unit, "Environment=ICEBATH_HOME=\xff\n")
	if _, err := hermesHomePinnedByUnit(unit); err == nil {
		t.Fatal("undecodable unit must raise")
	}
}

func TestServicePinnedHomeReadsPlistAndUnit(t *testing.T) {
	dir := t.TempDir()
	plist := filepath.Join(dir, "ai.icebath.gateway.plist")
	writeUnit(t, plist, `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>EnvironmentVariables</key><dict><key>ICEBATH_HOME</key><string>  /srv/home  </string></dict></dict></plist>`)
	if v, err := ServicePinnedHome(plist); err != nil || v != "/srv/home" {
		t.Fatalf("plist %q %v", v, err)
	}
	writeUnit(t, plist, "not a plist")
	if v, err := ServicePinnedHome(plist); err != nil || v != "" {
		t.Fatalf("broken plist %q %v", v, err)
	}
	dotPlist := filepath.Join(dir, ".plist")
	writeUnit(t, dotPlist, "Environment=ICEBATH_HOME=/unit\n")
	if v, _ := ServicePinnedHome(dotPlist); v != "/unit" {
		t.Fatalf("dot file read as unit %q", v)
	}
	if h := profiles.GatewayServiceHooks.PinnedHome; h == nil {
		t.Fatal("hooks not installed")
	} else if v, ok := h(dotPlist); !ok || v != "/unit" {
		t.Fatalf("hook %q %v", v, ok)
	}
}

func TestLaunchdPlistPathUsesAccountHomeAndFallsBackToRealHome(t *testing.T) {
	tmp := t.TempDir()
	account := mkdirs(t, filepath.Join(tmp, "account"))
	t.Setenv("ICEBATH_HOME", mkdirs(t, filepath.Join(tmp, "account", ".icebath", "profiles", "coder")))
	t.Setenv("HOME", filepath.Join(tmp, "profile-home"))
	pinServiceSeams(t, "darwin", 501, "")
	serviceLookupAccount = func() (*user.User, error) { return &user.User{HomeDir: account}, nil }
	ctx := context.Background()
	if p, err := GetLaunchdPlistPath(ctx); err != nil || p != filepath.Join(account, "Library", "LaunchAgents", "ai.icebath.gateway-coder.plist") {
		t.Fatalf("plist %q %v", p, err)
	}
	serviceLookupAccount = func() (*user.User, error) { return nil, errors.New("uid not found") }
	t.Setenv("ICEBATH_REAL_HOME", filepath.Join(tmp, "real"))
	if p, _ := GetLaunchdPlistPath(ctx); p != filepath.Join(tmp, "real", "Library", "LaunchAgents", "ai.icebath.gateway-coder.plist") {
		t.Fatalf("fallback plist %q", p)
	}
}
