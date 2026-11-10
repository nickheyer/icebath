package gateway

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Temp HOME and ICEBATH_HOME so no real unit or config is read or written
func sandboxServiceHomes(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	h := filepath.Join(tmp, ".icebath")
	os.MkdirAll(h, 0o700)
	t.Setenv("ICEBATH_HOME", h)
	return tmp
}

// Systemctl double recording each call's args and scope
func recordSystemctl(t *testing.T) *[][]string {
	t.Helper()
	calls := &[][]string{}
	swapSeam(t, &lifecycleRunSystemctl, func(_ context.Context, args []string, system bool, _ time.Duration) (CommandResult, error) {
		scope := "user"
		if system {
			scope = "system"
		}
		*calls = append(*calls, append([]string{scope}, args...))
		return CommandResult{}, nil
	})
	return calls
}

// Captured lifecycle stdout for the test's lifetime
func captureLifecycleOut(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	old := lifecycleOut
	lifecycleOut = &out
	t.Cleanup(func() { lifecycleOut = old })
	return &out
}

func TestStopMarkWritesPlannedStopForSupervisedPid(t *testing.T) {
	var marked []int
	swapSeam(t, &stopMarkWrite, func(_ context.Context, pid int) bool { marked = append(marked, pid); return true })
	t.Setenv("MAINPID", "4242")
	if SystemdStopMarkMain(context.Background(), []string{"systemd_stop_mark"}) != 0 || !slices.Equal(marked, []int{4242}) {
		t.Fatal(marked)
	}
	marked = nil
	os.Unsetenv("MAINPID")
	if SystemdStopMarkMain(context.Background(), []string{"systemd_stop_mark", "7777"}) != 0 || !slices.Equal(marked, []int{7777}) {
		t.Fatal(marked)
	}
}

func TestStopMarkNeverBlocksRestartAndUnitWiresExecStop(t *testing.T) {
	sandboxServiceHomes(t)
	var marked []int
	swapSeam(t, &stopMarkWrite, func(_ context.Context, pid int) bool { marked = append(marked, pid); return true })
	t.Setenv("MAINPID", "")
	os.Unsetenv("MAINPID")
	for _, argv := range [][]string{{"m"}, {"m", "0"}, {"m", "abc"}} {
		if SystemdStopMarkMain(context.Background(), argv) != 0 {
			t.Fatal(argv)
		}
	}
	if len(marked) != 0 {
		t.Fatal(marked)
	}
	swapSeam(t, &stopMarkWrite, func(context.Context, int) bool { panic(errors.New("disk gone")) })
	t.Setenv("MAINPID", "4242")
	if SystemdStopMarkMain(context.Background(), []string{"m"}) != 0 {
		t.Fatal("marker failure blocked the stop")
	}
	unit, err := GenerateSystemdUnit(context.Background(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unit, "ExecStop=-") || !strings.Contains(unit, SystemdStopMarkEntry) {
		t.Fatal(unit)
	}
	if !strings.Contains(unit, "ExecStopPost=-") || !strings.Contains(unit, CgroupCleanupEntry) {
		t.Fatal(unit)
	}
	if strings.Index(unit, SystemdStopMarkEntry) >= strings.Index(unit, CgroupCleanupEntry) {
		t.Fatal(unit)
	}
}

// Routes cgroup file reads of the named paths to replacements
func redirectCgroupReads(t *testing.T, route func(string) string) {
	t.Helper()
	swapSeam(t, &cgroupReadFile, func(p string) ([]byte, error) { return os.ReadFile(route(p)) })
}

// Kill double failing the test when any signal is sent
func forbidCgroupKill(t *testing.T, why string) {
	t.Helper()
	swapSeam(t, &cgroupKill, func(int) error { t.Fatal(why); return nil })
}

func TestOwnCgroupPathParsesV2CgroupPath(t *testing.T) {
	procSelf := filepath.Join(t.TempDir(), "cgroup")
	os.WriteFile(procSelf, []byte("0::/user.slice/user-1000.slice/icebath-gateway.service\n"), 0o644)
	redirectCgroupReads(t, func(p string) string {
		if p == "/proc/self/cgroup" {
			return procSelf
		}
		return p
	})
	if got, err := ownCgroupPath(); err != nil || got != "/user.slice/user-1000.slice/icebath-gateway.service" {
		t.Fatal(got, err)
	}
}

func TestReapCgroupNoopWhenProcsFileMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	redirectCgroupReads(t, func(p string) string {
		if strings.Contains(p, "cgroup.procs") {
			return missing
		}
		return p
	})
	forbidCgroupKill(t, "kill must not be called when cgroup.procs is unreadable")
	if killed, ok, err := ReapCgroup("/missing.slice/icebath-gateway.service"); killed != 0 || !ok || err != nil {
		t.Fatal(killed, ok, err)
	}
}

func TestMainRefusesWhenPid1ParentIsNotSystemd(t *testing.T) {
	comm := filepath.Join(t.TempDir(), "comm")
	os.WriteFile(comm, []byte("tini\n"), 0o644)
	swapSeam(t, &cgroupGetppid, func() int { return 1 })
	redirectCgroupReads(t, func(p string) string {
		if p == "/proc/1/comm" {
			return comm
		}
		return p
	})
	old := cgroupStderr
	cgroupStderr = &bytes.Buffer{}
	t.Cleanup(func() { cgroupStderr = old })
	forbidCgroupKill(t, "kill must not be called for a non-systemd PID 1 parent")
	if CgroupCleanupMain() != 1 {
		t.Fatal("non-systemd parent was authorized")
	}
}

func TestLiveGatewayGuardReapRefusesWhenLiveGatewayInCgroup(t *testing.T) {
	for _, verb := range []string{"run", "restart"} {
		t.Run(verb, func(t *testing.T) {
			cgroupPath := "/some.slice/some-gateway.service"
			gatewayCmdline := "/opt/icebath/bin/icebath gateway " + verb
			me := os.Getpid()
			swapSeam(t, &cgroupReadPids, func(string) ([]int, error) { return []int{777, me}, nil })
			swapSeam(t, &statusReadProcessCmdline, func(pid int) (string, bool) {
				if pid == 777 {
					return gatewayCmdline, true
				}
				return "", false
			})
			var stderr bytes.Buffer
			old := cgroupStderr
			cgroupStderr = &stderr
			t.Cleanup(func() { cgroupStderr = old })
			forbidCgroupKill(t, "kill must not signal a cgroup holding a live gateway")
			if killed, ok, err := ReapCgroup(cgroupPath); ok || killed != 0 || err != nil {
				t.Fatal(killed, ok, err)
			}
			swapSeam(t, &cgroupParentIsSystemd, func() (bool, error) { return true, nil })
			swapSeam(t, &cgroupOwnPath, func() (string, error) { return cgroupPath, nil })
			if CgroupCleanupMain() != 1 {
				t.Fatal("refusal exited zero")
			}
			swapSeam(t, &statusReadProcessCmdline, func(pid int) (string, bool) {
				if pid == 777 {
					return "bash -c adb forward tcp:8888 tcp:8889", true
				}
				return "", false
			})
			var killedPids []int
			swapSeam(t, &cgroupKill, func(pid int) error { killedPids = append(killedPids, pid); return nil })
			if killed, ok, err := ReapCgroup(cgroupPath); killed != 1 || !ok || err != nil || !slices.Equal(killedPids, []int{777}) {
				t.Fatal(killed, ok, err, killedPids)
			}
			reads := [][]int{{777, me}, {888, me}}
			swapSeam(t, &cgroupReadPids, func(string) ([]int, error) { next := reads[0]; reads = reads[1:]; return next, nil })
			killedPids = nil
			if killed, ok, err := ReapCgroup(cgroupPath); killed != 1 || !ok || err != nil || !slices.Equal(killedPids, []int{888}) {
				t.Fatal(killed, ok, err, killedPids)
			}
		})
	}
}

const legacyReplaceDropin = "# Added to end the gateway respawn storm: a stray lock-holder used to make the\n" +
	"# plain `gateway run` exit 1, and Restart=always turned that into thousands of\n" +
	"# restarts. `--replace` makes systemd's start reclaim the lock instead of\n" +
	"# crash-looping. Remove this file (and daemon-reload) to revert.\n" +
	"[Service]\n" +
	"ExecStart=\n" +
	"ExecStart=/usr/bin/icebath gateway run --replace\n"

// Current unit with a drop-in beside it and every refresh boundary doubled
func currentUnitWithDropin(t *testing.T, dropinText string) (string, string, *[][]string) {
	t.Helper()
	tmp := sandboxServiceHomes(t)
	unit := filepath.Join(tmp, "icebath-gateway.service")
	os.WriteFile(unit, []byte("[Unit]\nDescription=current\n"), 0o644)
	swapSeam(t, &systemdUnitPathOf, func(context.Context, bool) (string, error) { return unit, nil })
	swapSeam(t, &systemdGenerateUnit, func(context.Context, bool, string) (string, error) { return "[Unit]\nDescription=current\n", nil })
	swapSeam(t, &lifecycleSyncHome, func(context.Context, bool) error { return nil })
	calls := recordSystemctl(t)
	dropin := filepath.Join(filepath.Dir(unit), filepath.Base(unit)+".d", "20-replace.conf")
	os.Mkdir(filepath.Dir(dropin), 0o755)
	os.WriteFile(dropin, []byte(dropinText), 0o644)
	return unit, dropin, calls
}

func TestRefreshRetiresTheHermesReplaceDropinEvenWhenTheUnitTextIsCurrent(t *testing.T) {
	unit, dropin, calls := currentUnitWithDropin(t, legacyReplaceDropin)
	out := captureLifecycleOut(t)
	before, _ := os.ReadFile(unit)
	if ok, err := refreshSystemdUnitIfNeeded(context.Background(), true); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if _, err := os.Stat(dropin); err == nil {
		t.Fatal("drop-in survived")
	}
	if after, _ := os.ReadFile(unit); !bytes.Equal(after, before) {
		t.Fatal("the current unit itself is left alone")
	}
	if len(*calls) != 1 || !slices.Equal((*calls)[0], []string{"system", "daemon-reload"}) {
		t.Fatal(*calls)
	}
	if !strings.Contains(out.String(), "--replace drop-in") {
		t.Fatal(out.String())
	}
}

func TestRefreshKeepsAnOperatorWrittenDropinOfTheSameName(t *testing.T) {
	_, dropin, calls := currentUnitWithDropin(t, "[Service]\nExecStart=\nExecStart=/opt/icebath gateway run --replace\n")
	if ok, err := refreshSystemdUnitIfNeeded(context.Background(), false); ok || err != nil {
		t.Fatal(ok, err)
	}
	if _, err := os.Stat(dropin); err != nil || len(*calls) != 0 {
		t.Fatal(err, *calls)
	}
}

func TestStripOptionalSystemdDirectivesRemovesRestartMaxDelaySec(t *testing.T) {
	result := stripOptionalSystemdDirectives("[Service]\nRestart=always\nRestartSec=5\nRestartMaxDelaySec=300\nRestartSteps=5\n")
	if strings.Contains(result, "RestartMaxDelaySec") || strings.Contains(result, "RestartSteps") {
		t.Fatal(result)
	}
	if !strings.Contains(result, "Restart=always") || !strings.Contains(result, "RestartSec=5") {
		t.Fatal(result)
	}
}

// Installed unit text and generated text served through the unit seams
func serveSystemdUnitTexts(t *testing.T, installed *string, generated string) {
	t.Helper()
	tmp := sandboxServiceHomes(t)
	unit := filepath.Join(tmp, "icebath-gateway.service")
	if installed != nil {
		os.WriteFile(unit, []byte(*installed), 0o644)
	}
	swapSeam(t, &systemdUnitPathOf, func(context.Context, bool) (string, error) { return unit, nil })
	swapSeam(t, &systemdGenerateUnit, func(context.Context, bool, string) (string, error) { return generated, nil })
}

func TestSystemdUnitIsCurrentUnitWithoutFatalConfigRestartPolicyIsNotCurrent(t *testing.T) {
	expected := "[Service]\nRestart=always\nRestartForceExitStatus=75\nRestartPreventExitStatus=78\n"
	installed := strings.Replace(expected, "RestartPreventExitStatus=78\n", "", 1)
	serveSystemdUnitTexts(t, &installed, expected)
	if current, err := SystemdUnitIsCurrent(context.Background(), false); current || err != nil {
		t.Fatal(current, err)
	}
}

func TestSystemdUnitIsCurrentUnitWithoutOptionalDirectivesIsCurrent(t *testing.T) {
	installed := "[Unit]\nDescription=Icebath Gateway\n\n[Service]\nType=simple\nExecStart=/usr/bin/icebath gateway run\nRestart=always\nRestartSec=5\n\n[Install]\nWantedBy=default.target\n"
	serveSystemdUnitTexts(t, &installed, installed+"\nRestartMaxDelaySec=300\nRestartSteps=5\n")
	if current, err := SystemdUnitIsCurrent(context.Background(), false); !current || err != nil {
		t.Fatal(current, err)
	}
}

func TestSystemdUnitIsCurrentNonexistentUnitIsNotCurrent(t *testing.T) {
	serveSystemdUnitTexts(t, nil, "")
	if current, err := SystemdUnitIsCurrent(context.Background(), false); current || err != nil {
		t.Fatal(current, err)
	}
}

func TestPositiveWatchdogConfigGeneratesNotifyUnit(t *testing.T) {
	sandboxServiceHomes(t)
	swapSeam(t, &systemdLoadGatewayConfig, func(ctx context.Context) (*GatewayConfig, error) {
		return GatewayConfigFromDict(ctx, map[string]any{"systemd_watchdog_seconds": 120})
	})
	unit, err := GenerateSystemdUnit(context.Background(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Type=notify", "NotifyAccess=main", "WatchdogSec=120s"} {
		if !strings.Contains(unit, want) {
			t.Fatalf("%q lacks %q", unit, want)
		}
	}
}

func TestSystemUnitReadsWatchdogFromTargetHome(t *testing.T) {
	tmp := sandboxServiceHomes(t)
	callerHome, targetHome := filepath.Join(tmp, "caller"), filepath.Join(tmp, "target")
	os.Mkdir(callerHome, 0o700)
	os.Mkdir(targetHome, 0o700)
	os.WriteFile(filepath.Join(callerHome, "config.yaml"), []byte("gateway:\n  systemd_watchdog_seconds: 0\n"), 0o644)
	os.WriteFile(filepath.Join(targetHome, "config.yaml"), []byte("gateway:\n  systemd_watchdog_seconds: 75\n"), 0o644)
	t.Setenv("ICEBATH_HOME", callerHome)
	swapSeam(t, &systemdServiceIdentity, func(string) (string, string, string, int, error) {
		return "service", "service", filepath.Join(tmp, "account"), 1001, nil
	})
	swapSeam(t, &systemdTargetHome, func(context.Context, string) (string, error) { return targetHome, nil })
	unit, err := GenerateSystemdUnit(context.Background(), true, "service")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unit, "Type=notify") || !strings.Contains(unit, "WatchdogSec=75s") {
		t.Fatal(unit)
	}
}

// Unit pinning the real account home with every refresh boundary doubled
func pinnedUnitFixture(t *testing.T) (unit, real, scratch, original string, calls *[][]string) {
	t.Helper()
	tmp := sandboxServiceHomes(t)
	real = filepath.Join(tmp, "account", ".icebath")
	scratch = filepath.Join(tmp, "scratch", "fakehome", ".icebath")
	os.MkdirAll(real, 0o700)
	os.MkdirAll(scratch, 0o700)
	unit = filepath.Join(tmp, "systemd", "icebath-gateway.service")
	os.Mkdir(filepath.Dir(unit), 0o755)
	original = "[Service]\nExecStart=/home/ace/.icebath/icebath-agent/.icebath/bin/icebath gateway run\nWorkingDirectory=" + real + "\nEnvironment=\"ICEBATH_HOME=" + real + "\"\n"
	os.WriteFile(unit, []byte(original), 0o644)
	swapSeam(t, &systemdRefuseTempHome, func(string, string) bool { return false })
	swapSeam(t, &systemdUnitPathOf, func(context.Context, bool) (string, error) { return unit, nil })
	swapSeam(t, &systemdUnitCurrent, func(context.Context, bool) (bool, error) { return false, nil })
	swapSeam(t, &systemdRetireDropin, func(context.Context, bool) (bool, error) { return false, nil })
	swapSeam(t, &systemdPrepareLauncher, func(context.Context, bool, string) error { return nil })
	swapSeam(t, &systemdGenerateUnit, func(context.Context, bool, string) (string, error) { return "ExecStart=new\n", nil })
	calls = recordSystemctl(t)
	return
}

func TestRefreshFromAnotherHomeLeavesTheUnitUntouched(t *testing.T) {
	unit, _, scratch, original, calls := pinnedUnitFixture(t)
	out := captureLifecycleOut(t)
	t.Setenv("ICEBATH_HOME", scratch)
	if ok, err := refreshSystemdUnitIfNeeded(context.Background(), false); ok || err != nil {
		t.Fatal(ok, err)
	}
	if now, _ := os.ReadFile(unit); string(now) != original || len(*calls) != 0 {
		t.Fatal(string(now), *calls)
	}
	if !strings.Contains(out.String(), "Refusing to rewrite") {
		t.Fatal(out.String())
	}
}

func TestRefreshFromThePinnedHomeStillRewritesAStaleUnit(t *testing.T) {
	unit, real, _, _, calls := pinnedUnitFixture(t)
	captureLifecycleOut(t)
	t.Setenv("ICEBATH_HOME", real)
	if ok, err := refreshSystemdUnitIfNeeded(context.Background(), false); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if now, _ := os.ReadFile(unit); string(now) != "ExecStart=new\n" || !slices.ContainsFunc(*calls, func(c []string) bool { return slices.Equal(c[1:], []string{"daemon-reload"}) }) {
		t.Fatal(string(now), *calls)
	}
}
