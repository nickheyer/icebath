package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Reason a dashboard stop prints when the caller gives none
const DefaultDashboardKillReason = "the running backend no longer matches the updated frontend"

// Host facts of the dashboard stop path, swapped by tests
var (
	posixTermGraceSeconds       = 10.0
	posixDescendantGraceSeconds = 2.0
	dashboardKillPid            = killPid
	dashboardGetsid             = procGetsid
	dashboardStartTime          = GetProcessStartTime
	dashboardNow                = time.Now
	dashboardLockOwnedServePIDs = func() map[int]bool { return tools.LockOwnedServePIDs("") }
	dashboardGeteuid            = os.Geteuid
	dashboardGetppid            = os.Getppid
	dashboardProcExe            = proc.Exe
	dashboardStopForRelaunch    = pm.StopForRelaunch
	dashboardKillPidsWindows    = killPidsWindows
	dashboardKillPidsPosix      = killPidsPosix
	dashboardFindStale          = findStaleDashboardPIDs
	dashboardRestartManaged     = restartManagedDashboardService
	dashboardPidCgroupPath      = getPidCgroupPath
	dashboardServiceForPid      = getSystemdServiceForPid
	dashboardTryRestartUnit     = tryRestartSystemdService
	dashboardCmdlineFor         = dashboardCmdlineForPID
	dashboardRespawn            = respawnDashboardProcesses
	dashboardLaunchdJobs        = LoadedLaunchdBackendJobs
	dashboardRestartLaunchd     = restartLaunchdJob
	dashboardAncestors          = ProcessAncestors
)

// Captured run of a short command whose console window stays hidden on Windows
var dashboardRunHidden = func(ctx context.Context, argv []string, timeout time.Duration) (*pysubprocess.Completed, error) {
	hidden := &exec.Cmd{}
	hideConsole(hidden)
	return pysubprocess.RunWith(ctx, argv, pysubprocess.Options{Timeout: timeout, Attr: hidden.SysProcAttr})
}

// One pid a stop could not take down with the reason printed for it
type DashboardKillFailure struct {
	PID    int
	Reason string
}

// Pids a dashboard stop matched, stopped and failed on, plus the stopped ones nothing brought back
type DashboardKillResult struct {
	Matched     []int
	Killed      []int
	Failed      []DashboardKillFailure
	Unrecovered []int
}

// Result of a stop that matched nothing
func emptyResult() DashboardKillResult {
	return DashboardKillResult{Matched: []int{}, Killed: []int{}, Failed: []DashboardKillFailure{}}
}

// Pids Desktop marks as its live backends in the comma separated child pid variable
func excludePidsFromEnv() map[int]bool {
	out := map[int]bool{}
	for _, part := range strings.Split(os.Getenv("ICEBATH_DESKTOP_CHILD_PID"), ",") {
		if pid, ok := pyIntText(part); ok {
			out[pid] = true
		}
	}
	return out
}

// True when text names icebath at a path segment or token boundary
func textNamesHermes(text string) bool {
	tokens := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return pystr.IsSpace(r) || strings.ContainsRune("\\/=,;\"'", r)
	})
	for _, token := range tokens {
		if strings.HasPrefix(token, "icebath") || strings.HasPrefix(token, ".icebath") {
			return true
		}
	}
	return false
}

// True when the command line or executable of pid names icebath, false when either is unreadable
func processCommandIsHermes(pid int) bool {
	argv, err := dashboardProcCmdline(pid)
	if err != nil {
		return false
	}
	exe, err := dashboardProcExe(pid)
	if err != nil {
		return false
	}
	return textNamesHermes(strings.Join(argv, " ") + " " + exe)
}

// Whether taskkill may target pid, its start time unchanged since discovery and on Windows its command icebath
func pidIsHermes(pid int, expectedStartTime any) bool {
	if pid <= 0 {
		return false
	}
	if dashboardGOOS != "windows" {
		return expectedStartTime == nil || dashboardStartTime(pid) == expectedStartTime
	}
	current := dashboardStartTime(pid)
	if current == nil {
		return false
	}
	if expectedStartTime != nil && current != expectedStartTime {
		return false
	}
	return processCommandIsHermes(pid)
}

// Force kills each pid with taskkill after its start time and icebath identity are verified again
func killPidsWindows(ctx context.Context, pids []int, killed *[]int, failed *[]DashboardKillFailure) {
	startTimes := map[int]any{}
	for _, pid := range pids {
		startTimes[pid] = dashboardStartTime(pid)
	}
	for _, pid := range pids {
		expected := startTimes[pid]
		switch {
		case expected == nil:
			*failed = append(*failed, DashboardKillFailure{pid, "could not verify process identity"})
		case !pidIsHermes(pid, expected):
			*failed = append(*failed, DashboardKillFailure{pid, "not icebath-owned or process identity changed"})
		default:
			result, err := dashboardRunHidden(ctx, []string{"taskkill", "/PID", strconv.Itoa(pid), "/F"}, 10*time.Second)
			if err != nil {
				*failed = append(*failed, DashboardKillFailure{pid, killErrorText(err)})
				continue
			}
			if result.Returncode == 0 {
				*killed = append(*killed, pid)
				continue
			}
			detail := probeText(result.Stderr)
			if detail == "" {
				detail = probeText(result.Stdout)
			}
			*failed = append(*failed, DashboardKillFailure{pid, pystr.Strip(detail)})
		}
	}
}

// True for a process in its own session with no controlling terminal, the shape of a detached launch
func isDetachedSessionLeader(pid int, tty string) bool {
	if tty != "?" && tty != "??" && tty != "-" {
		return false
	}
	sid, err := dashboardGetsid(pid)
	return err == nil && sid == pid
}

// Dashboard owned descendant of a stop root with the start time read before the kill
type dashboardDescendant struct {
	pid, root int
	start     any
}

// True when every rune of a non empty text is a digit
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !pystr.IsDigit(r) {
			return false
		}
	}
	return true
}

// Descendants of the roots in discovery order, minus detached session leaders, this process and its ancestors
func posixDescendants(ctx context.Context, roots []int) []dashboardDescendant {
	result, err := dashboardRunProbe(ctx, []string{"ps", "-A", "-o", "pid=,ppid=,tty="}, 10*time.Second)
	if err != nil {
		return nil
	}
	type child struct {
		pid int
		tty string
	}
	children := map[int][]child{}
	parent := map[int]int{}
	for _, line := range pystr.Splitlines(probeText(result.Stdout), false) {
		parts := pystr.SplitWhitespace(line, -1)
		if len(parts) != 3 || !allDigits(parts[0]) || !allDigits(parts[1]) {
			continue
		}
		pid, okPID := pyIntText(parts[0])
		ppid, okPPID := pyIntText(parts[1])
		if !okPID || !okPPID {
			continue
		}
		children[ppid] = append(children[ppid], child{pid, parts[2]})
		parent[pid] = ppid
	}
	me := dashboardScanGetpid()
	ancestors := map[int]bool{}
	for cur := parent[me]; cur > 1 && !ancestors[cur]; cur = parent[cur] {
		ancestors[cur] = true
	}
	type step struct{ root, cur int }
	found := []dashboardDescendant{}
	seen := map[int]bool{}
	pending := []step{}
	for _, root := range roots {
		pending = append(pending, step{root, root})
	}
	for len(pending) > 0 {
		at := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		for _, c := range children[at.cur] {
			if seen[c.pid] || slices.Contains(roots, c.pid) || c.pid == me || isDetachedSessionLeader(c.pid, c.tty) {
				continue
			}
			if !ancestors[c.pid] {
				seen[c.pid] = true
				found = append(found, dashboardDescendant{c.pid, at.root, dashboardStartTime(c.pid)})
			}
			pending = append(pending, step{at.root, c.pid})
		}
	}
	return found
}

// Polls every tenth of a second up to the given seconds and returns the pids still alive
func waitGone(pids []int, seconds float64) []int {
	deadline := dashboardNow().Add(secondsDuration(seconds))
	alive := slices.Clone(pids)
	for len(alive) > 0 && dashboardNow().Before(deadline) {
		dashboardSleep(100 * time.Millisecond)
		still := alive[:0:0]
		for _, pid := range alive {
			if dashboardPidExists(pid) {
				still = append(still, pid)
			}
		}
		alive = still
	}
	return alive
}

// SIGTERM, a grace wait, SIGKILL for survivors, then the same for descendants that outlived their root
func killPidsPosix(ctx context.Context, pids []int, killed *[]int, failed *[]DashboardKillFailure) {
	descendants := posixDescendants(ctx, pids)
	send := func(pid int, force bool) {
		err := dashboardKillPid(pid, force)
		switch {
		case err == nil:
			if force {
				*killed = append(*killed, pid)
			}
		case errors.Is(err, os.ErrProcessDone):
			*killed = append(*killed, pid)
		default:
			*failed = append(*failed, DashboardKillFailure{pid, killErrorText(err)})
		}
	}
	for _, pid := range pids {
		send(pid, false)
	}
	pending := []int{}
	for _, pid := range pids {
		if !slices.Contains(*killed, pid) && !slices.ContainsFunc(*failed, func(f DashboardKillFailure) bool { return f.PID == pid }) {
			pending = append(pending, pid)
		}
	}
	alive := waitGone(pending, posixTermGraceSeconds)
	for _, pid := range pending {
		if !slices.Contains(alive, pid) {
			*killed = append(*killed, pid)
		}
	}
	for _, pid := range alive {
		send(pid, true)
	}
	survivors := []int{}
	for _, d := range descendants {
		if d.start != nil && dashboardStartTime(d.pid) == d.start {
			survivors = append(survivors, d.pid)
		}
	}
	for _, force := range []bool{false, true} {
		for _, pid := range survivors {
			_ = dashboardKillPid(pid, force)
		}
		survivors = waitGone(survivors, posixDescendantGraceSeconds)
	}
	for _, pid := range survivors {
		*failed = append(*failed, DashboardKillFailure{pid, "descendant of the stopped backend still alive after SIGKILL"})
	}
}

// Stops running dashboard and serve backends and, on the update path, brings supervised and manual ones back
func KillStaleDashboardProcesses(ctx context.Context, w io.Writer, reason string, restartManaged bool, alreadyRestartedUnits map[string]bool, scopeHome string) DashboardKillResult {
	dashUnit := strings.TrimSuffix(dashboardSystemdUnit, ".service")
	if restartManaged && !alreadyRestartedUnits[dashUnit] && dashboardRestartManaged(ctx, w, reason, dashboardSystemdUnit) {
		merged := map[string]bool{dashUnit: true}
		for unit := range alreadyRestartedUnits {
			merged[unit] = true
		}
		alreadyRestartedUnits = merged
	}
	exclude := excludePidsFromEnv()
	if restartManaged {
		for pid := range dashboardLockOwnedServePIDs() {
			exclude[pid] = true
		}
	}
	pids := dashboardFindStale(ctx, exclude, scopeHome)
	if len(pids) == 0 {
		return emptyResult()
	}
	pidCgroup := map[int]string{}
	pidService := map[int]string{}
	pidLaunchd := map[int]*LaunchdBackendJob{}
	pidCmdline := map[int][]string{}
	pidHome := map[int]string{}
	launchdJobs := []LaunchdBackendJob{}
	if dashboardGOOS != "windows" {
		launchdJobs = dashboardLaunchdJobs(ctx)
	}
	launchdOwner := func(pid int, cmdline []string) *LaunchdBackendJob {
		return LaunchdJobOwningBackend(pid, cmdline, launchdJobs, dashboardAncestors(ctx, pid))
	}
	if restartManaged && dashboardGOOS != "windows" {
		for _, pid := range pids {
			pidCgroup[pid], _ = dashboardPidCgroupPath(pid)
			pidService[pid], _ = dashboardServiceForPid(ctx, pid)
			if pidService[pid] != "" {
				continue
			}
			cmdline, _ := dashboardCmdlineFor(ctx, pid)
			if len(cmdline) == 0 {
				cmdline = nil
			}
			var job *LaunchdBackendJob
			if len(launchdJobs) > 0 {
				job = launchdOwner(pid, cmdline)
			}
			if job != nil {
				pidLaunchd[pid] = job
			} else if cmdline != nil {
				pidCmdline[pid] = cmdline
				if h, ok, err := dashboardHomeForPID(ctx, pid); err == nil && ok {
					pidHome[pid] = h
				}
			}
		}
		if len(alreadyRestartedUnits) > 0 {
			kept := []int{}
			for _, pid := range pids {
				if !alreadyRestartedUnits[strings.TrimSuffix(pidService[pid], ".service")] {
					kept = append(kept, pid)
				}
			}
			pids = kept
			if len(pids) == 0 {
				return emptyResult()
			}
		}
	} else if len(launchdJobs) > 0 {
		for _, pid := range pids {
			cmdline, _ := dashboardCmdlineFor(ctx, pid)
			if len(cmdline) == 0 {
				cmdline = nil
			}
			if job := launchdOwner(pid, cmdline); job != nil {
				pidLaunchd[pid] = job
			}
		}
	}
	fmt.Fprintf(w, "\n⟲ Stopping %d dashboard process(es) (%s)\n", len(pids), reason)
	killed := []int{}
	failed := []DashboardKillFailure{}
	if dashboardGOOS == "windows" {
		dashboardKillPidsWindows(ctx, pids, &killed, &failed)
	} else {
		dashboardKillPidsPosix(ctx, pids, &killed, &failed)
	}
	for _, pid := range killed {
		fmt.Fprintf(w, "    ✓ stopped PID %d\n", pid)
	}
	for _, f := range failed {
		fmt.Fprintf(w, "    ✗ failed to stop PID %d: %s\n", f.PID, f.Reason)
	}
	var unrecovered []int
	if len(killed) > 0 && restartManaged {
		unrecovered = restartKilledBackends(ctx, w, killed, pidService, pidCgroup, pidCmdline, pidHome, pidLaunchd)
	} else {
		unrecovered = slices.Clone(killed)
		targets := []string{}
		manual := false
		for _, pid := range killed {
			job := pidLaunchd[pid]
			if job == nil {
				manual = true
				continue
			}
			if target := job.Domain + "/" + job.Label; !slices.Contains(targets, target) {
				targets = append(targets, target)
			}
		}
		slices.Sort(targets)
		for _, target := range targets {
			fmt.Fprintf(w, "  ⚠ PID(s) supervised by launchd job %s: a KeepAlive job restarts itself.\n    To keep it down: launchctl bootout %s\n", target, target)
		}
		if manual {
			fmt.Fprintln(w, "  Restart the dashboard when you're ready:\n    icebath dashboard --port <port>")
		}
	}
	return DashboardKillResult{Matched: slices.Clone(pids), Killed: slices.Clone(killed), Failed: slices.Clone(failed), Unrecovered: slices.Clone(unrecovered)}
}

// Restarts systemd units, kickstarts launchd jobs and respawns manual argvs, returning the pids not brought back
func restartKilledBackends(ctx context.Context, w io.Writer, killed []int, pidService, pidCgroup map[int]string, pidCmdline map[int][]string,
	pidHome map[int]string, pidLaunchd map[int]*LaunchdBackendJob) []int {
	type failure struct{ name, err string }
	unrecovered := []int{}
	failedRestarts := []failure{}
	seenServices := map[string]bool{}
	candidates := []respawnCandidate{}
	for _, pid := range killed {
		svcName := pidService[pid]
		job := pidLaunchd[pid]
		switch {
		case svcName != "":
			if seenServices[svcName] {
				continue
			}
			seenServices[svcName] = true
			if dashboardTryRestartUnit(ctx, svcName, pidCgroup[pid]) {
				fmt.Fprintf(w, "    ✓ restarted systemd service %s\n", svcName)
			} else {
				failedRestarts = append(failedRestarts, failure{svcName, "systemctl restart returned non-zero"})
				unrecovered = append(unrecovered, pid)
			}
		case job != nil:
			target := job.Domain + "/" + job.Label
			if seenServices[target] {
				continue
			}
			seenServices[target] = true
			oldPid, haveOld := 0, job.LivePID != nil
			if haveOld {
				oldPid = *job.LivePID
			}
			if dashboardRestartLaunchd(ctx, job.Domain, job.Label, oldPid, haveOld, 15.0) {
				fmt.Fprintf(w, "    ✓ restarted launchd job %s\n", target)
			} else {
				sudo := ""
				if strings.HasPrefix(job.Domain, "system") && dashboardGeteuid() != 0 {
					sudo = "sudo "
				}
				failedRestarts = append(failedRestarts, failure{target,
					fmt.Sprintf("launchd is not supervising a fresh process; run: %slaunchctl kickstart -k %s", sudo, target)})
				unrecovered = append(unrecovered, pid)
			}
		default:
			if cmdline, ok := pidCmdline[pid]; ok {
				candidates = append(candidates, respawnCandidate{pid: pid, argv: cmdline, home: pidHome[pid]})
			} else {
				unrecovered = append(unrecovered, pid)
			}
		}
	}
	for _, f := range failedRestarts {
		fmt.Fprintf(w, "    ⚠ %s: %s\n", f.name, f.err)
	}
	respawnCmds := filterDashboardRespawnCandidates(ctx, candidates, nil)
	if len(respawnCmds) > 0 {
		failedCmds := dashboardRespawn(ctx, w, respawnCmds)
		for _, pid := range killed {
			cmdline, ok := pidCmdline[pid]
			if ok && slices.ContainsFunc(failedCmds, func(c []string) bool { return slices.Equal(c, cmdline) }) {
				unrecovered = append(unrecovered, pid)
			}
		}
	}
	if len(failedRestarts) > 0 || len(unrecovered) > 0 {
		fmt.Fprintln(w, "  Restart anything not auto-restarted when you're ready:\n    icebath dashboard --port <port>")
	}
	return unrecovered
}

// Lower cased symlink resolved executable path used to compare two installs
func normExe(path string) string {
	resolved, err := pypath.Resolve(path)
	if err != nil {
		return strings.ToLower(path)
	}
	return strings.ToLower(resolved)
}

// Stops old updaters for a relaunch in place of the retired scan for live launcher shims
func detectConcurrentHermesInstances(ctx context.Context, scriptsDir string, excludePID int) error {
	return dashboardStopForRelaunch(ctx)
}
