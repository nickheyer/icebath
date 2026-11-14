package gateway

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pypwd"
)

// Linux host with the user and the linger file probe of a scenario, the loginctl run recorded
func lingerHost(t *testing.T, lingerFile bool, rc int, stderr string) *[][]string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, ".icebath"))
	swapSetup(t, &serviceGOOS, "linux")
	swapSetup(t, &hostIsTermux, func() bool { return false })
	swapSetup(t, &lifecycleGetuser, func() (string, error) { return "testuser", nil })
	swapSetup(t, &lifecyclePathExists, func(p string) bool { return lingerFile && p == "/var/lib/systemd/linger/testuser" })
	no := false
	swapSetup(t, &lifecycleLingerStatus, func(context.Context, string) (*bool, string) { return &no, "" })
	swapSetup(t, &serviceLookPath, func(string) (string, error) { return "/usr/bin/loginctl", nil })
	runs := &[][]string{}
	saved := installHooks.enableLinger
	t.Cleanup(func() { installHooks.enableLinger = saved })
	installHooks.enableLinger = func(_ context.Context, username string) (CommandResult, error) {
		*runs = append(*runs, []string{"loginctl", "enable-linger", username})
		return CommandResult{ReturnCode: rc, Stderr: stderr}, nil
	}
	return runs
}

func TestEnsureLingerEnabledLingerAlreadyEnabledViaFile(t *testing.T) {
	runs := lingerHost(t, true, 0, "")
	var out bytes.Buffer
	if _, err := ensureLingerEnabled(context.Background(), &out, "", false); err != nil {
		t.Fatal(err)
	}
	if len(*runs) != 0 {
		t.Fatalf("runs %v", *runs)
	}
}

func TestEnsureLingerEnabledLoginctlSuccessEnablesLinger(t *testing.T) {
	runs := lingerHost(t, false, 0, "")
	var out bytes.Buffer
	if _, err := ensureLingerEnabled(context.Background(), &out, "", false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*runs, [][]string{{"loginctl", "enable-linger", "testuser"}}) {
		t.Fatalf("runs %v", *runs)
	}
}

func TestEnsureLingerEnabledLoginctlFailureShowsManualGuidance(t *testing.T) {
	lingerHost(t, false, 1, "Permission denied")
	swapSetup(t, &serviceGeteuid, func() int { return 0 })
	var out bytes.Buffer
	if _, err := ensureLingerEnabled(context.Background(), &out, "", false); err != nil {
		t.Fatal(err)
	}
	if text := out.String(); !strings.Contains(text, "sudo loginctl enable-linger testuser") || !strings.Contains(text, "Permission denied") {
		t.Fatalf("output %q", text)
	}
}

func TestEnsureLingerEnabledSystemScopeEnablesTargetUserWithoutLogoutMessaging(t *testing.T) {
	runs := lingerHost(t, false, 0, "")
	var out bytes.Buffer
	enabled, err := ensureLingerEnabled(context.Background(), &out, "alice", true)
	if err != nil || !enabled {
		t.Fatalf("enabled %v %v", enabled, err)
	}
	if !reflect.DeepEqual(*runs, [][]string{{"loginctl", "enable-linger", "alice"}}) {
		t.Fatalf("runs %v", *runs)
	}
	if text := out.String(); !strings.Contains(text, "alice") || strings.Contains(text, "logout") {
		t.Fatalf("output %q", text)
	}
}

func TestEnsureLingerEnabledSystemScopeWarningUsesSystemRestart(t *testing.T) {
	lingerHost(t, false, 1, "Permission denied")
	swapSetup(t, &serviceGeteuid, func() int { return 0 })
	var out bytes.Buffer
	enabled, err := ensureLingerEnabled(context.Background(), &out, "alice", true)
	if err != nil || enabled {
		t.Fatalf("enabled %v %v", enabled, err)
	}
	name, err := GetServiceName(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if text := out.String(); !strings.Contains(text, "sudo systemctl restart "+name+".service") || strings.Contains(text, "systemctl --user") {
		t.Fatalf("output %q", text)
	}
}

// Restores the installer seam table after a test swaps its entries
func keepInstallHooks(t *testing.T) {
	t.Helper()
	saved := installHooks
	t.Cleanup(func() { installHooks = saved })
}

func TestEnsureSystemServiceLingerFreshEnableWaitsOnTargetUidAndHintsRestartOnlyWhenRunning(t *testing.T) {
	for _, running := range []bool{true, false} {
		t.Run(map[bool]string{true: "running", false: "stopped"}[running], func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			keepInstallHooks(t)
			installHooks.ensureLinger = func(context.Context, io.Writer, string, bool) (bool, error) { return true, nil }
			installHooks.getpwnam = func(string) (pypwd.Passwd, error) { return pypwd.Passwd{UID: 1001}, nil }
			var waited []int
			installHooks.waitUserBus = func(uid int, _ time.Duration) bool { waited = append(waited, uid); return true }
			adopted := false
			swapSetup(t, &lifecycleUserSystemdSocketUp, func() bool { adopted = true; return true })
			installHooks.unitIsActive = func(context.Context, bool) (bool, error) { return running, nil }
			var out bytes.Buffer
			if err := ensureSystemServiceLinger(context.Background(), &out, "alice"); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(waited, []int{1001}) || adopted {
				t.Fatalf("waited %v adopted %v", waited, adopted)
			}
			name, err := GetServiceName(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "sudo systemctl restart "+name+".service") != running {
				t.Fatalf("output %q", out.String())
			}
		})
	}
}

func TestEnsureSystemServiceLingerAlreadyEnabledSkipsWaitAndActivityProbe(t *testing.T) {
	keepInstallHooks(t)
	installHooks.ensureLinger = func(context.Context, io.Writer, string, bool) (bool, error) { return false, nil }
	installHooks.waitUserBus = func(int, time.Duration) bool { t.Fatal("waited"); return false }
	installHooks.unitIsActive = func(context.Context, bool) (bool, error) { t.Fatal("probed"); return false, nil }
	if err := ensureSystemServiceLinger(context.Background(), &bytes.Buffer{}, "alice"); err != nil {
		t.Fatal(err)
	}
}

// Temp home and recorded systemctl runs of one install, each run the full argv
func installHost(t *testing.T) (*bytes.Buffer, *[][]string) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, ".icebath"))
	keepInstallHooks(t)
	runs := &[][]string{}
	swapSetup(t, &lifecycleRunSystemctl, func(_ context.Context, args []string, system bool, _ time.Duration) (CommandResult, error) {
		*runs = append(*runs, append(append([]string(nil), SystemctlCmd(system)...), args...))
		return CommandResult{}, nil
	})
	return &bytes.Buffer{}, runs
}

func TestSystemdInstallCallsLingerHelper(t *testing.T) {
	out, runs := installHost(t)
	unitPath := filepath.Join(t.TempDir(), "systemd", "user", "icebath-gateway.service")
	installHooks.unitPath = func(context.Context, bool) (string, error) { return unitPath, nil }
	installHooks.generateUnit = func(context.Context, bool, string) (string, error) {
		return "[Service]\nEnvironment=\"ICEBATH_HOME=/home/alice/.icebath\"\n", nil
	}
	var helperCalls []bool
	installHooks.ensureLinger = func(context.Context, io.Writer, string, bool) (bool, error) {
		helperCalls = append(helperCalls, true)
		return false, nil
	}
	if err := systemdInstall(context.Background(), out, false, false, "", true, false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(unitPath); err != nil {
		t.Fatal(err)
	}
	name, err := GetServiceName(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*runs, [][]string{{"systemctl", "--user", "daemon-reload"}, {"systemctl", "--user", "enable", name}}) {
		t.Fatalf("runs %v", *runs)
	}
	if !reflect.DeepEqual(helperCalls, []bool{true}) {
		t.Fatalf("helper calls %v", helperCalls)
	}
}

// Points every system scope step of an install at fakes the way the system install cases patch them
func systemInstallFakes(t *testing.T, unitPath string, helperCalls *[]string) {
	t.Helper()
	installHooks.requireRoot = func(string) error { return nil }
	installHooks.hasLegacy = func(context.Context) bool { return false }
	installHooks.unitPath = func(context.Context, bool) (string, error) { return unitPath, nil }
	installHooks.ensureSystemLinger = func(_ context.Context, _ io.Writer, username string) error {
		*helperCalls = append(*helperCalls, username)
		return nil
	}
}

func TestSystemdInstallTargetsLingerAtSystemServiceUser(t *testing.T) {
	for _, user := range []string{"alice", "root"} {
		t.Run(user, func(t *testing.T) {
			out, _ := installHost(t)
			var helperCalls []string
			systemInstallFakes(t, filepath.Join(t.TempDir(), "systemd", "icebath-gateway.service"), &helperCalls)
			installHooks.prepareLauncher = func(context.Context, bool, string) error { return nil }
			installHooks.generateUnit = func(_ context.Context, _ bool, runAsUser string) (string, error) {
				return "[Service]\nUser=" + runAsUser + "\n", nil
			}
			installHooks.conflictWarning = func(context.Context, io.Writer) error { return nil }
			installHooks.legacyWarning = func(context.Context, io.Writer) {}
			if err := systemdInstall(context.Background(), out, false, true, user, true, false, false); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(helperCalls, []string{user}) {
				t.Fatalf("helper calls %v", helperCalls)
			}
		})
	}
}

func TestExistingSystemInstallRepairsLingerForConfiguredUser(t *testing.T) {
	for _, current := range []bool{true, false} {
		t.Run(map[bool]string{true: "current", false: "stale"}[current], func(t *testing.T) {
			out, _ := installHost(t)
			unitPath := filepath.Join(t.TempDir(), "systemd", "icebath-gateway.service")
			if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(unitPath, []byte("[Service]\nUser=alice\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			var helperCalls []string
			systemInstallFakes(t, unitPath, &helperCalls)
			installHooks.syncHome = func(context.Context, bool) error { return nil }
			installHooks.unitIsCurrent = func(context.Context, bool) (bool, error) { return current, nil }
			installHooks.refreshUnit = func(context.Context, bool) (bool, error) { return false, nil }
			if err := systemdInstall(context.Background(), out, false, true, "alice", true, false, false); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(helperCalls, []string{"alice"}) {
				t.Fatalf("helper calls %v", helperCalls)
			}
		})
	}
}

func TestSystemdInstallRepairPathKeepsLingerGuarantee(t *testing.T) {
	for _, system := range []bool{false, true} {
		t.Run(map[bool]string{true: "system", false: "user"}[system], func(t *testing.T) {
			out, _ := installHost(t)
			unitPath := filepath.Join(t.TempDir(), "icebath-gateway.service")
			if err := os.WriteFile(unitPath, []byte("old unit\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			installHooks.unitPath = func(context.Context, bool) (string, error) { return unitPath, nil }
			installHooks.unitIsCurrent = func(context.Context, bool) (bool, error) { return false, nil }
			installHooks.hasLegacy = func(context.Context) bool { return false }
			installHooks.requireRoot = func(string) error { return nil }
			installHooks.syncHome = func(context.Context, bool) error { return nil }
			installHooks.readUser = func(string) (string, error) { return "", nil }
			installHooks.refreshUnit = func(context.Context, bool) (bool, error) { return false, nil }
			var helperCalls []bool
			installHooks.ensureLinger = func(context.Context, io.Writer, string, bool) (bool, error) {
				helperCalls = append(helperCalls, true)
				return false, nil
			}
			if err := systemdInstall(context.Background(), out, false, system, "", true, false, false); err != nil {
				t.Fatal(err)
			}
			want := []bool{true}
			if system {
				want = nil
			}
			if !reflect.DeepEqual(helperCalls, want) {
				t.Fatalf("helper calls %v", helperCalls)
			}
		})
	}
}
