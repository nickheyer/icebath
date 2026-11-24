//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pydeadline"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Process probes of the holder scans and the Windows pause, the calls its tests replace
var (
	winIsWindows       = func() bool { return sysPlatform() == "win32" }
	winCmdline         = proc.Cmdline
	winExe             = proc.Exe
	winName            = proc.Name
	winCwd             = proc.Cwd
	winParents         = proc.Parents
	winPPID            = proc.PPID
	winBirth           = proc.Birth
	winSnapshot        = proc.Snapshot
	winPidExists       = proc.Exists
	winDetectHolders   = detectVenvPythonProcesses
	winDetectBinaries  = detectOtherIcebathProcesses
	winFindGatewayPids = func(ctx context.Context) ([]int, error) { return gateway.FindGatewayPIDsFor(ctx, nil, true) }
	winMonotonic       = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	winSleep           = func(seconds float64) { time.Sleep(time.Duration(seconds * float64(time.Second))) }
)

// One venv holder of the scan with its process name and full command line
type venvMatch struct {
	PID     int
	Name    string
	Cmdline string
}

// Result of fn, or the fallback after logging its failure at debug
func tryCall[T any](ctx context.Context, fn func() (T, error), fallback T, logMessage string, logArgs ...any) T {
	value, err := fn()
	if err != nil {
		pkgLog().DebugContext(ctx, fmt.Sprintf(logMessage, append(logArgs, launchErrorText(err))...))
		return fallback
	}
	return value
}

// RuntimeError of a mandatory step that failed, carrying its cause
type stepAborted struct {
	Msg   string
	Cause error
}

func (e *stepAborted) Error() string { return e.Msg }

func (e *stepAborted) Unwrap() error { return e.Cause }

// The failure of a mandatory step under its prefix, nil when the step passed
func abortOnError(prefix string, err error) error {
	if err == nil {
		return nil
	}
	return &stepAborted{Msg: prefix + ": " + launchErrorText(err), Cause: err}
}

// Marker path per gateway the socket pause will ask, the profile mapped ones outside any service
func plannedStopMarkers(runningPids []int, profileProcesses map[int]string, serviceGatewayPids map[int]bool) map[int]string {
	markers := map[int]string{}
	for _, pid := range runningPids {
		if served := profileProcesses[pid]; served != "" && !serviceGatewayPids[pid] {
			markers[pid] = plannedStopMarkerPath(served)
		}
	}
	return markers
}

// Waits for the gateway pids to exit and returns the survivors
func waitForWindowsUpdateGatewayExit(pids []int, timeout float64) map[int]bool {
	remaining := map[int]bool{}
	if len(pids) == 0 {
		return remaining
	}
	for _, pid := range pids {
		remaining[pid] = true
	}
	sweep := func() {
		for pid := range remaining {
			if !winPidExists(pid) {
				delete(remaining, pid)
			}
		}
	}
	pydeadline.PollUntil(func() (bool, bool) {
		sweep()
		return len(remaining) == 0, len(remaining) == 0
	}, time.Duration(math.Max(timeout, 0)*float64(time.Second)), 250*time.Millisecond)
	sweep()
	return remaining
}

// This process and its ancestors that are not gateways, the pids a holder scan never nominates
func selfAndNonGatewayAncestorPids() map[int]bool {
	skip := map[int]bool{os.Getpid(): true}
	ancestors, err := winParents(os.Getpid())
	if err != nil {
		return skip
	}
	for _, ancestor := range ancestors {
		if cmdline := cmdlineOrEmpty(ancestor); cmdline == "" || !gateway.LooksLikeGatewayCommandLine(cmdline) {
			skip[ancestor] = true
		}
	}
	return skip
}

// Joined argv of a process, empty when it cannot be read
func cmdlineOrEmpty(pid int) string {
	argv, err := winCmdline(pid)
	if err != nil {
		return ""
	}
	return strings.Join(argv, " ")
}

// Lower cased resolved path with one trailing separator, for prefix matching
func lowerDirPrefix(path string) string {
	raw, err := pypath.Resolve(path)
	if err != nil {
		raw = pypath.PathStr(path)
	}
	sep := string(os.PathSeparator)
	return strings.TrimRight(strings.ToLower(raw), sep) + sep
}

// Venv directory of the checkout the holder scans match against
func winVenvPrefix() string {
	root := fleetProjectRoot()
	venv, err := pm.ProjectVenvDir(root)
	if err != nil || venv == "" {
		venv = filepath.Join(root, "venv")
	}
	return lowerDirPrefix(venv)
}

// Live processes running from the project venv as pid, name and command line, never failing
func detectVenvPythonProcesses(ctx context.Context, excludePids map[int]bool) []venvMatch {
	if !winIsWindows() {
		return nil
	}
	venvPrefix, rootPrefix := winVenvPrefix(), lowerDirPrefix(fleetProjectRoot())
	skip := selfAndNonGatewayAncestorPids()
	for pid := range excludePids {
		skip[pid] = true
	}
	table, err := winSnapshot()
	if err != nil {
		return nil
	}
	var matches []venvMatch
	for _, row := range table {
		exe, err := winExe(row.PID)
		if err != nil || exe == "" || skip[row.PID] {
			continue
		}
		exeNorm := strings.ToLower(exe)
		if resolved, err := pypath.Resolve(exe); err == nil {
			exeNorm = strings.ToLower(resolved)
		}
		inVenv := strings.HasPrefix(exeNorm, venvPrefix)
		name, err := winName(row.PID)
		if err != nil || name == "" {
			name = pypath.PathName(exe)
		}
		nameLow := strings.ToLower(name)
		if !(inVenv || strings.HasPrefix(nameLow, "python") || strings.HasPrefix(nameLow, "pypy") ||
			nameLow == "uv.exe" || nameLow == "uvx.exe" || nameLow == "icebath.exe") {
			continue
		}
		cmdlineRaw := cmdlineOrEmpty(row.PID)
		cmdlineLow := strings.ToLower(cmdlineRaw)
		if inVenv || strings.Contains(cmdlineLow, venvPrefix) || (strings.Contains(cmdlineLow, "icebath_cli.main") &&
			(strings.Contains(cmdlineLow, rootPrefix) || strings.HasPrefix(cwdPrefix(row.PID), rootPrefix))) {
			matches = append(matches, venvMatch{PID: row.PID, Name: name, Cmdline: cmdlineRaw})
		}
	}
	return matches
}

// Live processes running this install's icebath executable, other than this process and its non gateway ancestors
func detectOtherIcebathProcesses(ctx context.Context) []venvMatch {
	if !winIsWindows() {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		pkgLog().DebugContext(ctx, "Could not read this executable's path for the running icebath scan: "+launchErrorText(err))
		return nil
	}
	selfNorm := strings.ToLower(self)
	if resolved, err := pypath.Resolve(self); err == nil {
		selfNorm = strings.ToLower(resolved)
	}
	table, err := winSnapshot()
	if err != nil {
		pkgLog().DebugContext(ctx, "Could not list processes for the running icebath scan: "+launchErrorText(err))
		return nil
	}
	skip := selfAndNonGatewayAncestorPids()
	var matches []venvMatch
	for _, row := range table {
		if skip[row.PID] {
			continue
		}
		exe, err := winExe(row.PID)
		if err != nil || exe == "" {
			continue
		}
		exeNorm := strings.ToLower(exe)
		if resolved, err := pypath.Resolve(exe); err == nil {
			exeNorm = strings.ToLower(resolved)
		}
		if exeNorm != selfNorm {
			continue
		}
		name, err := winName(row.PID)
		if err != nil || name == "" {
			name = pypath.PathName(exe)
		}
		matches = append(matches, venvMatch{PID: row.PID, Name: name, Cmdline: cmdlineOrEmpty(row.PID)})
	}
	return matches
}

// Refuses a Windows update while processes outside the paused gateways hold the install: other icebath processes unless --force, Python environment holders unless --force-venv, which --force never covers
func refuseHeldInstall(ctx context.Context, w io.Writer, opts *updateOptions) error {
	if !winIsWindows() {
		return nil
	}
	binaries, interpreters := winDetectBinaries(ctx), winDetectHolders(ctx, nil)
	refused := false
	report := func(holders []venvMatch, forced bool, flag, key string) {
		if len(holders) == 0 {
			return
		}
		mark := "✗"
		if forced {
			mark = "⚠"
		}
		fmt.Fprintln(w, i18n.T(ctx, key, "", map[string]any{"mark": mark, "count": len(holders)}))
		for _, holder := range holders {
			fmt.Fprintf(w, "    PID %d  %s\n", holder.PID, holder.Cmdline)
		}
		if forced {
			fmt.Fprintln(w, i18n.T(ctx, "cli.update.held_forced", "", map[string]any{"flag": flag}))
			return
		}
		fmt.Fprintln(w, i18n.T(ctx, "cli.update.held_refused", "", map[string]any{"flag": flag}))
		refused = true
	}
	report(binaries, opts.Force, "--force", "cli.update.held_icebath")
	report(interpreters, opts.ForceVenv, "--force-venv", "cli.update.held_venv")
	if refused {
		recordStop(ctx, "install_held")
		return updateExit(venvHoldersExit)
	}
	return nil
}

// Lower cased working directory of a process with one trailing separator, the bare separator when unreadable
func cwdPrefix(pid int) string {
	sep := string(os.PathSeparator)
	cwd, err := winCwd(pid)
	if err != nil {
		return sep
	}
	return strings.TrimRight(strings.ToLower(cwd), sep) + sep
}

// Venv interpreter parents of the pids that hold the install open, one hop up only
func venvLauncherAncestors(pids []int) []int {
	if !winIsWindows() || len(pids) == 0 {
		return nil
	}
	venvPrefix := winVenvPrefix()
	skip := selfAndNonGatewayAncestorPids()
	for _, pid := range pids {
		skip[pid] = true
	}
	var found []int
	for _, pid := range pids {
		parent, ok := winPPID(pid)
		if !ok || parent <= 0 || !winPidExists(parent) || skip[parent] {
			continue
		}
		seen := false
		for _, known := range found {
			seen = seen || known == parent
		}
		exe, err := winExe(parent)
		if seen || err != nil || !strings.HasPrefix(strings.ToLower(exe), venvPrefix) {
			continue
		}
		found = append(found, parent)
	}
	return found
}

// Whether a command line is a gateway the update can pause
func winIsPausableGateway(cmdline string) bool {
	return pm.LooksLikeGatewayCommandLine != nil && pm.LooksLikeGatewayCommandLine(cmdline)
}

// Machine readable class of one venv holder, gateway, a subcommand or python
func venvHolderKind(cmdline string) string {
	if winIsPausableGateway(cmdline) {
		return "gateway"
	}
	if subcommand, ok := icebathHolderSubcommand(cmdline); ok && subcommand != "" {
		return "icebath:" + subcommand
	}
	return "python"
}

// Status of the holder listing when holders remain
const venvHoldersExit = 3

// Pid, exe, argv and kind of every process the venv holder guard would refuse on
func listVenvHolders(ctx context.Context) []any {
	holders := []any{}
	for _, match := range winDetectHolders(ctx, nil) {
		exe, argv := match.Name, match.Cmdline
		if live, err := winExe(match.PID); err == nil {
			if live != "" {
				exe = live
			}
			if liveArgv, err := winCmdline(match.PID); err == nil {
				if joined := strings.Join(liveArgv, " "); joined != "" {
					argv = joined
				}
			}
		}
		holders = append(holders, map[string]any{"pid": match.PID, "exe": exe, "argv": argv, "kind": venvHolderKind(argv)})
	}
	return holders
}
