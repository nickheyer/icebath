package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	posixStopTimeoutS  = 120.0
	posixReadyTimeoutS = 30.0
)

// Runtimes the early resume already restarted on the new code
type pauseRestartedSets struct {
	Units  map[string]bool
	Labels map[string]bool
	PIDs   map[int]bool
}

var (
	posixPauseRun       map[string]any
	posixPauseOut       io.Writer = os.Stdout
	posixPauseRestarted           = &pauseRestartedSets{Units: map[string]bool{}, Labels: map[string]bool{}, PIDs: map[int]bool{}}
	posixPauseSleep               = time.Sleep

	pauseDiscoverSystemd = discoverSystemd
	pauseDiscoverLaunchd = discoverLaunchd
	pauseLeftRunning     = leftRunning
	pauseDiscoverBare    = discoverBare
	pauseStopGateways    = stopGateways
)

// The pause could not be completed and everything it stopped was started again
type PauseRefused struct{ Msg string }

func (e *PauseRefused) Error() string { return e.Msg }

// Fresh POSIX pause token owing nothing
func emptyToken() map[string]any {
	return map[string]any{"platform": "posix", "resume_needed": false, "profiles": map[string]any{}, "unmapped_pids": []any{},
		"unmapped": []any{}, "posix_units": []any{}}
}

// Python truth of a token field
func tokenTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case []string:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	case float64:
		return t != 0
	}
	if n, ok := pyIntValue(v); ok {
		return n != 0
	}
	return pyTruthy(v)
}

// Entries of a token list as dicts, a typed list read the same as a decoded one
func tokenEntries(v any) []map[string]any {
	var out []map[string]any
	switch list := v.(type) {
	case []map[string]any:
		return list
	case []any:
		for _, item := range list {
			if entry, ok := item.(map[string]any); ok {
				out = append(out, entry)
			}
		}
	}
	return out
}

func entryPid(entry map[string]any) int {
	n, _ := pyIntValue(entry["pid"])
	return int(n)
}

func entryStr(entry map[string]any, key string) string {
	s, _ := entry[key].(string)
	return s
}

func entriesAsAny(entries []map[string]any) []any {
	out := make([]any, len(entries))
	for i, entry := range entries {
		out[i] = entry
	}
	return out
}

// Arms this run's pause without stopping anything and adopts any set a killed update left paused
func armPause(ctx context.Context, w io.Writer, noGatewayRestart bool) (map[string]any, error) {
	if runtime.GOOS == "windows" || noGatewayRestart {
		return nil, nil
	}
	if w != nil {
		posixPauseOut = w
	}
	token := emptyToken()
	adopted, claims, err := updaterecord.AdoptPauseOrphans(ctx)
	if err == nil && adopted != nil {
		owed := emptyToken()
		owed["resume_needed"] = true
		var recorded map[string]any
		if recorded, err = updaterecord.RecordPause(ctx, owed, adopted, claims); err == nil {
			token = recorded
			token["platform"] = "posix"
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		fmt.Fprintf(posixPauseOut, "  ⚠ Could not adopt gateways an earlier update paused: %s\n", launchErrorText(err))
	}
	posixPauseRun = token
	return token, nil
}

// True while this run holds gateways stopped
func gatewaysPaused() bool {
	return posixPauseRun != nil && tokenTruthy(posixPauseRun["posix_stopped"]) && tokenTruthy(posixPauseRun["resume_needed"])
}

// Whether a failure is one the OSError and SubprocessError suppressions swallow
func pauseSuppressed(err error) bool {
	return pypath.IsOSError(err) || isSubprocessError(err)
}

func pauseText(raw []byte) string { return pystr.Strip(pystr.DecodeUTF8Replace(raw)) }

// Control group path systemd reports for a unit, empty when unknown
func unitCgroup(ctx context.Context, scopeCmd []string, unit string) (string, error) {
	result, err := systemctl(ctx, append(append([]string{}, scopeCmd...), "show", unit, "--property=ControlGroup", "--value"), 10)
	if err != nil {
		var expired *pysubprocess.TimeoutExpired
		if pypath.IsOSError(err) || errors.As(err, &expired) {
			return "", nil
		}
		return "", err
	}
	if result.Returncode == 0 {
		return pauseText(result.Stdout), nil
	}
	return "", nil
}

// Systemd cgroup path of a pid from the unified line else the v1 name=systemd hierarchy, empty when unreadable
func pidCgroup(pid string) string {
	raw, err := os.ReadFile("/proc/" + pid + "/cgroup")
	if err != nil {
		return ""
	}
	unified, legacy := "", ""
	haveUnified, haveLegacy := false, false
	for _, line := range pystr.Splitlines(string(raw), false) {
		if !haveUnified && strings.HasPrefix(line, "0::") {
			unified, haveUnified = pystr.Strip(line[3:]), true
		}
		if !haveLegacy && strings.Contains(line, ":name=systemd:") {
			legacy, haveLegacy = pystr.Strip(strings.SplitN(line, ":", 3)[2]), true
		}
	}
	if unified != "" {
		return unified
	}
	return legacy
}

// True when a cgroup path is the unit's own or lies below it
func inside(cgroup, unitCgroup string) bool {
	if cgroup == "" || unitCgroup == "" {
		return false
	}
	return cgroup == unitCgroup || strings.HasPrefix(cgroup, strings.TrimRight(unitCgroup, "/")+"/")
}

// Active gateway units of this install, one per live MainPID
func discoverSystemd(ctx context.Context) ([]map[string]any, error) {
	if !gateway.SupportsSystemdServices(ctx) {
		return nil, nil
	}
	gateway.EnsureUserSystemdEnv()
	listings, err := systemdGatewayUnitListings(ctx, nil)
	if err != nil {
		return nil, err
	}
	var units []map[string]any
	seen := map[int]bool{}
	for _, listing := range listings {
		var names []string
		err := forEachSystemdGatewayUnit(pystr.DecodeUTF8Replace(listing.Listing.Stdout),
			func(name string) error { names = append(names, name); return nil }, func(string, error) {})
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			if !strings.HasPrefix(name, gateway.ServiceBase) {
				continue
			}
			pid, err := unitMainPid(ctx, listing.ScopeCmd, name)
			if err != nil {
				return nil, err
			}
			if pid <= 0 || seen[pid] {
				continue
			}
			owned, err := systemdUnitOwnedByUpdate(ctx, posixPauseOut, listing.ScopeCmd, name)
			if err != nil {
				return nil, err
			}
			if !owned {
				continue
			}
			seen[pid] = true
			cgroup, err := unitCgroup(ctx, listing.ScopeCmd, name)
			if err != nil {
				return nil, err
			}
			unit := map[string]any{"kind": "systemd", "scope": listing.Scope, "unit": name, "pid": pid, "cgroup": nil, "home": homeOf(ctx, pid)}
			if cgroup != "" {
				unit["cgroup"] = cgroup
			}
			units = append(units, unit)
		}
	}
	return units, nil
}

// Loaded launchd gateway jobs of this install with a live process
func discoverLaunchd(ctx context.Context) ([]map[string]any, error) {
	if runtime.GOOS != "darwin" {
		return nil, nil
	}
	derived, err := gateway.LaunchdGatewayLabelsForInstall(ctx)
	if err != nil {
		return nil, err
	}
	exclude := map[string]bool{}
	for _, label := range derived {
		exclude[label] = true
	}
	legacy, err := legacyLaunchdLabelsForInstall(ctx, exclude)
	if err != nil {
		return nil, err
	}
	var jobs []map[string]any
	for _, label := range append(append([]string{}, derived...), legacy...) {
		if _, foreign := launchdLabelForeignHome(ctx, label); foreign {
			continue
		}
		domain, pid, ok, err := gateway.LocateLaunchdGatewayService(ctx, label)
		if err != nil {
			return nil, err
		}
		if domain == "" || !ok || pid <= 0 {
			continue
		}
		plistPath, err := gateway.GetLaunchdPlistPath(ctx)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, map[string]any{"kind": "launchd", "label": label, "domain": domain,
			"plist": filepath.Join(filepath.Dir(plistPath), label+".plist"), "pid": pid, "home": homeOf(ctx, pid)})
	}
	return jobs, nil
}

// Home a gateway pid serves, nil when it cannot be told
func homeOf(ctx context.Context, pid int) any {
	if served := gateway.GatewayPidHome(ctx, pid); served != "" {
		return served
	}
	return nil
}

// Why a pid answers to a supervisor other than an icebath unit or job, empty when none is proven
func supervisorMarkers(ctx context.Context, pid int) (string, error) {
	for _, arg := range gateway.CaptureGatewayArgv(pid) {
		if arg == "--external-supervisor" {
			return "--external-supervisor", nil
		}
	}
	env, err := proc.Environ(pid)
	if err != nil {
		return "", nil
	}
	if env["ICEBATH_S6_SUPERVISED_CHILD"] != "" {
		return "s6", nil
	}
	unit, err := respawningService(ctx, pid)
	if err != nil {
		return "", err
	}
	if unit != "" {
		return "systemd unit " + unit, nil
	}
	xpc, ok := env["XPC_SERVICE_NAME"]
	if !ok {
		xpc = "0"
	}
	if xpc != "" && xpc != "0" && !strings.HasPrefix(xpc, "application.") {
		return "launchd job " + xpc, nil
	}
	return "", nil
}

// Systemd service that would respawn a pid when it exits, empty when none would
func respawningService(ctx context.Context, pid int) (string, error) {
	cgroup := pidCgroup(strconv.Itoa(pid))
	leaf := cgroup[strings.LastIndex(cgroup, "/")+1:]
	if !strings.HasSuffix(leaf, ".service") {
		return "", nil
	}
	scopeCmd := []string{"systemctl"}
	if strings.Contains(cgroup, "/user@") {
		scopeCmd = []string{"systemctl", "--user"}
	}
	main, err := unitMainPid(ctx, scopeCmd, leaf)
	if err == nil && main > 0 && (main == pid || main == ppid(pid)) {
		var restart *pysubprocess.Completed
		if restart, err = systemctl(ctx, append(scopeCmd, "show", leaf, "--property=Restart", "--value"), 10); err == nil {
			if policy := pauseText(restart.Stdout); policy != "" && policy != "no" {
				return leaf, nil
			}
			return "", nil
		}
	}
	if err != nil && !pauseSuppressed(err) {
		return "", err
	}
	return "", nil
}

// Bare gateway run processes of this install outside any unit and a notice per one left running
func discoverBare(ctx context.Context, servicePids map[int]bool) ([]map[string]any, []string, error) {
	exclude := map[int]bool{}
	for pid := range servicePids {
		exclude[pid] = true
	}
	service, err := gateway.GetServicePIDs(ctx, true)
	if err != nil {
		return nil, nil, err
	}
	for _, pid := range service {
		exclude[pid] = true
	}
	found, err := gateway.FindGatewayPIDsFor(ctx, exclude, true)
	if err != nil {
		return nil, nil, err
	}
	pids, err := scopedManualGatewayPids(ctx, posixPauseOut, found, nil, false)
	if err != nil {
		return nil, nil, err
	}
	var bare []map[string]any
	var notices []string
	for _, pid := range pids {
		marker, err := supervisorMarkers(ctx, pid)
		if err != nil {
			return nil, nil, err
		}
		argv, served := gateway.CaptureGatewayArgv(pid), gateway.GatewayPidHome(ctx, pid)
		if marker != "" || len(argv) == 0 || served == "" {
			why := "its Icebath home could not be proved"
			if marker != "" {
				why = "supervised by " + marker
			} else if len(argv) == 0 {
				why = "its command line could not be read"
			}
			notices = append(notices, fmt.Sprintf("  ↷ gateway PID %d keeps running until after the update (%s)", pid, why))
			continue
		}
		args := make([]any, len(argv))
		for i, arg := range argv {
			args[i] = arg
		}
		bare = append(bare, map[string]any{"pid": pid, "argv": args, "home": served, "ct": gateway.GetProcessStartTime(pid)})
	}
	return bare, notices, nil
}

// Parent pid from the proc stat line, 0 when unreadable
func ppid(pid int) int {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	stat := string(raw)
	cut := strings.LastIndex(stat, ")")
	if cut < 0 {
		return 0
	}
	fields := strings.Fields(stat[cut+1:])
	if len(fields) < 2 {
		return 0
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return parent
}

// Moves this process and its ancestors in the unit into a transient scope, true only when proven outside
func escapeCgroup(ctx context.Context, unit map[string]any) bool {
	own, unitCg := pidCgroup("self"), entryStr(unit, "cgroup")
	if own == "" || unitCg == "" {
		return false
	}
	if !inside(own, unitCg) {
		return true
	}
	var pids []string
	self := os.Getpid()
	for pid := self; pid > 1 && pid != entryPid(unit) && inside(pidCgroup(strconv.Itoa(pid)), unitCg); pid = ppid(pid) {
		if raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
			cmdline := pystr.DecodeUTF8Replace([]byte(strings.ReplaceAll(string(raw), "\x00", " ")))
			if pid != self && gateway.LooksLikeGatewayCommandLine(cmdline) {
				break
			}
		}
		pids = append(pids, strconv.Itoa(pid))
	}
	bus := "--system"
	if entryStr(unit, "scope") == "user" {
		bus = "--user"
	}
	cmd := []string{"busctl", bus, "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
		"org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))",
		fmt.Sprintf("icebath-update-%d.scope", self), "fail", "2", "PIDs", "au", strconv.Itoa(len(pids))}
	cmd = append(cmd, pids...)
	cmd = append(cmd, "Description", "s", fmt.Sprintf("icebath update (outside %s while it is paused)", entryStr(unit, "unit")), "0")
	_, _ = pysubprocess.RunWith(ctx, cmd, pysubprocess.Options{Timeout: 15 * time.Second})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if now := pidCgroup("self"); now != "" && !inside(now, unitCg) {
			return true
		}
		posixPauseSleep(100 * time.Millisecond)
	}
	return false
}

// Drops the runtimes whose stop would kill this updater and names each one
func leftRunning(ctx context.Context, units, jobs []map[string]any) ([]map[string]any, []map[string]any, []string, error) {
	var keepUnits, keepJobs []map[string]any
	var notices []string
	for _, unit := range units {
		switch {
		case manageCmd(ctx, entryStr(unit, "scope")) == nil:
			notices = append(notices, fmt.Sprintf("  ↷ %s keeps running until after the update (stopping a system "+
				"unit needs root or passwordless sudo)", entryStr(unit, "unit")))
		case escapeCgroup(ctx, unit):
			keepUnits = append(keepUnits, unit)
		default:
			notices = append(notices, fmt.Sprintf("  ↷ %s keeps running until after the update (this update could "+
				"not prove it runs outside that unit's cgroup)", entryStr(unit, "unit")))
		}
	}
	for _, job := range jobs {
		if gateway.IsPidAncestorOfCurrentProcess(entryPid(job)) || os.Getenv("XPC_SERVICE_NAME") == entryStr(job, "label") {
			notices = append(notices, fmt.Sprintf("  ↷ %s keeps running until after the update (this update runs "+
				"inside its launchd job)", entryStr(job, "label")))
		} else {
			keepJobs = append(keepJobs, job)
		}
	}
	return keepUnits, keepJobs, notices, nil
}

// Systemctl prefix that may stop and start units of a scope, nil when this user lacks the privilege
func manageCmd(ctx context.Context, scope string) []string {
	cmd := []string{"systemctl"}
	if scope == "user" {
		cmd = []string{"systemctl", "--user"}
	}
	cmd = append(cmd, "--no-ask-password")
	if scope == "system" && needsSudo(scope) {
		if sudoNoninteractiveOk(ctx, append(append([]string{}, cmd...), "show", "-p", "Id", gateway.ServiceBase)) {
			return append([]string{"sudo", "-n"}, cmd...)
		}
		return nil
	}
	return cmd
}

func scopeCmd(unit map[string]any) []string {
	if entryStr(unit, "scope") == "user" {
		return []string{"systemctl", "--user"}
	}
	return []string{"systemctl"}
}

// Stops a unit through systemd, a stop job still running holding the pause unless the gateway is provably up
func stopUnit(ctx context.Context, unit map[string]any) error {
	name := entryStr(unit, "unit")
	cmd := manageCmd(ctx, entryStr(unit, "scope"))
	if cmd == nil {
		return &PauseRefused{"no privilege to stop system unit " + name}
	}
	result, err := runUnitJob(ctx, cmd, scopeCmd(unit), "stop", name, posixStopTimeoutS)
	if err != nil {
		return err
	}
	if result.Returncode == unitJobStillRunning && !alive(entryPid(unit), unit["ct"]) {
		main, err := unitMainPid(ctx, scopeCmd(unit), name)
		if err != nil {
			return err
		}
		if main <= 0 {
			return nil
		}
	}
	if result.Returncode != 0 {
		return &PauseRefused{fmt.Sprintf("systemctl stop %s failed: %s", name, pauseText(result.Stderr))}
	}
	return nil
}

// Starts a unit through systemd after the definition repair, the new MainPID once it is active
func startUnit(ctx context.Context, w io.Writer, unit map[string]any) (int, error) {
	name, scope := entryStr(unit, "unit"), entryStr(unit, "scope")
	cmd := manageCmd(ctx, scope)
	if cmd == nil {
		return 0, fmt.Errorf("no privilege to start system unit %s", name)
	}
	if err := repairUnitWithoutFatalExitPark(ctx, w, name, scope); err != nil {
		return 0, err
	}
	if _, err := systemctl(ctx, append(append([]string{}, cmd...), "reset-failed", name), 10); err != nil {
		return 0, err
	}
	scoped := scopeCmd(unit)
	result, err := runUnitJob(ctx, cmd, scoped, "start", name, posixStopTimeoutS)
	if err != nil {
		return 0, err
	}
	active := false
	if result.Returncode == 0 {
		if active, err = waitForServiceActive(ctx, scoped, name, 15.0); err != nil {
			return 0, err
		}
	}
	if !active {
		why := pauseText(result.Stderr)
		if why == "" {
			why = "not active"
		}
		return 0, fmt.Errorf("%s did not start: %s", name, why)
	}
	return unitMainPid(ctx, scoped, name)
}

// Boots a job out of launchd, a drained one only to keep KeepAlive from serving the update
func stopJob(ctx context.Context, job map[string]any, drained bool) error {
	label, pid := entryStr(job, "label"), entryPid(job)
	if served := entryStr(job, "home"); !drained && served != "" {
		if _, err := writeUpdatePlannedStopMarker(ctx, served, pid); err != nil {
			return err
		}
	}
	result, err := pysubprocess.RunWith(ctx, []string{"launchctl", "bootout", entryStr(job, "domain") + "/" + label},
		pysubprocess.Options{Timeout: 90 * time.Second})
	if err != nil {
		return err
	}
	waitGone([]int{pid}, 30.0, nil)
	if result.Returncode != 0 {
		_, running, _, err := gateway.LocateLaunchdGatewayService(ctx, label)
		if err != nil {
			return err
		}
		if running > 0 {
			return &PauseRefused{fmt.Sprintf("launchctl bootout %s failed: %s", label, pauseText(result.Stderr))}
		}
	}
	return nil
}

// Loads and starts a job, the invoking profile's plist regenerated first
func startJob(ctx context.Context, job map[string]any) error {
	label, domain := entryStr(job, "label"), entryStr(job, "domain")
	own, err := gateway.GetLaunchdLabel(ctx)
	if err != nil {
		return err
	}
	refreshed := false
	if label == own {
		if refreshed, err = gateway.RefreshLaunchdPlistIfNeeded(ctx); err != nil {
			return err
		}
	}
	if !refreshed {
		if err := gateway.LaunchctlBootstrap(ctx, domain, entryStr(job, "plist"), label, 30*time.Second); err != nil {
			return err
		}
	}
	if _, err := pysubprocess.RunWith(ctx, []string{"launchctl", "kickstart", domain + "/" + label},
		pysubprocess.Options{Timeout: 30 * time.Second}); err != nil {
		return err
	}
	back, err := gateway.WaitForLaunchdServicePID(ctx, label, entryPid(job), true, 15.0, domain)
	if err != nil {
		return err
	}
	if !back {
		return fmt.Errorf("%s did not come back under launchd", label)
	}
	return nil
}

// Whether the recorded incarnation of a pid still runs, an unreadable birth stamp counting as alive
func alive(pid int, ct any) bool {
	if !proc.Exists(pid) {
		return false
	}
	if ct == nil {
		return true
	}
	started := gateway.GetProcessStartTime(pid)
	return started == nil || pyrepr.Equal(started, ct)
}

// Polls every fifth of a second until the pids are gone or the timeout passes, the ones still alive
func waitGone(pids []int, timeout float64, born map[int]any) map[int]bool {
	deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
	live := map[int]bool{}
	for _, pid := range pids {
		live[pid] = true
	}
	for len(live) > 0 && time.Now().Before(deadline) {
		for pid := range live {
			if !alive(pid, born[pid]) {
				delete(live, pid)
			}
		}
		if len(live) > 0 {
			posixPauseSleep(200 * time.Millisecond)
		}
	}
	return live
}

// Asks the gateway over its control socket to finish its turns and exit, true on its acknowledgement
func askToDrain(entry map[string]any) bool {
	served := entryStr(entry, "home")
	if served == "" {
		return false
	}
	ack := gateway.PauseGatewayForUpdate(served, 2*time.Second)
	if ack == nil || ack.Len() == 0 {
		return false
	}
	return tokenTruthy(ack.GetOr("pausing", nil)) || tokenTruthy(ack.GetOr("already_stopping", nil))
}

// Sends the terminate or kill signal, a pid already gone or not ours counting as sent
func pauseSignal(pid int, force bool) error {
	var err error
	if force {
		err = proc.KillPID(pid)
	} else {
		err = proc.Terminate(pid)
	}
	if err == nil || errors.Is(err, syscall.ESRCH) || errors.Is(err, fs.ErrPermission) {
		return nil
	}
	return err
}

// Stops a gateway that did not take the drain through its supervisor, else marker and terminate
func stopAtOnce(ctx context.Context, entry map[string]any) error {
	switch entryStr(entry, "kind") {
	case "systemd":
		return stopUnit(ctx, entry)
	case "launchd":
		return stopJob(ctx, entry, false)
	}
	if _, err := writeUpdatePlannedStopMarker(ctx, entryStr(entry, "home"), entryPid(entry)); err != nil {
		return err
	}
	return pauseSignal(entryPid(entry), false)
}

// Wait the post update restart grants a drained gateway
func drainBudget(ctx context.Context) float64 { return gatewayDrainBudget(ctx) }

// Drains every gateway then waits for all within one budget, escalating survivors past it
func stopGateways(ctx context.Context, token map[string]any, entries []map[string]any) error {
	budget := drainBudget(ctx)
	var waiting []map[string]any
	for _, entry := range entries {
		if err := updaterecord.MarkStopSent(ctx, token, entryPid(entry)); err != nil {
			return err
		}
		if askToDrain(entry) {
			waiting = append(waiting, entry)
			continue
		}
		if err := stopAtOnce(ctx, entry); err != nil {
			return err
		}
		if entryStr(entry, "kind") == "" {
			waiting = append(waiting, entry)
		}
	}
	if len(waiting) == 0 {
		return nil
	}
	out := posixPauseOut
	fmt.Fprintf(out, "  ⏳ Waiting up to %.0fs for in-flight turns on %s to finish before the update...\n", budget, names(waiting))
	ticks := make([]func(), len(waiting))
	for i, entry := range waiting {
		ticks[i] = drainProgressReporter(ctx, entryStr(entry, "home"), budget, drainReportIntervalS, func(s string) { fmt.Fprintln(out, s) })
	}
	deadline := time.Now().Add(time.Duration(budget * float64(time.Second)))
	for len(waiting) > 0 && time.Now().Before(deadline) {
		var still, gone []map[string]any
		for _, entry := range waiting {
			if alive(entryPid(entry), entry["ct"]) {
				still = append(still, entry)
			} else {
				gone = append(gone, entry)
			}
		}
		waiting = still
		for _, entry := range gone {
			if err := disarm(ctx, entry); err != nil {
				return err
			}
		}
		if len(waiting) > 0 {
			for _, tick := range ticks {
				tick()
			}
		}
		posixPauseSleep(200 * time.Millisecond)
	}
	var bare []int
	born := map[int]any{}
	for _, entry := range waiting {
		if entryStr(entry, "kind") != "" {
			if err := stopAtOnce(ctx, entry); err != nil {
				return err
			}
			continue
		}
		if err := pauseSignal(entryPid(entry), true); err != nil {
			return err
		}
	}
	for _, entry := range waiting {
		if entryStr(entry, "kind") == "" {
			bare = append(bare, entryPid(entry))
			born[entryPid(entry)] = entry["ct"]
		}
	}
	if left := waitGone(bare, 10.0, born); len(left) > 0 {
		pids := make([]int, 0, len(left))
		for pid := range left {
			pids = append(pids, pid)
		}
		sort.Ints(pids)
		text := make([]string, len(pids))
		for i, pid := range pids {
			text[i] = strconv.Itoa(pid)
		}
		return &PauseRefused{"gateway PID(s) " + strings.Join(text, ", ") + " did not stop"}
	}
	return nil
}

// Keeps the supervisor of a drained gateway from bringing it back during the update
func disarm(ctx context.Context, entry map[string]any) error {
	switch entryStr(entry, "kind") {
	case "systemd":
		return stopUnit(ctx, entry)
	case "launchd":
		return stopJob(ctx, entry, true)
	}
	return nil
}

func names(entries []map[string]any) string {
	parts := make([]string, len(entries))
	for i, entry := range entries {
		switch {
		case entryStr(entry, "unit") != "":
			parts[i] = entryStr(entry, "unit")
		case entryStr(entry, "label") != "":
			parts[i] = entryStr(entry, "label")
		default:
			parts[i] = "gateway PID " + pyrepr.Str(entry["pid"])
		}
	}
	return strings.Join(parts, ", ")
}

// Gateway serving a home by its pid file, lock and runtime record, never one of the excluded pids
func gatewayOnHome(served string, exclude map[int]bool) (int, bool) {
	pid, ok, err := gateway.LiveGatewayPidForHome(served)
	if err != nil || !ok || pid == 0 || exclude[pid] {
		return 0, false
	}
	return pid, true
}

// Environment a replayed gateway starts with, the launcher's for its own named profile else the home's own secrets
func replayEnv(ctx context.Context, served string) (map[string]string, error) {
	target, err := pypath.Resolve(served)
	if err != nil {
		return nil, err
	}
	root, err := pypath.Resolve(home.Root())
	if err != nil {
		return nil, err
	}
	routing, err := pypath.Resolve(home.RoutingProcess())
	if err != nil {
		return nil, err
	}
	if target != root && target == routing {
		return tools.BuildSubprocessEnv(ctx, nil, tools.SubprocessEnvOptions{Extra: map[string]string{"ICEBATH_HOME": served}})
	}
	return tools.ServedProfileChildEnv(ctx, nil, nil, served, true)
}

// Replays a bare gateway from its recorded argv under its recorded home, the pid serving that home
func relaunchBare(ctx context.Context, entry map[string]any) (int, error) {
	served, old := entryStr(entry, "home"), map[int]bool{entryPid(entry): true}
	if existing, ok := gatewayOnHome(served, old); ok {
		return existing, nil
	}
	logs := filepath.Join(served, "logs")
	if err := os.MkdirAll(logs, 0o777); err != nil {
		return 0, err
	}
	env, err := replayEnv(ctx, served)
	if err != nil {
		return 0, err
	}
	for _, leaked := range []string{"INVOCATION_ID", "JOURNAL_STREAM", "ICEBATH_UPDATE_PAUSED", "_ICEBATH_GATEWAY"} {
		delete(env, leaked)
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environ := make([]string, len(keys))
	for i, key := range keys {
		environ[i] = key + "=" + env[key]
	}
	var argv []string
	switch list := entry["argv"].(type) {
	case []any:
		for _, arg := range list {
			argv = append(argv, pyrepr.Str(arg))
		}
	case []string:
		argv = list
	}
	logPath := filepath.Join(logs, "gateway-update-resume.log")
	log, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env, cmd.Stdout, cmd.Stderr, cmd.SysProcAttr = environ, log, log, posixPauseDetachedAttr()
	err = cmd.Start()
	log.Close()
	if err != nil {
		return 0, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.Now().Add(time.Duration(posixReadyTimeoutS * float64(time.Second)))
	for time.Now().Before(deadline) {
		if pid, ok := gatewayOnHome(served, old); ok {
			return pid, nil
		}
		select {
		case waitErr := <-exited:
			code := 0
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				code = pysubprocess.Returncode(exitErr)
			}
			return 0, fmt.Errorf("replayed gateway for %s exited %d (see %s)", served, code, logPath)
		default:
		}
		posixPauseSleep(500 * time.Millisecond)
	}
	return 0, fmt.Errorf("replayed gateway for %s did not come up within %ds", served, int(posixReadyTimeoutS))
}

func describe(token map[string]any) string {
	var parts []string
	for _, unit := range tokenEntries(token["posix_units"]) {
		name := entryStr(unit, "unit")
		if name == "" {
			name = entryStr(unit, "label")
		}
		parts = append(parts, name)
	}
	for _, entry := range tokenEntries(token["unmapped"]) {
		if tokenTruthy(entry["argv"]) {
			parts = append(parts, "gateway PID "+pyrepr.Str(entry["pid"]))
		}
	}
	return strings.Join(parts, ", ")
}

// Stops this install's gateways before the first checkout move once per run, never refusing the update
func pauseAtCommitPoint(ctx context.Context) (string, error) {
	token := posixPauseRun
	if token == nil {
		return "", nil
	}
	if _, judged := token["posix_pause_verdict"]; !judged {
		if err := pause(ctx, token); err != nil {
			return "", err
		}
		token["posix_pause_verdict"] = nil
	}
	verdict, _ := token["posix_pause_verdict"].(string)
	return verdict, nil
}

// Records then stops every gateway of this install, leaving them running when either cannot complete
func pause(ctx context.Context, token map[string]any) error {
	out := posixPauseOut
	var jobs, bare []map[string]any
	var notices, more []string
	units, err := pauseDiscoverSystemd(ctx)
	if err == nil {
		jobs, err = pauseDiscoverLaunchd(ctx)
	}
	if err == nil {
		units, jobs, notices, err = pauseLeftRunning(ctx, units, jobs)
	}
	if err == nil {
		service := map[int]bool{}
		for _, entry := range append(append([]map[string]any{}, units...), jobs...) {
			service[entryPid(entry)] = true
		}
		bare, more, err = pauseDiscoverBare(ctx, service)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fmt.Fprintf(out, "  ⚠ Could not list the running gateways (%s); they keep running and are restarted after the update\n", launchErrorText(err))
		return nil
	}
	for _, line := range append(notices, more...) {
		fmt.Fprintln(out, line)
	}
	supervised := append(append([]map[string]any{}, units...), jobs...)
	if len(supervised) == 0 && len(bare) == 0 {
		return nil
	}
	all := append(append([]map[string]any{}, supervised...), bare...)
	before, _ := pycopy.DeepCopy(token).(map[string]any)
	pids := make([]int, len(all))
	barePids := make([]any, len(bare))
	identities := map[string]any{}
	if recorded, ok := token["identities"].(map[string]any); ok {
		for pid, ct := range recorded {
			identities[pid] = ct
		}
	}
	markers := map[int]string{}
	for i, entry := range all {
		pid := entryPid(entry)
		entry["ct"] = gateway.GetProcessStartTime(pid)
		pids[i] = pid
		identities[strconv.Itoa(pid)] = updaterecord.PauseIdentity(pid)["ct"]
		if served := entryStr(entry, "home"); served != "" {
			markers[pid] = plannedStopMarkerPath(served)
		}
	}
	for i, entry := range bare {
		barePids[i] = entryPid(entry)
	}
	token["posix_units"] = append(entriesAsAny(tokenEntries(token["posix_units"])), entriesAsAny(supervised)...)
	token["unmapped"] = append(entriesAsAny(tokenEntries(token["unmapped"])), entriesAsAny(bare)...)
	recordedPids, _ := token["unmapped_pids"].([]any)
	token["unmapped_pids"] = append(append([]any{}, recordedPids...), barePids...)
	token["identities"] = identities
	token["resume_needed"] = true
	if _, err = updaterecord.RecordPause(ctx, token, nil, nil); err == nil {
		err = updaterecord.MarkStopRequested(ctx, token, pids, markers)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !pyrepr.Equal(token["pause_id"], before["pause_id"]) {
			_ = updaterecord.DischargePause(ctx, token, "")
		}
		for key := range token {
			delete(token, key)
		}
		for key, value := range before {
			token[key] = value
		}
		fmt.Fprintf(out, "  ⚠ Could not record the gateways to pause (%s); they keep running and are restarted after the update\n", launchErrorText(err))
		return nil
	}
	token["posix_stopped"] = true
	if err := pauseStopGateways(ctx, token, all); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fmt.Fprintf(out, "  ⚠ Could not pause every gateway (%s); restarting them, the update continues without the pause\n", launchErrorText(err))
		if again := resumePausedSet(ctx, out, token); again != nil {
			fmt.Fprintf(out, "  ⚠ %s; they restart once the dependencies are synced\n", launchErrorText(again))
		}
		posixPauseRestarted.Units, posixPauseRestarted.Labels, posixPauseRestarted.PIDs = map[string]bool{}, map[string]bool{}, map[int]bool{}
		return updaterecord.SyncPause(ctx, token)
	}
	if err := updaterecord.SyncPause(ctx, token); err != nil {
		return err
	}
	fmt.Fprintf(out, "  ⏸ Paused for the update: %s (restarted once the dependencies are synced)\n", describe(token))
	return nil
}

// Starts every paused runtime again, each leaving the token only on its own verified start
func resumePausedSet(ctx context.Context, w io.Writer, token map[string]any) error {
	var failures, restarted []string
	keptUnits := []any{}
	for _, entry := range tokenEntries(token["posix_units"]) {
		name := ""
		var err error
		if entryStr(entry, "kind") == "launchd" {
			if err = startJob(ctx, entry); err == nil {
				name = entryStr(entry, "label")
				posixPauseRestarted.Labels[name] = true
			}
		} else {
			var pid int
			if pid, err = startUnit(ctx, w, entry); err == nil {
				name = entryStr(entry, "unit")
				posixPauseRestarted.PIDs[pid] = true
				posixPauseRestarted.Units[entryStr(entry, "scope")+"/"+name] = true
			}
		}
		if err != nil {
			failures = append(failures, launchErrorText(err))
			keptUnits = append(keptUnits, entry)
			continue
		}
		restarted = append(restarted, name)
		services, _ := token["restarted_services"].([]any)
		token["restarted_services"] = append(services, name)
	}
	token["posix_units"] = keptUnits
	keptBare, keptPids := []any{}, []any{}
	for _, entry := range tokenEntries(token["unmapped"]) {
		if !tokenTruthy(entry["argv"]) {
			continue
		}
		pid, err := relaunchBare(ctx, entry)
		if err != nil {
			failures = append(failures, launchErrorText(err))
			keptBare, keptPids = append(keptBare, entry), append(keptPids, entry["pid"])
			continue
		}
		posixPauseRestarted.PIDs[pid] = true
		restarted = append(restarted, fmt.Sprintf("gateway PID %s → %d", pyrepr.Str(entry["pid"]), pid))
	}
	token["unmapped"], token["unmapped_pids"], token["profiles"] = keptBare, keptPids, map[string]any{}
	if len(restarted) > 0 {
		fmt.Fprintf(w, "  ▶ Restarted paused gateway(s): %s\n", strings.Join(restarted, ", "))
	}
	if len(failures) > 0 {
		return errors.New("Could not restart every paused gateway: " + strings.Join(failures, "; "))
	}
	token["resume_needed"], token["posix_stopped"] = false, false
	return nil
}

// What the early resume restarted on the new code in this process
func alreadyRestarted() *pauseRestartedSets { return posixPauseRestarted }
