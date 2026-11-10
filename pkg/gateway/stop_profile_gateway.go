package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

const (
	orphanExitGraceSeconds    = 30.0
	orphanExitPollSeconds     = 0.2
	reaperSupervisorWalkLimit = 12
)

// Task Scheduler states that still own a gateway, Ready and Queued included
var windowsTaskSupervisorStates = map[string]bool{"Running": true, "Ready": true, "Queued": true}

// Process boundaries of the stop and reap paths, seams tests replace like the patched gateway module
var (
	stopKill            = killPid
	stopMarkPlannedStop = markPlannedStop
	stopSleep           = time.Sleep
	stopRemovePidFile   = RemovePidFile
	stopReapOrphans     = reapUnsupervisedGatewayOrphans
	stopDrainPid        = drainWindowsGatewayPid
	stopDrainTimeout    = windowsStopDrainTimeout
	stopForceTerminate  = forceTerminateKnownGatewayPids
	reapTaskState       = windowsScheduledTaskState
	reapProcessAge      = processAgeSeconds
	reapParent          = updaterecord.PsutilParent
	reapProcessName     = proc.Name
	reapFindGatewayPIDs = FindGatewayPIDsFor
)

// Stops only this profile's own gateway through its pid file then reaps orphans, true when a process was stopped
func StopProfileGateway(ctx context.Context) (bool, error) {
	pid, owned := ownedGatewayPID(ctx, "", processHermesHome())
	if !owned {
		_, running, err := statusGetRunningPid("", true)
		if err != nil {
			return false, err
		}
		if running {
			return false, nil
		}
		return stopReapOrphans(ctx, nil, 0)
	}
	if IsWindows() {
		expectedStart := GetProcessStartTime(pid)
		if !stopDrainPid(ctx, pid, stopDrainTimeout(ctx)) {
			stopForceTerminate([]gatewayPidIdentity{{pid, expectedStart}})
		}
	} else {
		stopMarkPlannedStop(ctx, pid, true)
		if err := stopKill(pid, false); err != nil {
			switch {
			case errors.Is(err, os.ErrProcessDone):
			case errors.Is(err, os.ErrPermission):
				lifecyclePrintf("⚠ Permission denied to kill PID %d\n", pid)
				return false, nil
			default:
				return false, err
			}
		}
	}
	for range 20 {
		if !statusPidExists(pid) {
			break
		}
		stopSleep(500 * time.Millisecond)
	}
	if _, running, err := statusGetRunningPid("", true); err != nil {
		return false, err
	} else if !running {
		stopRemovePidFile()
	}
	if _, err := stopReapOrphans(ctx, map[int]bool{pid: true}, 0); err != nil {
		slog.Debug(fmt.Sprintf("orphan reap after stop_profile_gateway failed: %v", err))
	}
	return true, nil
}

// Kills gateway processes no supervisor or record can see, sparing candidates younger than the grace
func reapUnsupervisedGatewayOrphans(ctx context.Context, extraExclude map[int]bool, minAgeS float64) (bool, error) {
	if setupSupportsSystemd(ctx) {
		return false, nil
	}
	if IsWindows() {
		taskName, err := winTaskName(ctx)
		if err != nil {
			taskName = windowsTaskNameDefault
		}
		if windowsScheduledTaskSupervises(ctx, taskName) {
			return false, nil
		}
	}
	own := reaperExclusionPids(ctx, extraExclude)
	found, err := reapFindGatewayPIDs(ctx, own, false)
	if err != nil {
		return false, nil
	}
	var orphans []int
	for _, p := range found {
		if p > 0 && !reaperCandidateIsSupervisorOwned(p) {
			orphans = append(orphans, p)
		}
	}
	if minAgeS > 0 {
		var aged []int
		for _, p := range orphans {
			if age, err := reapProcessAge(p); err == nil && age >= minAgeS {
				aged = append(aged, p)
			}
		}
		orphans = aged
	}
	if len(orphans) == 0 {
		return false, nil
	}
	identity := map[int]any{}
	for _, p := range orphans {
		if start := GetProcessStartTime(p); start != nil {
			identity[p] = start
		}
	}
	reaped := false
	for _, p := range orphans {
		lifecycleWritePlannedStop(ctx, p)
		if IsWindows() {
			reaped = true
			continue
		}
		if err := stopKill(p, false); err != nil {
			switch {
			case errors.Is(err, os.ErrProcessDone):
			case errors.Is(err, os.ErrPermission):
				lifecyclePrintf("⚠ Permission denied to kill orphaned gateway PID %d\n", p)
			default:
				return reaped, err
			}
			continue
		}
		reaped = true
	}
	var stale []int
	for _, p := range awaitGatewayExit(orphans, statusPidExists, nil, orphanExitGraceSeconds, orphanExitPollSeconds) {
		if start, ok := identity[p]; ok && pyEq(GetProcessStartTime(p), start) {
			stale = append(stale, p)
		}
	}
	forceKillSurvivors(stale, nil)
	return reaped, nil
}

// Pids the orphan reaper never kills, this process, caller extras, service managed and recorded gateways with their parents
func reaperExclusionPids(ctx context.Context, extraExclude map[int]bool) map[int]bool {
	own := map[int]bool{os.Getpid(): true}
	for p := range extraExclude {
		own[p] = true
	}
	if service, err := servicePIDsHook(ctx, true); err == nil {
		for _, p := range service {
			own[p] = true
		}
	}
	var recorded []int
	for _, record := range []*pyjson.Object{readPidRecord(""), readGatewayLockRecord("")} {
		if p, ok := pidFromRecord(record, "pid"); ok {
			recorded = append(recorded, p)
		}
	}
	running, ok, err := statusGetRunningPid("", false)
	if err != nil {
		return own
	}
	if ok {
		recorded = append(recorded, running)
	}
	for _, p := range recorded {
		if p <= 0 {
			continue
		}
		own[p] = true
		for parent, ok, err := reapParent(p); err == nil && ok; parent, ok, err = reapParent(parent) {
			own[parent] = true
		}
	}
	return own
}

// Polls up to the grace for the pids to exit, rechecking after the last sleep, returning the survivors
func awaitGatewayExit(pids []int, pidExists func(int) bool, sleep func(seconds float64), graceS, pollS float64) []int {
	if sleep == nil {
		sleep = func(seconds float64) { stopSleep(time.Duration(seconds * float64(time.Second))) }
	}
	alive := func(in []int) []int {
		var out []int
		for _, p := range in {
			if pidExists(p) {
				out = append(out, p)
			}
		}
		return out
	}
	survivors := append([]int(nil), pids...)
	for range max(1, int(graceS/pollS)) {
		if survivors = alive(survivors); len(survivors) == 0 {
			return survivors
		}
		sleep(pollS)
	}
	return alive(survivors)
}

// Force kills processes that outlasted the grace, logging each so a torn store leaves a trace
func forceKillSurvivors(survivors []int, kill func(pid int) error) {
	if kill == nil {
		kill = func(pid int) error { return stopKill(pid, true) }
	}
	for _, pid := range survivors {
		slog.Warn(fmt.Sprintf("Gateway PID %d did not exit within %.0fs of the stop request (SIGTERM, or the planned-stop "+
			"marker on Windows) — sending SIGKILL. A kill during a WAL checkpoint can corrupt state.db; "+
			"the next start will run an integrity check.", pid, orphanExitGraceSeconds))
		_ = kill(pid)
	}
}

// Whether the pid's parent chain reaches services.exe on Windows, never on POSIX where every chain ends at init
func reaperCandidateIsSupervisorOwned(pid int) bool {
	if !IsWindows() {
		return false
	}
	parent, ok, err := reapParent(pid)
	for range reaperSupervisorWalkLimit {
		if err != nil || !ok {
			break
		}
		if name, nerr := reapProcessName(parent); nerr == nil && pystr.Lower(name) == "services.exe" {
			return true
		}
		parent, ok, err = reapParent(parent)
	}
	return false
}

// Whether Task Scheduler still owns this profile's gateway, any probe failure reading as not supervised
func windowsScheduledTaskSupervises(ctx context.Context, taskName string) bool {
	state, ok := reapTaskState(ctx, taskName)
	return ok && windowsTaskSupervisorStates[state]
}

// Locale independent Task Scheduler state through the COM API, false on any failure
func windowsScheduledTaskState(ctx context.Context, taskName string) (string, bool) {
	if !IsWindows() {
		return "", false
	}
	quoted := strings.ReplaceAll(taskName, "'", "''")
	psCmd := "$ErrorActionPreference = 'Stop'; " +
		"$s = [Activator]::CreateInstance([type]::GetTypeFromProgID('Schedule.Service')); " +
		"$s.Connect(); " +
		"try { " +
		"$t = $s.GetFolder('\\').GetTask('" + quoted + "'); " +
		"@('Unknown', 'Disabled', 'Queued', 'Ready', 'Running')[[int]$t.State] " +
		"} catch { " +
		"$e = $_.Exception; while ($e.InnerException) { $e = $e.InnerException }; " +
		"if ($e.HResult -in @(-2147024894, -2147024893)) { 'MISSING' } else { throw } " +
		"}"
	shell, err := serviceLookPath("powershell")
	if err != nil {
		if shell, err = serviceLookPath("pwsh"); err != nil {
			return "", false
		}
	}
	res, err := runHiddenCommand(ctx, 10*time.Second, shell, "-NoProfile", "-NonInteractive", "-Command", psCmd)
	if err != nil || res.ReturnCode != 0 {
		return "", false
	}
	state := pystr.Strip(res.Stdout)
	return state, state != ""
}

// Seconds since the process started by its psutil create time, never negative
func processAgeSeconds(pid int) (float64, error) {
	birth, err := proc.Birth(pid)
	if err != nil {
		return 0, err
	}
	return math.Max(0, float64(time.Now().UnixNano())/1e9-birth), nil
}

// Runs argv with a hidden console and decodes its output dropping invalid bytes, any failure returned as the error
var runHiddenCommand = func(ctx context.Context, timeout time.Duration, argv ...string) (CommandResult, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.WaitDelay = time.Second
	hideConsole(cmd)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if cctx.Err() != nil {
		return CommandResult{}, fmt.Errorf("%w: %q after %v", ErrCommandTimeout, argv, timeout)
	}
	res := CommandResult{Stdout: strings.ToValidUTF8(out.String(), "")}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ReturnCode = exitErr.ExitCode()
		return res, nil
	}
	return res, err
}
