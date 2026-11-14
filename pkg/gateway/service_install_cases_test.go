package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Unit text whose ExecStart runs the gateway the way an older install wrote it
const legacyUnitText = "[Unit]\nDescription=Icebath Gateway\n[Service]\nExecStart=/usr/bin/python -m icebath_cli.main gateway run --replace\n"

// Points the legacy unit search at a user and a system directory under the test temp dir
func legacySearchDirs(t *testing.T) (string, string) {
	t.Helper()
	userDir, systemDir := filepath.Join(t.TempDir(), "user"), filepath.Join(t.TempDir(), "system")
	for _, d := range []string{userDir, systemDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	swapSetup(t, &legacyUnitSearchPaths, func(context.Context) []legacySearchPath {
		return []legacySearchPath{{false, userDir}, {true, systemDir}}
	})
	return userDir, systemDir
}

func TestLegacyHermesUnitDetectionDetectsBothScopesSimultaneously(t *testing.T) {
	userDir, systemDir := legacySearchDirs(t)
	for _, d := range []string{userDir, systemDir} {
		if err := os.WriteFile(filepath.Join(d, "icebath.service"), []byte(legacyUnitText), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var scopes []bool
	for _, u := range findLegacyHermesUnits(context.Background()) {
		scopes = append(scopes, u.system)
	}
	if !reflect.DeepEqual(scopes, []bool{false, true}) {
		t.Fatalf("scopes %v", scopes)
	}
}

func TestLegacyHermesUnitDetectionAcceptsAlternateExecstartFormats(t *testing.T) {
	userDir, _ := legacySearchDirs(t)
	for i, execStart := range []string{
		"ExecStart=/venv/bin/python -m icebath_cli.main gateway run --replace",
		"ExecStart=/venv/bin/python /opt/icebath/icebath_cli/main.py gateway run",
		"ExecStart=/usr/local/bin/icebath gateway run --replace",
		"ExecStart=/venv/bin/python /opt/icebath/gateway/run.py",
	} {
		text := "[Unit]\nDescription=Old Icebath\n[Service]\n" + execStart + "\n"
		if err := os.WriteFile(filepath.Join(userDir, "icebath.service"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		if n := len(findLegacyHermesUnits(context.Background())); n != 1 {
			t.Fatalf("variant %d not detected: %q", i, execStart)
		}
	}
}

// Legacy dirs with every systemctl run recorded and the effective user id faked
func legacyRemovalHost(t *testing.T, asRoot bool) (string, *[]string) {
	t.Helper()
	userDir, _ := legacySearchDirs(t)
	calls := &[]string{}
	swapSetup(t, &lifecycleRunSystemctl, func(_ context.Context, args []string, system bool, _ time.Duration) (CommandResult, error) {
		*calls = append(*calls, strings.Join(append(append([]string(nil), SystemctlCmd(system)...), args...), " "))
		return CommandResult{}, nil
	})
	uid := 1000
	if asRoot {
		uid = 0
	}
	swapSetup(t, &serviceGeteuid, func() int { return uid })
	return userDir, calls
}

func TestRemoveLegacyHermesUnitsRemovesUserScopeLegacyUnit(t *testing.T) {
	userDir, calls := legacyRemovalHost(t, false)
	legacy := filepath.Join(userDir, "icebath.service")
	if err := os.WriteFile(legacy, []byte(legacyUnitText), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, remaining, err := removeLegacyHermesUnits(context.Background(), &bytes.Buffer{}, false, false)
	if err != nil || removed != 1 || len(remaining) != 0 {
		t.Fatalf("removed %d remaining %v %v", removed, remaining, err)
	}
	if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy unit still there %v", err)
	}
	for _, want := range []string{"--user stop icebath.service", "--user disable icebath.service", "--user daemon-reload"} {
		found := false
		for _, c := range *calls {
			found = found || strings.Contains(c, want)
		}
		if !found {
			t.Fatalf("%q not run in %v", want, *calls)
		}
	}
}

func TestRemoveLegacyHermesUnitsDoesNotTouchProfileUnitsDuringMigration(t *testing.T) {
	userDir, _ := legacyRemovalHost(t, true)
	profileUnit, defaultUnit := filepath.Join(userDir, "icebath-gateway-coder.service"), filepath.Join(userDir, "icebath-gateway.service")
	for _, p := range []string{profileUnit, defaultUnit} {
		if err := os.WriteFile(p, []byte(legacyUnitText), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	removed, remaining, err := removeLegacyHermesUnits(context.Background(), &bytes.Buffer{}, false, false)
	if err != nil || removed != 0 || len(remaining) != 0 {
		t.Fatalf("removed %d remaining %v %v", removed, remaining, err)
	}
	for _, p := range []string{profileUnit, defaultUnit} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s removed", p)
		}
	}
}

// Install of a synthetic unit under the test temp dir with the legacy probe, prompt and removal faked
func legacyOfferInstall(t *testing.T, hasLegacy, answer bool) (string, *map[string]any, *int) {
	t.Helper()
	installHost(t)
	unitPath := filepath.Join(t.TempDir(), "icebath-gateway.service")
	installHooks.hasLegacy = func(context.Context) bool { return hasLegacy }
	removeCalled := &map[string]any{"invoked": false}
	installHooks.removeLegacy = func(_ context.Context, _ io.Writer, interactive, _ bool) (int, []string, error) {
		(*removeCalled)["invoked"], (*removeCalled)["interactive"] = true, interactive
		return 1, nil, nil
	}
	installHooks.legacyWarning = func(context.Context, io.Writer) {}
	prompts := new(int)
	saved := gatewayFacade.promptYesNo
	t.Cleanup(func() { gatewayFacade.promptYesNo = saved })
	gatewayFacade.promptYesNo = func(string, bool) bool { *prompts++; return answer }
	installHooks.unitPath = func(context.Context, bool) (string, error) { return unitPath, nil }
	installHooks.generateUnit = func(context.Context, bool, string) (string, error) { return "unit text\n", nil }
	installHooks.ensureLinger = func(context.Context, io.Writer, string, bool) (bool, error) { return false, nil }
	return unitPath, removeCalled, prompts
}

func TestSystemdInstallOffersLegacyRemovalInstallOffersRemovalWhenLegacyDetected(t *testing.T) {
	_, removeCalled, _ := legacyOfferInstall(t, true, true)
	if err := systemdInstall(context.Background(), &bytes.Buffer{}, false, false, "", true, false, false); err != nil {
		t.Fatal(err)
	}
	if (*removeCalled)["invoked"] != true || (*removeCalled)["interactive"] != false {
		t.Fatalf("remove %v", *removeCalled)
	}
}

func TestSystemdInstallOffersLegacyRemovalInstallDeclinesLegacyRemovalWhenUserSaysNo(t *testing.T) {
	unitPath, removeCalled, _ := legacyOfferInstall(t, true, false)
	if err := systemdInstall(context.Background(), &bytes.Buffer{}, false, false, "", true, false, false); err != nil {
		t.Fatal(err)
	}
	if (*removeCalled)["invoked"] != false {
		t.Fatalf("remove %v", *removeCalled)
	}
	if raw, err := os.ReadFile(unitPath); err != nil || string(raw) != "unit text\n" {
		t.Fatalf("unit %q %v", raw, err)
	}
}

func TestSystemdInstallOffersLegacyRemovalInstallSkipsLegacyCheckWhenNonePresent(t *testing.T) {
	_, removeCalled, prompts := legacyOfferInstall(t, false, true)
	if err := systemdInstall(context.Background(), &bytes.Buffer{}, false, false, "", true, false, false); err != nil {
		t.Fatal(err)
	}
	if *prompts != 0 || (*removeCalled)["invoked"] != false {
		t.Fatalf("prompts %d remove %v", *prompts, *removeCalled)
	}
}

func TestGatewayInstallNoninteractiveSkipsLegacyUnitPrompt(t *testing.T) {
	_, removeCalled, prompts := legacyOfferInstall(t, true, true)
	installHooks.conflictWarning = func(context.Context, io.Writer) error { return nil }
	installHooks.generateUnit = func(context.Context, bool, string) (string, error) { return "[Service]", nil }
	if err := systemdInstall(context.Background(), &bytes.Buffer{}, false, false, "", true, true, false); err != nil {
		t.Fatal(err)
	}
	if (*removeCalled)["invoked"] != true || *prompts != 0 {
		t.Fatalf("prompts %d remove %v", *prompts, *removeCalled)
	}
}

func TestSystemdInstallChecksLingerStatus(t *testing.T) {
	out, runs := installHost(t)
	unitPath := filepath.Join(t.TempDir(), "systemd", "user", "icebath-gateway.service")
	installHooks.unitPath = func(context.Context, bool) (string, error) { return unitPath, nil }
	installHooks.generateUnit = func(context.Context, bool, string) (string, error) {
		return "[Service]\nEnvironment=\"ICEBATH_HOME=/home/alice/.icebath\"\n", nil
	}
	helperCalls := 0
	installHooks.ensureLinger = func(context.Context, io.Writer, string, bool) (bool, error) { helperCalls++; return false, nil }
	if err := systemdInstall(context.Background(), out, false, false, "", true, false, false); err != nil {
		t.Fatal(err)
	}
	name, err := GetServiceName(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(unitPath); err != nil || helperCalls != 1 ||
		!reflect.DeepEqual(*runs, [][]string{{"systemctl", "--user", "daemon-reload"}, {"systemctl", "--user", "enable", name}}) {
		t.Fatalf("unit %v helper %d runs %v", err, helperCalls, *runs)
	}
}

// Home and system unit dir of the root pre check, one unit written per present scope
func precheckUnits(t *testing.T, systemPresent, userPresent bool) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, ".config"))
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, ".icebath"))
	swapSetup(t, &systemUnitDir, filepath.Join(tmp, "system"))
	for _, s := range []struct{ system, present bool }{{true, systemPresent}, {false, userPresent}} {
		if !s.present {
			continue
		}
		p, err := GetSystemdUnitPath(context.Background(), s.system)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("[Unit]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	swapSetup(t, &serviceGeteuid, func() int { return 1000 })
}

func TestSystemScopeWizardPreCheckNonRootWithOnlySystemUnitReturnsTrue(t *testing.T) {
	precheckUnits(t, true, false)
	if needs, err := systemScopeWizardWouldNeedRoot(context.Background(), false); err != nil || !needs {
		t.Fatalf("needs root %v %v", needs, err)
	}
}

func TestSystemScopeWizardPreCheckNonRootWithExplicitSystemArgReturnsTrue(t *testing.T) {
	precheckUnits(t, false, false)
	if needs, err := systemScopeWizardWouldNeedRoot(context.Background(), true); err != nil || !needs {
		t.Fatalf("needs root %v %v", needs, err)
	}
}

// Forced launchd reinstall over a supervised incumbent, every step faked and recorded like the hermes fixture
func forcedLaunchdInstall(t *testing.T) (string, *[][]any) {
	t.Helper()
	const label, domain = "ai.icebath.gateway", "gui/501"
	plist := filepath.Join(t.TempDir(), "LaunchAgents", label+".plist")
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plist, []byte("<old/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", t.TempDir())
	keepInstallHooks(t)
	events := &[][]any{}
	swapSetup(t, &launchdPlistPathOf, func(context.Context) (string, error) { return plist, nil })
	swapSetup(t, &launchdLabelOf, func(context.Context) (string, error) { return label, nil })
	swapSetup(t, &launchdDomainOf, func(context.Context) (string, error) { return domain, nil })
	installHooks.definitionBelongs = func(string, string, string) (bool, error) { return true, nil }
	installHooks.supervising = func(context.Context, string) bool { return true }
	installHooks.supervisedPID = func(context.Context, string) (int, bool) { return 4242, true }
	swapSetup(t, &launchdGeneratePlist, func(context.Context) (string, error) { return "<new/>", nil })
	installHooks.refuseTemp = func(string, string) bool { return false }
	installHooks.prepareLauncher = func(context.Context, bool, string) error { return nil }
	installHooks.clearMarker = func(context.Context) {}
	installHooks.reloadBudget = func(context.Context) (float64, error) { return 45.0, nil }
	swapSetup(t, &lifecycleWaitForPIDExit, func(pid int, timeout float64, _ func()) bool {
		*events = append(*events, []any{"wait", pid, timeout})
		return true
	})
	installHooks.appendReloadLog = func(_ context.Context, msg string) { *events = append(*events, []any{"log", msg}) }
	installHooks.bootstrap = func(context.Context, string, string, string, time.Duration) error {
		t.Fatal("forced live reinstall must use the bounded retry path")
		return nil
	}
	swapSetup(t, &launchctlRun, func(_ context.Context, _ time.Duration, _ bool, argv ...string) (CommandResult, error) {
		*events = append(*events, []any{"run", strings.Join(argv, " ")})
		return CommandResult{}, nil
	})
	return plist, events
}

func TestForceInstallWaitsForIncumbentThenRetriesUntilSupervised(t *testing.T) {
	plist, events := forcedLaunchdInstall(t)
	swapSetup(t, &launchdRetryBootstrap, func(_ context.Context, domain, plistPath, label string, deadline time.Time) (bool, error) {
		*events = append(*events, []any{"retry", domain, plistPath, label})
		return true, nil
	})
	var out bytes.Buffer
	if err := launchdInstall(context.Background(), &out, true, true, false); err != nil {
		t.Fatal(err)
	}
	waitIndex, retryIndex, bootout := -1, -1, false
	for i, e := range *events {
		switch e[0] {
		case "run":
			bootout = bootout || e[1] == "launchctl bootout gui/501/ai.icebath.gateway"
		case "wait":
			if reflect.DeepEqual(e, []any{"wait", 4242, 45.0}) {
				waitIndex = i
			}
		case "retry":
			if retryIndex < 0 {
				retryIndex = i
			}
		}
	}
	if !bootout || waitIndex < 0 || retryIndex < 0 || waitIndex > retryIndex {
		t.Fatalf("events %v", *events)
	}
	if raw, _ := os.ReadFile(plist); string(raw) != "<new/>" || !strings.Contains(out.String(), "Service installed and loaded") {
		t.Fatalf("plist %q output %q", raw, out.String())
	}
}

func TestForceInstallFailsClosedWhenLaunchdNeverRegisters(t *testing.T) {
	_, events := forcedLaunchdInstall(t)
	swapSetup(t, &launchdRetryBootstrap, func(context.Context, string, string, string, time.Time) (bool, error) { return false, nil })
	var out bytes.Buffer
	err := launchdInstall(context.Background(), &out, true, true, false)
	var exit *SystemExitError
	if !errors.As(err, &exit) || exit.Code != 1 {
		t.Fatalf("error %v", err)
	}
	if !strings.Contains(out.String(), "service is NOT loaded") || strings.Contains(out.String(), "Service installed and loaded") {
		t.Fatalf("output %q", out.String())
	}
	waited := false
	for _, e := range *events {
		waited = waited || e[0] == "wait"
	}
	if !waited {
		t.Fatalf("events %v", *events)
	}
}
