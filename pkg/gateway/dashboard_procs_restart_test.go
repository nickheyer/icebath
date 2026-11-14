package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

// Recorded collaborators of one update path stop of a single manual or supervised backend
type restartHarness struct {
	out          bytes.Buffer
	unitRestarts [][2]string
	respawns     [][][]string
	launchd      [][3]any
}

// Pins the stop path on Linux with the given pids found, signals accepted and every stopped pid gone at once
func newRestartHarness(t *testing.T, pids []int, cgroup, service string, argv []string) *restartHarness {
	t.Helper()
	h := &restartHarness{}
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_DESKTOP_CHILD_PID", "")
	swapSeam(t, &dashboardGOOS, "linux")
	swapSeam(t, &dashboardRestartManaged, func(context.Context, io.Writer, string, string) bool { return false })
	swapSeam(t, &dashboardFindStale, func(context.Context, map[int]bool, string) []int { return slices.Clone(pids) })
	swapSeam(t, &dashboardLockOwnedServePIDs, func() map[int]bool { return map[int]bool{} })
	swapSeam(t, &dashboardPidCgroupPath, func(int) (string, bool) { return cgroup, cgroup != "" })
	swapSeam(t, &dashboardServiceForPid, func(context.Context, int) (string, bool) { return service, service != "" })
	swapSeam(t, &dashboardTryRestartUnit, func(_ context.Context, svc, cg string) bool {
		h.unitRestarts = append(h.unitRestarts, [2]string{svc, cg})
		return true
	})
	swapSeam(t, &dashboardCmdlineFor, func(context.Context, int) ([]string, bool) { return slices.Clone(argv), argv != nil })
	swapSeam(t, &dashboardHomeForPID, func(context.Context, int) (string, bool, error) { return "", false, nil })
	swapSeam(t, &dashboardRespawn, func(_ context.Context, _ io.Writer, commands [][]string) [][]string {
		h.respawns = append(h.respawns, commands)
		return [][]string{}
	})
	swapSeam(t, &dashboardLaunchdJobs, func(context.Context) []LaunchdBackendJob { return []LaunchdBackendJob{} })
	swapSeam(t, &dashboardAncestors, func(context.Context, int) []int { return []int{} })
	swapSeam(t, &dashboardPosixDescendants, func(context.Context, []int) []dashboardDescendant { return nil })
	swapSeam(t, &dashboardRunProbe, func(context.Context, []string, time.Duration) (*pysubprocess.Completed, error) {
		return &pysubprocess.Completed{}, nil
	})
	swapSeam(t, &dashboardKillPid, func(int, bool) error { return nil })
	swapSeam(t, &dashboardPidExists, func(int) bool { return false })
	swapSeam(t, &dashboardSleep, func(time.Duration) {})
	return h
}

func (h *restartHarness) run(restartManaged bool, already map[string]bool) DashboardKillResult {
	return KillStaleDashboardProcesses(context.Background(), &h.out, DefaultDashboardKillReason, restartManaged, already, "")
}

func TestSupervisedBackendRestartSupervisedPidRestartsOwningUnit(t *testing.T) {
	h := newRestartHarness(t, []int{4321}, "/system.slice/icebath-serve.service", "icebath-serve.service", nil)
	h.run(true, nil)
	if len(h.unitRestarts) != 1 || h.unitRestarts[0] != [2]string{"icebath-serve.service", "/system.slice/icebath-serve.service"} {
		t.Fatalf("restarts = %v", h.unitRestarts)
	}
	if strings.Contains(h.out.String(), "when you're ready") {
		t.Fatalf("out = %q", h.out.String())
	}
}

func TestSupervisedBackendRestartAlreadyRestartedUnitIsLeftUntouched(t *testing.T) {
	h := newRestartHarness(t, []int{4321}, "/system.slice/icebath-serve.service", "icebath-serve.service", nil)
	kills := 0
	swapSeam(t, &dashboardKillPid, func(int, bool) error { kills++; return nil })
	result := h.run(true, map[string]bool{"icebath-serve": true})
	if kills != 0 || len(h.unitRestarts) != 0 {
		t.Fatalf("kills = %d restarts = %v", kills, h.unitRestarts)
	}
	if !emptyKillResult(result) {
		t.Fatalf("result = %+v", result)
	}
}

func TestSupervisedBackendRestartOnlyTheUnitWhoseMainProcessIsTheBackendIsRestarted(t *testing.T) {
	for _, c := range []struct {
		name, mainPid string
		restarted     bool
	}{{"foreign-unit-cgroup", "991", false}, {"unit-main-process", "4321", true}} {
		t.Run(c.name, func(t *testing.T) {
			argv := []string{"icebath", "dashboard", "--port", "8300"}
			h := newRestartHarness(t, []int{4321}, "", "", argv)
			procRoot := t.TempDir()
			if err := os.MkdirAll(filepath.Join(procRoot, "4321"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(procRoot, "4321", "cgroup"), []byte("0::/system.slice/hosted-compute-agent.service\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			swapSeam(t, &dashboardProcRoot, procRoot)
			swapSeam(t, &dashboardPidCgroupPath, getPidCgroupPath)
			swapSeam(t, &dashboardServiceForPid, getSystemdServiceForPid)
			swapSeam(t, &dashboardTryRestartUnit, tryRestartSystemdService)
			var probes [][]string
			swapSeam(t, &dashboardRunProbe, func(_ context.Context, cmd []string, _ time.Duration) (*pysubprocess.Completed, error) {
				probes = append(probes, slices.Clone(cmd))
				out := ""
				if len(cmd) >= 2 && slices.Equal(cmd[len(cmd)-2:], []string{"--property=MainPID", "--value"}) {
					out = c.mainPid
				}
				return &pysubprocess.Completed{Stdout: []byte(out)}, nil
			})
			result := h.run(true, nil)
			restarts := [][]string{}
			for _, p := range probes {
				if slices.Contains(p, "restart") {
					restarts = append(restarts, p)
				}
			}
			if c.restarted {
				if len(restarts) != 1 || !slices.Equal(restarts[0], []string{"systemctl", "restart", "hosted-compute-agent.service"}) {
					t.Fatalf("restarts = %v", restarts)
				}
				if len(h.respawns) != 0 {
					t.Fatalf("respawns = %v", h.respawns)
				}
			} else {
				if len(restarts) != 0 {
					t.Fatalf("restarted a unit that does not supervise the dashboard: %v", restarts)
				}
				if len(h.respawns) != 1 || len(h.respawns[0]) != 1 || !slices.Equal(h.respawns[0][0], argv) {
					t.Fatalf("respawns = %v", h.respawns)
				}
			}
			if len(result.Unrecovered) != 0 {
				t.Fatalf("unrecovered = %v", result.Unrecovered)
			}
		})
	}
}

func TestManualBackendRespawnArgvCaptureFailureFallsBackToHint(t *testing.T) {
	h := newRestartHarness(t, []int{5555}, "", "", nil)
	result := h.run(true, nil)
	if len(h.respawns) != 0 || !slices.Equal(result.Unrecovered, []int{5555}) {
		t.Fatalf("respawns = %v unrecovered = %v", h.respawns, result.Unrecovered)
	}
}

func TestManualBackendRespawnNonOrphanFixedPortStillRespawns(t *testing.T) {
	argv := []string{"icebath", "dashboard", "--port", "8300"}
	h := newRestartHarness(t, []int{6001}, "", "", argv)
	h.run(true, nil)
	if len(h.respawns) != 1 || len(h.respawns[0]) != 1 || !slices.Equal(h.respawns[0][0], argv) {
		t.Fatalf("respawns = %v", h.respawns)
	}
	if strings.Contains(h.out.String(), "when you're ready") {
		t.Fatalf("out = %q", h.out.String())
	}
}

func TestManualBackendRespawnPortZeroServesKilledWithoutRespawn(t *testing.T) {
	argv := []string{"python", "-m", "icebath_cli.main", "serve", "--host", "127.0.0.1", "--port", "0"}
	h := newRestartHarness(t, []int{7001, 7002, 7003}, "", "", argv)
	result := h.run(true, nil)
	killed := slices.Clone(result.Killed)
	slices.Sort(killed)
	if len(h.respawns) != 0 || !slices.Equal(killed, []int{7001, 7002, 7003}) || len(result.Unrecovered) != 0 {
		t.Fatalf("respawns = %v result = %+v", h.respawns, result)
	}
	if strings.Contains(h.out.String(), "when you're ready") {
		t.Fatalf("out = %q", h.out.String())
	}
}

// Child of a respawn that is still running, or exited with the code when exited is set
type fakeRespawned struct {
	code   int
	exited bool
}

func (f fakeRespawned) Poll() (int, bool) { return f.code, f.exited }

// Records every detached spawn of a respawn pass and hands each the given child
func respawnSeams(t *testing.T, child respawnedDashboard, spawnErr error) *[][]string {
	t.Helper()
	t.Setenv("ICEBATH_HOME", filepath.Join(t.TempDir(), ".icebath"))
	spawned := &[][]string{}
	swapSeam(t, &dashboardSpawnDetached, func(command []string, _ string) (respawnedDashboard, error) {
		if spawnErr != nil {
			return nil, spawnErr
		}
		*spawned = append(*spawned, slices.Clone(command))
		return child, nil
	})
	swapSeam(t, &dashboardSleep, func(time.Duration) {})
	return spawned
}

func TestManualBackendRespawnRespawnAddsNoOpenToDashboardCommands(t *testing.T) {
	spawned := respawnSeams(t, fakeRespawned{}, nil)
	failed := respawnDashboardProcesses(context.Background(), &bytes.Buffer{}, [][]string{
		{"icebath", "dashboard", "--port", "8300"}, {"icebath", "serve", "--host", "0.0.0.0"}})
	if len(failed) != 0 {
		t.Fatalf("failed = %v", failed)
	}
	if !slices.Equal((*spawned)[0], []string{"icebath", "dashboard", "--port", "8300", "--no-open"}) ||
		!slices.Equal((*spawned)[1], []string{"icebath", "serve", "--host", "0.0.0.0"}) {
		t.Fatalf("spawned = %v", *spawned)
	}
}

func TestManualBackendRespawnRespawnFailureReturned(t *testing.T) {
	respawnSeams(t, nil, errors.New("no such file"))
	failed := respawnDashboardProcesses(context.Background(), &bytes.Buffer{}, [][]string{{"icebath", "serve"}})
	if len(failed) != 1 || !slices.Equal(failed[0], []string{"icebath", "serve"}) {
		t.Fatalf("failed = %v", failed)
	}
}

func TestManualBackendRespawnPreTakeoverInterpreterLauncherArgvIsRebuilt(t *testing.T) {
	spawned := respawnSeams(t, fakeRespawned{}, nil)
	swapSeam(t, &dashboardSelfExecutable, func() (string, error) { return "/opt/icebath/bin/icebath", nil })
	captured := []string{"/old/venv/bin/python", "/home/u/.local/bin/icebath", "dashboard", "--no-open", "--host", "127.0.0.1", "--port", "34553"}
	failed := respawnDashboardProcesses(context.Background(), &bytes.Buffer{}, [][]string{captured})
	expected := append([]string{"/opt/icebath/bin/icebath"}, captured[2:]...)
	if len(failed) != 0 || len(*spawned) != 1 || !slices.Equal((*spawned)[0], expected) {
		t.Fatalf("failed = %v spawned = %v", failed, *spawned)
	}
}

func TestManualBackendRespawnChildExitingWithinTheGraceWindowIsAReportedFailure(t *testing.T) {
	respawnSeams(t, fakeRespawned{code: 1, exited: true}, nil)
	var out bytes.Buffer
	argv := []string{"icebath", "dashboard", "--port", "8300"}
	failed := respawnDashboardProcesses(context.Background(), &out, [][]string{argv})
	if len(failed) != 1 || !slices.Equal(failed[0], argv) {
		t.Fatalf("failed = %v", failed)
	}
	if strings.Contains(out.String(), "✓ restarted") || !strings.Contains(out.String(), "✗ failed to restart") {
		t.Fatalf("out = %q", out.String())
	}
}

var launchdBackendArgv = []string{"/opt/icebath/venv/bin/python", "-m", "icebath_cli.main", "dashboard", "--host", "0.0.0.0", "--port", "9119", "--no-open", "--skip-build"}

// Runs one update path stop of a backend with the given loaded launchd jobs and kickstart outcome
func runLaunchdStop(t *testing.T, pid int, jobs []LaunchdBackendJob, restartOK bool, euid int) (*restartHarness, DashboardKillResult) {
	t.Helper()
	h := newRestartHarness(t, []int{pid}, "", "", launchdBackendArgv)
	swapSeam(t, &dashboardLaunchdJobs, func(context.Context) []LaunchdBackendJob { return jobs })
	swapSeam(t, &dashboardGeteuid, func() int { return euid })
	swapSeam(t, &dashboardRestartLaunchd, func(_ context.Context, domain, label string, oldPid int, haveOld bool, _ float64) bool {
		var old any
		if haveOld {
			old = oldPid
		}
		h.launchd = append(h.launchd, [3]any{domain, label, old})
		return restartOK
	})
	return h, h.run(true, nil)
}

func TestLaunchdSupervisedBackendsLaunchdOwnedBackendRestartsThroughLaunchdNeverAsADetachedRespawn(t *testing.T) {
	pid := func(n int) *int { return &n }
	h, result := runLaunchdStop(t, 9102, []LaunchdBackendJob{{Domain: "system", Label: "ai.icebath.dashboard", Argv: slices.Clone(launchdBackendArgv)}}, true, 0)
	if len(h.respawns) != 0 || len(h.launchd) != 1 || h.launchd[0] != [3]any{"system", "ai.icebath.dashboard", nil} {
		t.Fatalf("respawns = %v launchd = %v", h.respawns, h.launchd)
	}
	if !slices.Equal(result.Killed, []int{9102}) || len(result.Unrecovered) != 0 || strings.Contains(h.out.String(), "when you're ready") {
		t.Fatalf("result = %+v out = %q", result, h.out.String())
	}

	h, result = runLaunchdStop(t, 9103, []LaunchdBackendJob{{Domain: "gui/501", Label: "ai.icebath.dashboard", Argv: slices.Clone(launchdBackendArgv), LivePID: pid(9103)}}, false, 501)
	if len(h.respawns) != 0 || !slices.Equal(result.Unrecovered, []int{9103}) {
		t.Fatalf("respawns = %v unrecovered = %v", h.respawns, result.Unrecovered)
	}
	if !strings.Contains(h.out.String(), "run: launchctl kickstart -k gui/501/ai.icebath.dashboard") {
		t.Fatalf("out = %q", h.out.String())
	}

	h, _ = runLaunchdStop(t, 9105, []LaunchdBackendJob{{Domain: "system", Label: "ai.icebath.serve", Argv: slices.Clone(launchdBackendArgv)}}, false, 501)
	if !strings.Contains(h.out.String(), "run: sudo launchctl kickstart -k system/ai.icebath.serve") {
		t.Fatalf("out = %q", h.out.String())
	}

	other := LaunchdBackendJob{Domain: "gui/501", Label: "ai.icebath.other", Argv: []string{"icebath", "dashboard", "--port", "8300"}, LivePID: pid(777)}
	h, result = runLaunchdStop(t, 9104, []LaunchdBackendJob{other}, true, 501)
	if len(h.launchd) != 0 || len(h.respawns) != 1 || len(h.respawns[0]) != 1 || !slices.Equal(h.respawns[0][0], launchdBackendArgv) {
		t.Fatalf("launchd = %v respawns = %v", h.launchd, h.respawns)
	}
	if len(result.Unrecovered) != 0 {
		t.Fatalf("unrecovered = %v", result.Unrecovered)
	}
}

func TestLaunchdSupervisedBackendsLaunchdJobAttributionIsByLivePidAncestorOrExactArgv(t *testing.T) {
	uid := 501
	live := 4242
	backendArgv := []string{"/opt/icebath/venv/bin/python", "-m", "icebath_cli.main", "dashboard", "--port", "9119"}
	serveArgv := []string{"/opt/icebath/venv/bin/python", "-m", "icebath_cli.main", "serve", "--port", "8642"}
	jobs := []LaunchdBackendJob{
		{Domain: "user/" + strconv.Itoa(uid), Label: "ai.icebath.dashboard", Argv: backendArgv, LivePID: &live},
		{Domain: "system", Label: "ai.icebath.serve", Argv: serveArgv},
	}
	is := func(job *LaunchdBackendJob, label string) bool { return job != nil && job.Label == label }
	if !is(LaunchdJobOwningBackend(4242, []string{"something", "else"}, jobs, nil), "ai.icebath.dashboard") {
		t.Fatal("a job was not attributed by its live pid")
	}
	if !is(LaunchdJobOwningBackend(9999, []string{"python"}, jobs, []int{4242, 1}), "ai.icebath.dashboard") {
		t.Fatal("a job was not attributed by an ancestor")
	}
	if job := LaunchdJobOwningBackend(9999, serveArgv, jobs, nil); !is(job, "ai.icebath.serve") || job.LivePID != nil {
		t.Fatal("a job was not attributed by exact argv")
	}
	if !is(LaunchdJobOwningBackend(9999, append(slices.Clone(backendArgv), "--no-open"), jobs, nil), "ai.icebath.dashboard") {
		t.Fatal("the no open flag hid the owning job")
	}
	if LaunchdJobOwningBackend(9999, append(slices.Clone(serveArgv[:len(serveArgv)-1]), "8643"), jobs, nil) != nil {
		t.Fatal("a different argv was attributed")
	}
	if LaunchdJobOwningBackend(9999, nil, jobs, nil) != nil || LaunchdJobOwningBackend(4242, nil, []LaunchdBackendJob{}, nil) != nil {
		t.Fatal("an unreadable argv was attributed")
	}
}
