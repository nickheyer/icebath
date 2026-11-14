package gateway

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

const gatewayStandInScript = `touch "$STANDIN_READY"
parent=$PPID
i=0
while [ "$(ps -o ppid= -p $$ | tr -d ' ')" = "$parent" ] && [ $i -lt 1200 ]; do sleep 0.1; i=$((i+1)); done
`

func TestScanClaimsABareGatewayOnlyForTheHomeItsEnvironmentNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell lookalike")
	}
	tmp := t.TempDir()
	homeA, homeB := filepath.Join(tmp, "home-a"), filepath.Join(tmp, "home-b")
	mkdirAllT(t, homeA, homeB, filepath.Join(tmp, "bin"))
	script := filepath.Join(tmp, "bin", "icebath")
	if err := os.WriteFile(script, []byte(gatewayStandInScript), 0o755); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(tmp, "gateway.ready")
	oldService, oldHome := servicePIDsHook, homeForPIDHook
	t.Cleanup(func() { servicePIDsHook, homeForPIDHook = oldService, oldHome })
	servicePIDsHook = func(context.Context, bool) ([]int, error) { return nil, nil }
	cmd := exec.Command("/bin/sh", script, "gateway", "run")
	cmd.Env = append(os.Environ(), "ICEBATH_HOME="+homeA, "STANDIN_READY="+ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	homeForPIDHook = func(ctx context.Context, pid int, require bool) (string, bool, error) {
		if pid == cmd.Process.Pid {
			return HermesHomeForPID(ctx, pid, require)
		}
		return "", false, nil
	}
	deadline := time.Now().Add(15 * time.Second)
	for !fileExists(ready) {
		if time.Now().After(deadline) {
			t.Fatal("stand in gateway never came up")
		}
		time.Sleep(50 * time.Millisecond)
	}
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", homeB)
	if pids, err := FindGatewayPIDsFor(ctx, nil, false); err != nil || slices.Contains(pids, cmd.Process.Pid) {
		t.Fatalf("home b claimed the home a gateway %v %v", pids, err)
	}
	t.Setenv("ICEBATH_HOME", homeA)
	if pids, err := FindGatewayPIDsFor(ctx, nil, false); err != nil || !slices.Contains(pids, cmd.Process.Pid) {
		t.Fatalf("home a lost its gateway %v %v", pids, err)
	}
	if pids, err := FindGatewayPIDs(ctx, homeA); err != nil || !slices.Contains(pids, cmd.Process.Pid) {
		t.Fatalf("explicit home lost its gateway %v %v", pids, err)
	}
	if pids, err := FindGatewayPIDsFor(ctx, map[int]bool{cmd.Process.Pid: true}, true); err != nil || slices.Contains(pids, cmd.Process.Pid) {
		t.Fatalf("exclusion ignored %v %v", pids, err)
	}
}

func mkdirAllT(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// Pins the host predicates and the subprocess runner of the systemd support probe
func pinSystemdHost(t *testing.T, goos string, termux, wsl, container bool, which bool, run func(argv []string) (CommandResult, error)) {
	t.Helper()
	oldGOOS, oldT, oldW, oldC, oldLook, oldRun := serviceGOOS, hostIsTermux, hostIsWSL, hostIsContainer, serviceLookPath, RunServiceCommand
	t.Cleanup(func() {
		serviceGOOS, hostIsTermux, hostIsWSL, hostIsContainer, serviceLookPath, RunServiceCommand = oldGOOS, oldT, oldW, oldC, oldLook, oldRun
	})
	serviceGOOS = goos
	hostIsTermux = func() bool { return termux }
	hostIsWSL = func() bool { return wsl }
	hostIsContainer = func() bool { return container }
	serviceLookPath = func(name string) (string, error) {
		if which {
			return "/usr/bin/" + name, nil
		}
		return "", exec.ErrNotFound
	}
	RunServiceCommand = func(_ context.Context, _ time.Duration, argv ...string) (CommandResult, error) { return run(argv) }
}

func TestSupportsSystemdServicesDetection(t *testing.T) {
	ctx := context.Background()
	never := func(argv []string) (CommandResult, error) {
		t.Fatalf("unexpected systemctl call %v", argv)
		return CommandResult{}, nil
	}
	pinSystemdHost(t, "linux", false, false, false, false, never)
	if SupportsSystemdServices(ctx) {
		t.Fatal("systemctl missing must refuse")
	}
	pinSystemdHost(t, "linux", false, false, false, true, never)
	if !SupportsSystemdServices(ctx) {
		t.Fatal("systemctl present must accept")
	}
	pinSystemdHost(t, "linux", true, true, false, true, never)
	if SupportsSystemdServices(ctx) {
		t.Fatal("termux must stay excluded")
	}
	pinSystemdHost(t, "darwin", false, false, false, true, never)
	if SupportsSystemdServices(ctx) {
		t.Fatal("non linux must refuse")
	}
	var seen [][]string
	userOnly := func(argv []string) (CommandResult, error) {
		seen = append(seen, argv)
		if slices.Contains(argv, "--user") {
			return CommandResult{Stdout: "degraded\n"}, nil
		}
		return CommandResult{Stdout: "offline\n", ReturnCode: 1}, nil
	}
	pinSystemdHost(t, "linux", false, false, true, true, userOnly)
	if !SupportsSystemdServices(ctx) || len(seen) != 1 || !slices.Equal(seen[0], []string{"systemctl", "--user", "is-system-running"}) {
		t.Fatalf("container with a user manager %v", seen)
	}
	seen = nil
	pinSystemdHost(t, "linux", false, true, false, true, func(argv []string) (CommandResult, error) {
		seen = append(seen, argv)
		return CommandResult{Stdout: " Running \n"}, nil
	})
	if !SupportsSystemdServices(ctx) || len(seen) != 1 || slices.Contains(seen[0], "--user") {
		t.Fatalf("wsl with systemd probes the system scope %v", seen)
	}
	pinSystemdHost(t, "linux", false, true, false, true, func(argv []string) (CommandResult, error) {
		return CommandResult{}, ErrCommandNotFound
	})
	if SupportsSystemdServices(ctx) {
		t.Fatal("wsl without a reachable systemctl must refuse")
	}
}

func TestServicePIDsReadSystemdMainPIDsAndLaunchdFleet(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var calls []string
	pinSystemdHost(t, "linux", false, false, false, true, func(argv []string) (CommandResult, error) {
		calls = append(calls, strings.Join(argv, " "))
		switch {
		case slices.Contains(argv, "list-units") && slices.Contains(argv, "--user"):
			return CommandResult{Stdout: "icebath-gateway.service loaded active running x\nbogus.timer loaded\n"}, nil
		case slices.Contains(argv, "list-units"):
			return CommandResult{}, ErrCommandTimeout
		case slices.Contains(argv, "show"):
			return CommandResult{Stdout: "4242\n"}, nil
		}
		return CommandResult{}, nil
	})
	pids, err := getServicePIDs(ctx, false)
	if err != nil || !slices.Equal(pids, []int{4242}) {
		t.Fatalf("systemd pids %v %v %v", pids, err, calls)
	}
	name, err := GetServiceName(ctx)
	if err != nil || !strings.HasPrefix(calls[0], "systemctl --user list-units "+name+" ") {
		t.Fatalf("default scope pattern %q", calls[0])
	}
	calls = nil
	if _, err := getServicePIDs(ctx, true); err != nil || !strings.Contains(calls[0], "list-units icebath-gateway* ") {
		t.Fatalf("fleet pattern %v %v", calls, err)
	}
	pinSystemdHost(t, "darwin", false, false, false, true, func(argv []string) (CommandResult, error) {
		switch {
		case argv[1] == "print" && strings.HasPrefix(argv[2], "gui/"):
			return CommandResult{Stdout: "state = running\n\tpid = 77\n\tpid = 99\n"}, nil
		case argv[1] == "list" && len(argv) == 2:
			return CommandResult{Stdout: "PID\tStatus\tLabel\n88\t0\tai.icebath.gateway-coder\n-\t0\tai.icebath.gateway-idle\n5\t0\tcom.apple.x\n"}, nil
		}
		return CommandResult{ReturnCode: 113}, nil
	})
	if pids, err := getServicePIDs(ctx, false); err != nil || !slices.Equal(pids, []int{77}) {
		t.Fatalf("launchd own label %v %v", pids, err)
	}
	if pids, err := GetServicePIDs(ctx, true); err != nil || !slices.Equal(pids, []int{77, 88}) {
		t.Fatalf("launchd fleet %v %v", pids, err)
	}
}

func TestProcessListingParsers(t *testing.T) {
	cases := []struct {
		line string
		pid  int
		cmd  string
		ok   bool
	}{
		{"  123 /usr/bin/icebath gateway run", 123, "/usr/bin/icebath gateway run", true},
		{"user 456 0.0 0.1 1 2 ? S 10:00 0:00 icebath gateway run", 456, "icebath gateway run", true},
		{"999 grep icebath", 0, "", false},
		{"", 0, "", false},
		{"abc def", 0, "", false},
	}
	for _, c := range cases {
		pid, cmd, ok := parsePSLine(c.line)
		if pid != c.pid || cmd != c.cmd || ok != c.ok {
			t.Fatalf("%q -> %d %q %v", c.line, pid, cmd, ok)
		}
	}
	listing := "\r\nCommandLine=C:\\py\\pythonw.exe -m icebath gateway run\r\nProcessId=10\r\n\r\nCommandLine=\r\nProcessId=x\r\nProcessId=11\r\n"
	got := iterWindowsListProcesses(listing)
	if len(got) != 2 || got[0] != (pidCommand{10, `C:\py\pythonw.exe -m icebath gateway run`}) || got[1] != (pidCommand{11, ""}) {
		t.Fatalf("windows listing %v", got)
	}
	if pid, ok := ParseLaunchdPIDFromListOutput("{\n\t\"Label\" = \"x\";\n\t\"PID\" = 321;\n};"); !ok || pid != 321 {
		t.Fatalf("list pid %d %v", pid, ok)
	}
	if _, ok := ParseLaunchdPIDFromListOutput("\t\"PID\" = 0;"); ok {
		t.Fatal("crashed pid must not count")
	}
	old := scanPPID
	t.Cleanup(func() { scanPPID = old })
	scanPPID = func(pid int) (int, bool) { return map[int]int{20: 10, 30: 1}[pid], pid != 10 }
	if got := filterVenvLauncherStubs([]int{10, 20, 30}); !slices.Equal(got, []int{20, 30}) {
		t.Fatalf("stand in filter %v", got)
	}
}

func TestGatewayRuntimeLockPathSitsBesideThePidFile(t *testing.T) {
	h := t.TempDir()
	if got := GatewayRuntimeLockPath(h); got != filepath.Join(h, gatewayLockFilename) {
		t.Fatalf("lock path %q", got)
	}
}

func TestFindGatewayPIDsForReadsTheDefaultPidPathWhenTheLockDirIsOverridden(t *testing.T) {
	root, _ := stopRefusalHomes(t)
	locks := filepath.Join(t.TempDir(), "elsewhere", "gateway-locks")
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", locks)
	var asked []string
	swapSetup(t, &statusGetRunningPid, func(p string, cleanup bool) (int, bool, error) {
		if !cleanup {
			t.Fatal("ownership read asked for a pid without stale cleanup")
		}
		asked = append(asked, p)
		return 5151, true, nil
	})
	processHomes(t, func(int) (string, bool, error) { return root, true, nil })
	if got := gatewayPids(t); !slices.Equal(got, []int{5151}) {
		t.Fatalf("find_gateway_pids = %v", got)
	}
	if !slices.Equal(asked, []string{"", ""}) {
		t.Fatalf("get_running_pid asked with pid paths %q, want the unscoped default twice", asked)
	}
	if got := HostRecordPath(roleGateway); filepath.Dir(got) != locks {
		t.Fatalf("host record %q not under the overridden lock dir %q", got, locks)
	}
}
