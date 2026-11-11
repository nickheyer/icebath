package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pydeadline"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

var backendPurposes = map[string]bool{"serve": true, "dashboard": true}

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
	winLedgerEntries   = func() []map[string]any { return tools.LedgerEntries(tools.LedgerOptions{}) }
	winSpawnerIsDead   = tools.SpawnerIsDead
	winDetectHolders   = detectVenvPythonProcesses
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
		updateLog.DebugContext(ctx, fmt.Sprintf(logMessage, append(logArgs, launchErrorText(err))...))
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

// True when a process has a running parent that is not a recycled pid
func parentIsLive(pid int) (bool, error) {
	parent, ok := winPPID(pid)
	if !ok {
		return false, proc.ErrNoSuchProcess
	}
	if parent <= 0 || !winPidExists(parent) {
		return false, nil
	}
	parentBorn, err := winBirth(parent)
	if err != nil {
		return false, err
	}
	born, err := winBirth(pid)
	if err != nil {
		return false, err
	}
	return parentBorn <= born, nil
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

// Whether a command line is the pausable gateway the Desktop preflight exempts
func winIsPausableGateway(cmdline string) bool {
	return pm.LooksLikeGatewayCommandLine != nil && pm.LooksLikeGatewayCommandLine(cmdline)
}

// Machine readable class of one venv holder, gateway, backend, a hermes subcommand or python
func venvHolderKind(cmdline string) string {
	if winIsPausableGateway(cmdline) {
		return "gateway"
	}
	subcommand, ok := hermesHolderSubcommand(cmdline)
	switch {
	case ok && backendPurposes[subcommand]:
		return "backend"
	case ok && subcommand != "":
		return "hermes:" + subcommand
	}
	return "python"
}

// Status of the holder listing when holders remain
const venvHoldersExit = 3

// Live argv of a holder joined, the scanned command line when it cannot be read or is empty
func winLiveArgv(pid int, cmdline string) string {
	if argv, err := winCmdline(pid); err == nil && len(argv) > 0 {
		if joined := strings.Join(argv, " "); joined != "" {
			return joined
		}
	}
	return cmdline
}

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

// Pids of the holders when every one is a pausable gateway, nil with false when any is not
func leftoverPausableGatewayPids(matches []venvMatch) ([]int, bool) {
	pids := []int{}
	for _, match := range matches {
		if !winIsPausableGateway(winLiveArgv(match.PID, match.Cmdline)) {
			return nil, false
		}
		pids = append(pids, match.PID)
	}
	return pids, true
}

// Refuses a plain Windows update that would tree kill a gateway this updater runs inside
func refuseGatewayAncestorTreeKill(ctx context.Context, w io.Writer, pids []int, gatewayMode bool) bool {
	if gatewayMode || len(pids) == 0 {
		return false
	}
	var ancestors []string
	for _, pid := range pids {
		if gateway.IsPidAncestorOfCurrentProcess(pid) {
			ancestors = append(ancestors, strconv.Itoa(pid))
		}
	}
	if len(ancestors) == 0 {
		return false
	}
	fmt.Fprintln(w, "✗ Refusing to stop the gateway process tree because this updater "+
		"is running inside it (gateway PID(s): "+strings.Join(ancestors, ", ")+").\n"+
		"  On Windows, taskkill /T would terminate the updater before the update can run.\n"+
		"  From a chat platform, use `/update` instead.\n  Otherwise, run `icebath update` from a separate terminal.")
	return true
}

// Whether a ledger entry carries an integer pid, with that pid
func winEntryPid(entry map[string]any) (int, bool) {
	if _, isBool := entry["pid"].(bool); isBool {
		return 0, false
	}
	pid, ok := pyIntValue(entry["pid"])
	return int(pid), ok
}

// Ledger entries of the holders that are manual serve or dashboard backends whose spawner is not provably alive
func ledgerManualServeHolders(matches []venvMatch) []map[string]any {
	holderPids := map[int]bool{}
	for _, match := range matches {
		holderPids[match.PID] = true
	}
	var entries []map[string]any
	for _, entry := range winLedgerEntries() {
		purpose, _ := entry["purpose"].(string)
		pid, ok := winEntryPid(entry)
		if !backendPurposes[purpose] || !ok || !holderPids[pid] {
			continue
		}
		if dead, known := winSpawnerIsDead(entry); known && !dead {
			continue
		}
		entries = append(entries, entry)
	}
	return entries
}

// Launch commands of stopped serves rebuilt from the ledger host, port and profile
func serveRelaunchCommands(entries []map[string]any) [][]string {
	command := "icebath"
	if scripts := venvScriptsDir(fleetProjectRoot()); scripts != "" {
		for _, name := range []string{"icebath.exe", "icebath"} {
			if takeoverIsFile(filepath.Join(scripts, name)) {
				command = filepath.Join(scripts, name)
				break
			}
		}
	}
	commands := [][]string{}
	for _, entry := range entries {
		if _, isBool := entry["port"].(bool); isBool {
			continue
		}
		port, ok := pyIntValue(entry["port"])
		if !ok || port <= 0 {
			continue
		}
		argv := []string{command}
		if profile := winText(entry["profile"]); profile != "" && profile != "default" {
			argv = append(argv, "--profile", profile)
		}
		argv = append(argv, pyrepr.Str(entry["purpose"]))
		if host := winText(entry["host"]); host != "" {
			argv = append(argv, "--host", host)
		}
		commands = append(commands, append(argv, "--port", strconv.FormatInt(port, 10)))
	}
	return commands
}

// Text of a truthy value, empty for a falsy one
func winText(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// Relaunches the manual serves the venv guard stopped, once per token
func relaunchStoppedServes(ctx context.Context, w io.Writer, token map[string]any) {
	if !pyrepr.Truthy(token["pending"]) {
		return
	}
	token["pending"] = false
	var entries []map[string]any
	switch list := token["entries"].(type) {
	case []map[string]any:
		entries = list
	case []any:
		for _, item := range list {
			if entry, ok := item.(map[string]any); ok {
				entries = append(entries, entry)
			}
		}
	}
	if len(entries) == 0 {
		return
	}
	commands := serveRelaunchCommands(entries)
	skipped := len(entries) - len(commands)
	var failed [][]string
	if len(commands) > 0 {
		fmt.Fprintln(w, "  ⟲ Relaunching stopped serve/dashboard backend(s)")
		failed = gateway.RespawnDashboardProcesses(ctx, w, commands)
	}
	if skipped > 0 || len(failed) > 0 {
		fmt.Fprintln(w, "  ⚠ Some stopped backends could not be relaunched automatically; restart them manually (icebath serve --host <ip> --port <port>).")
	}
	recordUpdateStep(ctx, "serve_relaunch", len(failed) == 0 && skipped == 0,
		fmt.Sprintf("relaunched=%d failed=%d skipped=%d", len(commands)-len(failed), len(failed), skipped))
}

// Whether a lower cased argv is a Desktop backend, the module entry shape running serve or dashboard
func isBackendArgv(argvLow string) bool { return looksLikeDesktopControlPlane(argvLow) }

// Exported form of the Desktop backend argv test for the dashboard cases outside this package
func IsBackendArgv(argvLow string) bool { return isBackendArgv(argvLow) }

// Current lower cased argv of a pid, the scanned command line when unreadable, false when it exited
func liveArgvLow(pid int, cmdline string) (string, bool) {
	argv, err := winCmdline(pid)
	if errors.Is(err, proc.ErrNoSuchProcess) {
		return "", false
	}
	if joined := strings.Join(argv, " "); err == nil && joined != "" {
		cmdline = joined
	}
	return strings.ToLower(cmdline), true
}

// Orphaned backend root of a tree reap with the start fingerprint read at classification
type backendRoot struct {
	PID       int
	StartTime int64
}

// Roots among the holders when every remaining holder is an orphaned backend or inside one's tree, false otherwise
func orphanedDesktopBackendPids(matches []venvMatch) ([]backendRoot, bool) {
	roots := []backendRoot{}
	var remaining []int
	for _, match := range matches {
		low, alive := liveArgvLow(match.PID, match.Cmdline)
		if !alive {
			continue
		}
		if !isBackendArgv(low) {
			remaining = append(remaining, match.PID)
			continue
		}
		born, err := winBirth(match.PID)
		if errors.Is(err, proc.ErrNoSuchProcess) {
			continue
		}
		if err != nil {
			return nil, false
		}
		live, err := parentIsLive(match.PID)
		if err != nil && !errors.Is(err, proc.ErrNoSuchProcess) {
			return nil, false
		}
		if err == nil && live {
			remaining = append(remaining, match.PID)
			continue
		}
		roots = append(roots, backendRoot{PID: match.PID, StartTime: int64(math.RoundToEven(born * 100))})
	}
	rootSet := map[int]bool{}
	for _, root := range roots {
		rootSet[root.PID] = true
	}
	for _, pid := range remaining {
		ancestors, err := winParents(pid)
		if errors.Is(err, proc.ErrNoSuchProcess) {
			continue
		}
		if err != nil {
			return nil, false
		}
		inside := false
		for _, ancestor := range ancestors {
			inside = inside || rootSet[ancestor]
		}
		if !inside {
			return nil, false
		}
	}
	return roots, true
}

// Holder pids the spawn ledger positively identifies as orphaned reapable backends
func ledgerReapableBackendPids(matches []venvMatch) []int {
	byPid := map[int]map[string]any{}
	for _, entry := range winLedgerEntries() {
		if pid, ok := winEntryPid(entry); ok {
			byPid[pid] = entry
		}
	}
	pids := []int{}
	for _, match := range matches {
		entry := byPid[match.PID]
		purpose, _ := entry["purpose"].(string)
		if len(entry) == 0 || !tools.ReapablePurposes[purpose] {
			continue
		}
		if dead, known := winSpawnerIsDead(entry); known && dead {
			pids = append(pids, match.PID)
		}
	}
	return pids
}

// Backend pids safe to tree reap during a GUI updater hand off, false when another holder remains
func handoffReapableBackendPids(matches []venvMatch) ([]int, bool) {
	var roots []int
	for _, match := range matches {
		low, alive := liveArgvLow(match.PID, match.Cmdline)
		if !alive {
			continue
		}
		if !isBackendArgv(low) {
			return nil, false
		}
		roots = append(roots, match.PID)
	}
	return roots, len(roots) > 0
}

// Force stops each pid with its child tree when it is still the icebath process it was, never failing
func stopProcessTrees(ctx context.Context, roots []backendRoot) {
	for _, root := range roots {
		if !gateway.PidIsHermes(root.PID, root.StartTime) {
			updateLog.DebugContext(ctx, fmt.Sprintf("Skipping taskkill of non-Hermes or changed PID %d", root.PID))
			continue
		}
		if _, err := pysubprocess.RunWith(ctx, []string{"taskkill", "/PID", strconv.Itoa(root.PID), "/T", "/F"},
			pysubprocess.Options{Attr: tools.WindowsHideAttr()}); err != nil {
			updateLog.DebugContext(ctx, fmt.Sprintf("Could not stop process tree %d: %s", root.PID, launchErrorText(err)))
		}
	}
}

// Force stops plain pids with their child trees under the start time each has now
func stopProcessTreesOf(ctx context.Context, pids []int) {
	var roots []backendRoot
	for _, pid := range pids {
		start, ok := pyIntValue(gateway.GetProcessStartTime(pid))
		if !ok {
			updateLog.DebugContext(ctx, fmt.Sprintf("Skipping taskkill of PID %d: process identity unavailable", pid))
			continue
		}
		roots = append(roots, backendRoot{PID: pid, StartTime: start})
	}
	stopProcessTrees(ctx, roots)
}

// True for a module entry command line of this install running serve or dashboard
func looksLikeDesktopControlPlane(cmdline string) bool {
	if !strings.Contains(strings.ToLower(cmdline), "icebath_cli.main") {
		return false
	}
	subcommand, ok := hermesHolderSubcommand(cmdline)
	return ok && backendPurposes[subcommand]
}

// True when Desktop currently supervises this install's control plane
func desktopOwnsGatewayLifecycle(ctx context.Context) bool {
	for _, entry := range winLedgerEntries() {
		purpose, _ := entry["purpose"].(string)
		if dead, known := winSpawnerIsDead(entry); backendPurposes[purpose] && known && !dead {
			return true
		}
	}
	for _, match := range winDetectHolders(ctx, nil) {
		if !looksLikeDesktopControlPlane(match.Cmdline) {
			continue
		}
		if live, err := parentIsLive(match.PID); err == nil && live {
			return true
		}
	}
	return false
}
