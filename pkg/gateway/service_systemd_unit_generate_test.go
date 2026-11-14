package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
)

// Generated unit of the requested scope, failing the test on error
func mustGenerateUnit(t *testing.T, system bool, runAsUser string) string {
	t.Helper()
	unit, err := GenerateSystemdUnit(context.Background(), system, runAsUser)
	if err != nil {
		t.Fatal(err)
	}
	return unit
}

// Identity double naming alice with the given home and uid 1001
func aliceIdentity(t *testing.T, homeDir string) {
	t.Helper()
	swapSeam(t, &systemdServiceIdentity, func(string) (string, string, string, int, error) { return "alice", "alice", homeDir, 1001, nil })
}

// Service PATH dirs double returning none
func noServicePathDirs(t *testing.T) {
	t.Helper()
	swapSeam(t, &systemdServicePathDirs, func(context.Context, string) []string { return nil })
}

// Timeout stop line for the given drain budgets
func timeoutStopLine(t *testing.T, drain, cron float64) string {
	t.Helper()
	v, err := ResolveSystemdTimeoutStopSec(drain, cron, CronDrainCleanupReserveS, SystemdStopHeadroomS, SystemdTimeoutStopSecFloor)
	if err != nil {
		t.Fatal(err)
	}
	return "TimeoutStopSec=" + v.String()
}

func TestSystemdServiceRefreshRefreshRefusesToBakePytestTmpdirIntoRealUserUnit(t *testing.T) {
	tmp := sandboxServiceHomes(t)
	unitPath := filepath.Join(tmp, "icebath-gateway.service")
	os.WriteFile(unitPath, []byte("old unit\n"), 0o644)
	swapSeam(t, &systemdUnitPathOf, func(context.Context, bool) (string, error) { return unitPath, nil })
	polluted := "[Service]\n" + `Environment="ICEBATH_HOME=/tmp/pytest-of-alice/pytest-42/popen-gw0/test_x/icebath_test"` + "\n"
	swapSeam(t, &systemdGenerateUnit, func(context.Context, bool, string) (string, error) { return polluted, nil })
	calls := recordSystemctl(t)
	if ok, err := refreshSystemdUnitIfNeeded(context.Background(), false); ok || err != nil {
		t.Fatal(ok, err)
	}
	if now, _ := os.ReadFile(unitPath); string(now) != "old unit\n" || len(*calls) != 0 {
		t.Fatal(string(now), *calls)
	}
}

func TestGeneratedSystemdUnitsTimeoutStopSecCoversDefaultCronDrainFloor(t *testing.T) {
	sandboxServiceHomes(t)
	os.Unsetenv("ICEBATH_RESTART_DRAIN_TIMEOUT")
	os.Unsetenv("ICEBATH_CRON_DRAIN_TIMEOUT")
	swapSeam(t, &systemdRestartDrain, func(context.Context) (float64, error) { return 0, nil })
	swapSeam(t, &systemdCronDrain, func(context.Context) (float64, error) { return DefaultGatewayCronDrainTimeout, nil })
	unit := mustGenerateUnit(t, false, "")
	expected, _ := ResolveSystemdTimeoutStopSec(0.0, DefaultGatewayCronDrainTimeout, CronDrainCleanupReserveS, SystemdStopHeadroomS, SystemdTimeoutStopSecFloor)
	if !strings.Contains(unit, "TimeoutStopSec="+expected.String()) || expected.Int64() <= 60 {
		t.Fatal(expected, unit)
	}
	if !strings.Contains(unit, timeoutStopLine(t, DefaultGatewayRestartDrainTimeout, DefaultGatewayCronDrainTimeout)) {
		t.Fatal(unit)
	}
}

func TestGeneratedSystemdUnitsTimeoutStopSecKeepsTheFloorWhenCronDrainIsOptedOut(t *testing.T) {
	sandboxServiceHomes(t)
	swapSeam(t, &systemdRestartDrain, func(context.Context) (float64, error) { return 0, nil })
	swapSeam(t, &systemdCronDrain, func(context.Context) (float64, error) { return 0, nil })
	if unit := mustGenerateUnit(t, false, ""); !strings.Contains(unit, "TimeoutStopSec=60") {
		t.Fatal(unit)
	}
}

func TestGeneratedSystemdUnitsRestartExitCodeIsAlsoDeclaredASuccessStatus(t *testing.T) {
	sandboxServiceHomes(t)
	unit := mustGenerateUnit(t, false, "")
	for _, want := range []string{"SuccessExitStatus=75", "RestartForceExitStatus=75", "RestartPreventExitStatus=78"} {
		if !strings.Contains(unit, want) {
			t.Fatalf("%q lacks %q", unit, want)
		}
	}
}

func TestGeneratedSystemdUnitsUserUnitCarriesLdLibraryPathEscapedForSystemd(t *testing.T) {
	tmp := sandboxServiceHomes(t)
	swapSeam(t, &systemdUnitPathOf, func(context.Context, bool) (string, error) { return filepath.Join(tmp, "icebath-gateway.service"), nil })
	t.Setenv("LD_LIBRARY_PATH", `/opt/cu"da/lib64:/opt/back\slash/lib`)
	if unit := mustGenerateUnit(t, false, ""); !strings.Contains(unit, `Environment="LD_LIBRARY_PATH=/opt/cu\"da/lib64:/opt/back\\slash/lib"`) {
		t.Fatal(unit)
	}
	t.Setenv("LD_LIBRARY_PATH", "")
	if unit := mustGenerateUnit(t, false, ""); strings.Contains(unit, "LD_LIBRARY_PATH") {
		t.Fatal(unit)
	}
	os.Unsetenv("LD_LIBRARY_PATH")
	if unit := mustGenerateUnit(t, false, ""); strings.Contains(unit, "LD_LIBRARY_PATH") {
		t.Fatal(unit)
	}
}

// Pm installed state recording node and npm store entries under the root, returning their dirs
func seedPmNodeFacts(t *testing.T, root string) []string {
	t.Helper()
	store := filepath.Join(root, "tools")
	nodeDir, npmDir := filepath.Join(store, "node-v22.0.0"), filepath.Join(store, "npm-9.0.0", "bin")
	os.MkdirAll(nodeDir, 0o755)
	os.MkdirAll(npmDir, 0o755)
	facts, _ := json.Marshal(map[string]any{"schema": 1, "packages": map[string]any{
		"node": map[string]any{"entry": "node-v22.0.0", "version": "22.0.0", "env": map[string]any{"PATH": []string{"{{store}}/node-v22.0.0"}}},
		"npm":  map[string]any{"entry": "npm-9.0.0", "version": "9.0.0", "env": map[string]any{"PATH": []string{"{{store}}/npm-9.0.0/bin"}}},
	}})
	os.WriteFile(filepath.Join(store, "facts.json"), facts, 0o644)
	return []string{npmDir, nodeDir}
}

func TestSystemUnitHermesHomeManagedNodeMakesSystemUnitIndependentOfCallersPath(t *testing.T) {
	tmp := sandboxServiceHomes(t)
	targetHome := filepath.Join(tmp, "home", "alice")
	rootHome := filepath.Join(tmp, "root")
	rootIcebath := filepath.Join(rootHome, ".icebath")
	managed := seedPmNodeFacts(t, filepath.Join(targetHome, ".icebath"))
	os.MkdirAll(rootIcebath, 0o700)
	t.Setenv("HOME", rootHome)
	t.Setenv("ICEBATH_HOME", rootIcebath)
	aliceIdentity(t, targetHome)
	noServicePathDirs(t)
	t.Setenv("PATH", "/root/bin:"+os.Getenv("PATH"))
	rootUnit := mustGenerateUnit(t, true, "alice")
	t.Setenv("PATH", "/home/alice/.local/bin:"+os.Getenv("PATH"))
	if userUnit := mustGenerateUnit(t, true, "alice"); rootUnit != userUnit {
		t.Fatalf("%s\n%s", rootUnit, userUnit)
	}
	for _, dir := range managed {
		if !strings.Contains(rootUnit, dir) {
			t.Fatalf("%q lacks %q", rootUnit, dir)
		}
	}
	if strings.Contains(rootUnit, "/root/bin") {
		t.Fatal(rootUnit)
	}
}

func TestSystemUnitHermesHomeSystemUnitOrdersAfterTargetUserManager(t *testing.T) {
	tmp := sandboxServiceHomes(t)
	aliceIdentity(t, tmp)
	noServicePathDirs(t)
	systemUnit := mustGenerateUnit(t, true, "alice")
	userUnit := mustGenerateUnit(t, false, "")
	section, _, _ := strings.Cut(systemUnit, "[Service]")
	if !strings.Contains(section, "After=user@1001.service") || !strings.Contains(section, "Wants=user@1001.service") || strings.Contains(userUnit, "user@") {
		t.Fatalf("%s\n%s", systemUnit, userUnit)
	}
}

func TestSystemUnitHermesHomeInstalledUnitKeepsLdLibraryPathWhenTheShellLacksIt(t *testing.T) {
	tmp := sandboxServiceHomes(t)
	unitPath := filepath.Join(tmp, "icebath-gateway.service")
	swapSeam(t, &systemdUnitPathOf, func(context.Context, bool) (string, error) { return unitPath, nil })
	t.Setenv("LD_LIBRARY_PATH", "/opt/cuda/lib64:/opt/pct%dir/lib")
	os.WriteFile(unitPath, []byte(mustGenerateUnit(t, false, "")), 0o644)
	if raw, _ := os.ReadFile(unitPath); !strings.Contains(string(raw), `Environment="LD_LIBRARY_PATH=/opt/cuda/lib64:/opt/pct%%dir/lib"`) {
		t.Fatal(string(raw))
	}
	os.Unsetenv("LD_LIBRARY_PATH")
	if current, err := SystemdUnitIsCurrent(context.Background(), false); !current || err != nil {
		t.Fatal(current, err)
	}
	if unit := mustGenerateUnit(t, false, ""); !strings.Contains(unit, "LD_LIBRARY_PATH=/opt/cuda/lib64:/opt/pct%%dir/lib") {
		t.Fatal(unit)
	}
}

func TestSystemUnitHermesHomeSystemUnitRemapsCallerHomeLdLibraryPathComponents(t *testing.T) {
	sandboxServiceHomes(t)
	t.Setenv("HOME", "/root")
	os.Unsetenv("ICEBATH_HOME")
	aliceIdentity(t, "/home/alice")
	noServicePathDirs(t)
	t.Setenv("LD_LIBRARY_PATH", "/root/cuda/lib:/opt/cuda/lib64")
	if unit := mustGenerateUnit(t, true, "alice"); !strings.Contains(unit, `Environment="LD_LIBRARY_PATH=/home/alice/cuda/lib:/opt/cuda/lib64"`) {
		t.Fatal(unit)
	}
}

func TestSystemUnitHermesHomeSystemUnitUsesTargetUserHomeNotCallingUser(t *testing.T) {
	tmp := sandboxServiceHomes(t)
	callerHome, targetHome := filepath.Join(tmp, "root"), filepath.Join(tmp, "alice")
	os.Mkdir(callerHome, 0o700)
	os.Mkdir(targetHome, 0o700)
	t.Setenv("HOME", callerHome)
	t.Setenv("ICEBATH_HOME", filepath.Join(callerHome, ".icebath"))
	aliceIdentity(t, targetHome)
	swapSeam(t, &systemdUserLocalPaths, func(string, []string) []string { return nil })
	unit := mustGenerateUnit(t, true, "alice")
	if !strings.Contains(unit, "ICEBATH_HOME="+filepath.Join(targetHome, ".icebath")) || strings.Contains(unit, filepath.Join(callerHome, ".icebath")) {
		t.Fatal(unit)
	}
}

func TestSystemUnitHermesHomeUserUnitUnaffectedByChange(t *testing.T) {
	sandboxServiceHomes(t)
	resolved, _ := pypath.Resolve(os.Getenv("ICEBATH_HOME"))
	if unit := mustGenerateUnit(t, false, ""); !strings.Contains(unit, "ICEBATH_HOME="+resolved) {
		t.Fatal(unit)
	}
}

func TestSystemUnitRefreshSyncsHermesHomeRefreshAdoptsUnitHermesHomeBeforeRewriting(t *testing.T) {
	tmp := sandboxServiceHomes(t)
	rootHome, aliceHome := filepath.Join(tmp, "root"), filepath.Join(tmp, "alice")
	rootIcebath, aliceIcebath := filepath.Join(rootHome, ".icebath"), filepath.Join(aliceHome, ".icebath")
	os.MkdirAll(rootIcebath, 0o700)
	os.MkdirAll(aliceIcebath, 0o700)
	os.WriteFile(filepath.Join(rootIcebath, "config.yaml"), []byte("agent:\n  restart_drain_timeout: 60\n"), 0o644)
	os.WriteFile(filepath.Join(aliceIcebath, "config.yaml"), []byte("agent:\n  restart_drain_timeout: 180\n"), 0o644)
	unitPath := filepath.Join(tmp, "icebath-gateway.service")
	t.Setenv("HOME", rootHome)
	aliceIdentity(t, aliceHome)
	swapSeam(t, &systemdUserLocalPaths, func(string, []string) []string { return nil })
	swapSeam(t, &systemdUnitPathOf, func(context.Context, bool) (string, error) { return unitPath, nil })
	recordSystemctl(t)
	os.Unsetenv("ICEBATH_RESTART_DRAIN_TIMEOUT")
	t.Setenv("ICEBATH_HOME", aliceIcebath)
	good := mustGenerateUnit(t, true, "alice")
	if !strings.Contains(good, timeoutStopLine(t, 180, DefaultGatewayCronDrainTimeout)) {
		t.Fatal(good)
	}
	os.WriteFile(unitPath, []byte(good), 0o644)
	t.Setenv("ICEBATH_HOME", rootIcebath)
	if ok, err := refreshSystemdUnitIfNeeded(context.Background(), true); ok || err != nil {
		t.Fatal(ok, err)
	}
	if now, _ := os.ReadFile(unitPath); string(now) != good || os.Getenv("ICEBATH_HOME") != aliceIcebath {
		t.Fatal(string(now), os.Getenv("ICEBATH_HOME"))
	}
	if current, err := SystemdUnitIsCurrent(context.Background(), true); !current || err != nil {
		t.Fatal(current, err)
	}
}

func TestGeneratedUnitIncludesLocalBinSystemUnitIncludesLocalBinInPath(t *testing.T) {
	tmp := sandboxServiceHomes(t)
	aliceIdentity(t, tmp)
	swapSeam(t, &systemdUserLocalPaths, func(h string, _ []string) []string { return []string{filepath.Join(h, ".local", "bin")} })
	if unit := mustGenerateUnit(t, true, ""); !strings.Contains(unit, "/.local/bin") {
		t.Fatal(unit)
	}
}

func TestProfileArgSystemdUnitForTargetUserIncludesNamedProfile(t *testing.T) {
	tmp := sandboxServiceHomes(t)
	rootHome, targetHome := filepath.Join(tmp, "root"), filepath.Join(tmp, "home", "alice")
	rootProfile := filepath.Join(rootHome, ".icebath", "profiles", "mybot")
	os.MkdirAll(rootProfile, 0o700)
	t.Setenv("HOME", rootHome)
	t.Setenv("ICEBATH_HOME", rootProfile)
	aliceIdentity(t, targetHome)
	unit := mustGenerateUnit(t, true, "alice")
	for _, line := range strings.Split(unit, "\n") {
		if value, ok := strings.CutPrefix(line, "ExecStart="); ok {
			command, err := pyshlex.SplitPOSIX(value, false)
			if err != nil || len(command) < 4 || strings.Join(command[len(command)-4:], " ") != "--profile mybot gateway run" {
				t.Fatal(command, err)
			}
			return
		}
	}
	t.Fatal(unit)
}

func TestSystemUnitPathRemappingSystemUnitHasNoRootPaths(t *testing.T) {
	tmp := sandboxServiceHomes(t)
	rootHome := filepath.Join(tmp, "root")
	project := filepath.Join(rootHome, ".icebath", "icebath-agent")
	venvBin := filepath.Join(project, "venv", "bin")
	os.MkdirAll(venvBin, 0o755)
	os.WriteFile(filepath.Join(venvBin, "python"), nil, 0o644)
	t.Setenv("HOME", rootHome)
	t.Setenv("ICEBATH_HOME", filepath.Join(rootHome, ".icebath"))
	swapSeam(t, &serviceDefProjectRoot, func() string { return project })
	swapSeam(t, &systemdPythonPath, func() string { return filepath.Join(venvBin, "python") })
	aliceIdentity(t, "/home/alice")
	unit := mustGenerateUnit(t, true, "")
	if strings.Contains(unit, rootHome) || !strings.Contains(unit, "/home/alice") {
		t.Fatal(unit)
	}
	if !strings.Contains(unit, "WorkingDirectory=/home/alice/.icebath\n") || strings.Contains(unit, "WorkingDirectory=/home/alice/.icebath/icebath-agent") {
		t.Fatal(unit)
	}
}

func TestServiceWorkingDirIsStableUserUnitWorkingdirectoryIsHermesHomeNotCheckout(t *testing.T) {
	sandboxServiceHomes(t)
	h := os.Getenv("ICEBATH_HOME")
	for _, line := range strings.Split(mustGenerateUnit(t, false, ""), "\n") {
		if value, ok := strings.CutPrefix(line, "WorkingDirectory="); ok {
			got, _ := pypath.Resolve(value)
			want, _ := pypath.Resolve(h)
			if got != want || strings.Contains(value, "/.worktrees/") {
				t.Fatal(value)
			}
			return
		}
	}
	t.Fatal("unit has no WorkingDirectory line")
}

func TestServiceTakeoverGovernanceSystemdUnitDoesNotArmTakeover(t *testing.T) {
	sandboxServiceHomes(t)
	for _, line := range strings.Split(mustGenerateUnit(t, false, ""), "\n") {
		if strings.HasPrefix(line, "ExecStart=") {
			if strings.Contains(line, "--replace") {
				t.Fatal(line)
			}
			return
		}
	}
	t.Fatal("unit has no ExecStart line")
}

func TestGeneralizedSupervisorMarkersGeneratedSystemdUnitExportsSupervisedMarker(t *testing.T) {
	tmp := sandboxServiceHomes(t)
	h := filepath.Join(tmp, "home")
	os.Mkdir(h, 0o700)
	t.Setenv("ICEBATH_HOME", h)
	if unit := mustGenerateUnit(t, false, ""); !strings.Contains(unit, `Environment="ICEBATH_SUPERVISED_CHILD=1"`) {
		t.Fatal(unit)
	}
}
