package gatewaycli

import (
	"context"
	"fmt"
	"io"
	"math/big"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewayrun"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydeadline"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// One human line of an active work entry, installed by cmd/icebath over the update drain report
var DescribeActiveWorkUnit func(ctx context.Context, unit any, h string) (string, error)

// Flag a stop or restart takes to interrupt live sessions without asking
const interruptFlag = "--yes"

// Seconds a custom supervisor gets to register a fresh gateway pid after a graceful exit
const supervisedReplacementVerifyTimeout = 15.0

// sys.exit with code, the exit the gateway command turns into its status
func exitWith(code int) error { return &gateway.SystemExitError{Code: code} }

// Console the gateway command prints colored status lines through
func gatewayConsole(w io.Writer) *cliui.Console {
	return &cliui.Console{In: cliui.Default().In, Out: w}
}

// Prints each line on its own row
func printLines(w io.Writer, lines ...string) {
	for _, line := range lines {
		fmt.Fprintln(w, line)
	}
}

// int of a JSON number or numeric string, the ValueError text for anything else
func pyIntValue(v any) (int64, error) {
	switch n := v.(type) {
	case nil:
		return 0, nil
	case bool:
		if n {
			return 1, nil
		}
		return 0, nil
	case int:
		return int64(n), nil
	case int64:
		return n, nil
	case float64:
		return int64(n), nil
	case *big.Int:
		return n.Int64(), nil
	case string:
		if parsed, ok := pyint.Parse(pystr.Strip(n)); ok {
			return parsed.Int64(), nil
		}
		return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.Repr(n))
	}
	return 0, fmt.Errorf("int() argument must be a string, a bytes-like object or a real number, not '%s'", pyrepr.TypeName(v))
}

// incompatible verdict when the gateway answering for this home is one this version's clients refuse
func incompatibleOwnerVerdict(ctx context.Context) string {
	observed, err := gateway.DiscoverGatewayEndpointChecked(ctx, home.Dir(ctx), 1500*time.Millisecond)
	if err != nil || observed.State != "incompatible" {
		return ""
	}
	reason := observed.ReasonCode
	if reason == "" {
		reason = "unknown"
	}
	return "incompatible: " + reason
}

// Prints the cross version refusal with its fix when the running gateway cannot serve this version
func ReportIncompatibleOwner(ctx context.Context, w io.Writer, pids []int) bool {
	stale := incompatibleOwnerVerdict(ctx)
	if stale == "" {
		return false
	}
	running := "The running gateway"
	if len(pids) > 0 {
		running = "Gateway is running (PID: " + joinInts(pids, ", ") + ")"
	}
	fmt.Fprintf(w, "✗ %s but cannot serve this Icebath (%s)\n", running, stale)
	printLines(w, "  It was started by another Icebath version; TUI, Desktop and chat cannot attach to it.",
		"  Replace it with this version:", "    icebath gateway restart")
	return true
}

// Names the profiles that opted out of the host multiplexer by config, default profile only
func PrintStandaloneByConfig(ctx context.Context, w io.Writer) error {
	if active := profiles.GetActiveProfileName(ctx); active != "" && active != "default" {
		return nil
	}
	roster, err := profiles.ProfilesToServe(ctx, true, true, false)
	if err != nil {
		return err
	}
	served, err := profiles.ProfilesToServe(ctx, true, false, false)
	if err != nil {
		return err
	}
	servedNames := map[string]bool{"default": true}
	for _, p := range served {
		servedNames[p.Name] = true
	}
	names := []string{}
	for _, p := range roster {
		if !servedNames[p.Name] && !slices.Contains(names, p.Name) {
			names = append(names, p.Name)
		}
	}
	sort.Strings(names)
	if len(names) > 0 {
		fmt.Fprintf(w, "standalone by config (temporary compatibility shim): %s\n", strings.Join(names, ", "))
	}
	return nil
}

func joinInts(values []int, sep string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, sep)
}

// Active agents and work of a fresh state file whose writer is alive, nil otherwise
func liveWorkFromState(h string) (*pyjson.Object, error) {
	record := gateway.ReadRuntimeStatus(pypath.PathJoin(h, "gateway_state.json"))
	if record == nil || record.Len() == 0 || gateway.RuntimeStatusIsStale(record, gateway.RuntimeStatusStaleTTL) || !gateway.RuntimeStatusPidIsLive(record) {
		return nil, nil
	}
	agents, err := gateway.ParseActiveAgents(record.GetOr("active_agents", nil))
	if err != nil {
		return nil, err
	}
	work, ok := record.GetOr("active_work", nil).([]any)
	if !ok {
		work = []any{}
	}
	out := pyjson.NewObject()
	out.Set("active_agents", agents)
	out.Set("active_work", work)
	return out, nil
}

// What the gateway serving home runs now, the control socket first then the state file when no profile narrows it
func LiveWork(h, profile string) (*pyjson.Object, error) {
	var params *pyjson.Object
	if profile != "" {
		params = pyjson.NewObject()
		params.Set("profile", profile)
	}
	answer := gateway.QueryGatewayControl(h, "active-work", params, gateway.DefaultControlClientTimeout)
	if answer != nil {
		if _, ok := answer.GetOr("active_work", nil).([]any); ok {
			return answer, nil
		}
	}
	if profile != "" {
		return nil, nil
	}
	return liveWorkFromState(h)
}

// Homes a stop or restart takes down, the system unit's pinned home first and the host owner last
func candidateHomes(ctx context.Context, system bool) ([]string, error) {
	homes := []string{home.Dir(ctx)}
	contains := func(p string) bool {
		for _, h := range homes {
			if pypath.PathStr(h) == pypath.PathStr(p) {
				return true
			}
		}
		return false
	}
	if gateway.SupportsSystemdServices(ctx) {
		selected, err := gateway.SelectSystemdScope(ctx, system)
		if err != nil {
			return nil, err
		}
		if selected {
			foreign, err := gateway.ForeignSystemdUnitHome(ctx, true)
			if err != nil {
				return nil, err
			}
			if foreign == "" {
				unitPath, err := gateway.GetSystemdUnitPath(ctx, true)
				if err != nil {
					return nil, err
				}
				unitHome, err := gateway.ServiceHomeForUnit(ctx, unitPath, true)
				if err != nil {
					return nil, err
				}
				if !contains(unitHome) {
					homes = append([]string{unitHome}, homes...)
				}
			}
		}
	}
	if owner := hostMultiplexerForAllVerb(); owner != nil && !contains(owner.Home) {
		homes = append(homes, owner.Home)
	}
	return homes, nil
}

// Refusal text for verb while the first answering gateway among homes has live work, empty otherwise
func RefusalLines(ctx context.Context, verb string, homes []string, flagHint, profile string) ([]string, error) {
	if flagHint == "" {
		flagHint = "icebath gateway {verb} " + interruptFlag
	}
	for _, h := range homes {
		work, err := LiveWork(h, profile)
		if err != nil {
			return nil, err
		}
		if work != nil {
			return workLines(ctx, verb, h, work, flagHint)
		}
	}
	return nil, nil
}

func workLines(ctx context.Context, verb, h string, work *pyjson.Object, flagHint string) ([]string, error) {
	units := []any{}
	if list, ok := work.GetOr("active_work", nil).([]any); ok {
		for _, u := range list {
			if _, isDict := u.(*pyjson.Object); isDict {
				units = append(units, u)
			}
		}
	}
	agents, err := pyIntValue(work.GetOr("active_agents", nil))
	if err != nil {
		return nil, err
	}
	count := max(int64(len(units)), agents)
	if count == 0 {
		return nil, nil
	}
	lines := []string{fmt.Sprintf("Refusing to %s the gateway: %d live session(s) would be interrupted.", verb, count)}
	for _, u := range units {
		text, err := DescribeActiveWorkUnit(ctx, u, h)
		if err != nil {
			return nil, err
		}
		lines = append(lines, "  • "+text)
	}
	if len(units) == 0 {
		lines = append(lines, fmt.Sprintf("  • %d active agent run(s) (this gateway does not name them)", count))
	}
	lines = append(lines, "  Let them finish, or interrupt them deliberately:",
		fmt.Sprintf("    %s   # interrupts %d live session(s)", strings.ReplaceAll(flagHint, "{verb}", verb), count))
	return lines, nil
}

// Exits 1 naming the live work unless interrupt was passed or a terminal user confirms
func RefuseWhileSessionsLive(ctx context.Context, w io.Writer, verb string, ns map[string]any, hostHome, profile string) error {
	var lines []string
	var err error
	if profile != "" {
		hint := fmt.Sprintf("icebath -p %s gateway {verb} %s", profile, interruptFlag)
		lines, err = RefusalLines(ctx, verb, []string{hostHome}, hint, profile)
	} else {
		var homes []string
		if homes, err = candidateHomes(ctx, nsFlag(ns, "system")); err == nil {
			lines, err = RefusalLines(ctx, verb, homes, "", "")
		}
	}
	if err != nil || len(lines) == 0 {
		return err
	}
	con := gatewayConsole(w)
	refusing := "Refusing to " + verb + " the gateway"
	if nsFlag(ns, "interrupt") {
		con.PrintWarning(strings.Replace(lines[0], refusing, "--yes: "+verb+" proceeds", 1))
		printLines(w, lines[1:len(lines)-2]...)
		return nil
	}
	if con.StdinIsTTY() && con.StdoutIsTTY() {
		con.PrintWarning(strings.Replace(lines[0], refusing, pystr.Capitalize(verb)+" the gateway", 1))
		printLines(w, lines[1:len(lines)-2]...)
		if con.PromptYesNo(ctx, fmt.Sprintf("Interrupt them and %s the gateway?", verb), false) {
			return nil
		}
		done := "restarted"
		if verb == "stop" {
			done = "stopped"
		}
		fmt.Fprintf(w, "  Cancelled: the gateway was not %s.\n", done)
		return exitWith(1)
	}
	con.PrintError(lines[0])
	printLines(w, lines[1:]...)
	return exitWith(1)
}

// Truthiness of a namespace flag, false when absent
func nsFlag(ns map[string]any, key string) bool { return pyrepr.Truthy(ns[key]) }

// Whether the running gateway pid was launched for an external supervisor by identify, live argv or recorded argv
func GatewayDeclaresExternalSupervisor(pid int, h string) bool {
	if pid <= 1 {
		return false
	}
	if h == "" {
		h = home.Process()
	}
	identity := gateway.IdentifyGateway(h, gateway.DefaultControlClientTimeout)
	if identity != nil && pyrepr.Equal(identity.GetOr("pid", nil), int64(pid)) && gateway.LaunchSupervisor(identity) != "" && gateway.LaunchSupervisor(identity) != "manual" {
		return true
	}
	argv := []any{}
	if live := gateway.CaptureGatewayArgv(pid); live != nil {
		for _, a := range live {
			argv = append(argv, a)
		}
	} else {
		record := gateway.ReadRuntimeStatus(pypath.PathJoin(h, "gateway_state.json"))
		if record != nil && pyrepr.Equal(record.GetOr("pid", nil), int64(pid)) {
			recorded, _ := record.GetOr("argv", nil).([]any)
			argv = recorded
		}
	}
	for _, a := range argv {
		if a == "--external-supervisor" {
			return true
		}
	}
	return false
}

// Fresh supervised gateway pid once the supervisor relaunches it, zero when the timeout passes
func waitForSupervisedGatewayReplacement(oldPid int, timeout, pollInterval float64) int {
	fresh := func() (int, bool) {
		pid, ok, err := gateway.GetRunningPid("", true)
		if err != nil || !ok || pid <= 0 || pid == oldPid {
			return 0, false
		}
		return pid, true
	}
	return pydeadline.PollUntil(fresh, time.Duration(max(timeout, 0.5)*float64(time.Second)), time.Duration(pollInterval*float64(time.Second)))
}

// Hands the gateway back to its supervisor with a SIGUSR1 drain and reports the fresh pid, exit 1 on failure
func RestartExternallySupervisedGateway(ctx context.Context, w io.Writer, supervisedPid int) error {
	budget, err := gateway.GetRestartExitWaitBudget(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "→ Restarting externally-supervised gateway (PID %d) — draining in-flight runs (up to %.0fs)...\n", supervisedPid, budget)
	if gateway.GracefulRestartViaSIGUSR1(supervisedPid, budget, nil) {
		if replacement := waitForSupervisedGatewayReplacement(supervisedPid, supervisedReplacementVerifyTimeout, 0.5); replacement != 0 {
			fmt.Fprintln(w)
			fmt.Fprintf(w, "✓ Gateway relaunched by its supervisor (PID %d)\n", replacement)
			return nil
		}
		fmt.Fprintln(w, "⚠ Supervisor did not relaunch the gateway after its graceful exit")
	} else {
		fmt.Fprintf(w, "⚠ Gateway did not exit within %.0fs of SIGUSR1 (or the signal could not be sent)\n", budget)
	}
	printLines(w, "", "✗ Not stopping or foreground-running a supervisor-owned gateway.",
		"  Check the supervisor (it may be unloaded, wedged, or stopped retrying),",
		"  then rerun once it is healthy: icebath gateway restart")
	return exitWith(1)
}

// Refuses the manual restart fallback when systemd owns the gateway pid under any unit name
func RefuseRestartOfServiceManagedGateway(ctx context.Context, w io.Writer, pid int) error {
	if pid <= 1 {
		return nil
	}
	unit, scope, ok := gateway.SystemdOwnerForPid(ctx, pid)
	if !ok {
		return nil
	}
	cmds, err := gateway.SystemdRestartCommands(unit, scope)
	if err != nil {
		return err
	}
	lines := []string{"", fmt.Sprintf("✗ Gateway (PID %d) is managed by systemd unit %s.", pid, unit),
		"  `icebath gateway restart` cannot restart it from here: the fallback would",
		"  kill a live gateway and spawn an unsupervised replacement in this shell's",
		"  cgroup while the unit keeps failing against the stolen lock.",
		"  Restart it through its unit instead:"}
	for _, c := range cmds {
		lines = append(lines, "    "+c)
	}
	lines = append(lines, "  (Pre-convention `icebath.service` installs: `icebath gateway migrate-legacy`",
		"  removes the legacy unit so the canonical one can own the gateway.)")
	printLines(w, lines...)
	return exitWith(1)
}

// Whether a host control answer confirms name under key without an error
func ackConfirmed(answer *pyjson.Object, key, name string) bool {
	return answer != nil && answer.GetOr(key, nil) == name && !pyrepr.Truthy(answer.GetOr("error", nil))
}

// Why a host control answer did not confirm the operation
func ackFailure(answer *pyjson.Object) string {
	if answer == nil {
		return "host control socket did not answer"
	}
	if v := answer.GetOr("error", nil); pyrepr.Truthy(v) {
		return pyrepr.Str(v)
	}
	return "host operation is still pending"
}

// Acts as if h were the active home, the context override and ICEBATH_HOME switched and the launch home pinned
func homeEnv(ctx context.Context, h string) (context.Context, func()) {
	return gatewayrun.HomeEnv(ctx, h)
}

// Home scope an all verb acts in, the live host owner's home else the default root
func hostScopeForAllVerb(ctx context.Context, owner *gateway.HostGateway) (context.Context, func()) {
	if owner != nil {
		return homeEnv(ctx, owner.Home)
	}
	return homeEnv(ctx, home.Root())
}

// Live host gateway an all verb must target instead of sweeping every profile, nil when none answers
func hostMultiplexerForAllVerb() (owner *gateway.HostGateway) {
	defer func() {
		if recover() != nil {
			owner = nil
		}
	}()
	return gateway.HostGatewayNow()
}

// Prints the parked line of this satellite or the parked roster of the default profile, true for a parked satellite
func PrintParkedStatus(ctx context.Context, w io.Writer) (bool, error) {
	name, err := gateway.CurrentProfileName(ctx)
	if err != nil {
		return false, err
	}
	if name != "" && name != "default" && profiles.ProfileIsParked(home.Dir(ctx)) {
		fmt.Fprintf(w, "Profile '%s': parked (icebath -p %s gateway start)\n", name, name)
		return true, nil
	}
	if name == "" || name == "default" {
		served, err := profiles.ProfilesToServe(ctx, true, false, true)
		if err != nil {
			return false, err
		}
		parked := []string{}
		for _, p := range served {
			if p.Name != "default" && profiles.ProfileIsParked(p.Home) {
				parked = append(parked, p.Name)
			}
		}
		if len(parked) > 0 {
			if owner := gateway.HostMultiplexerServingOwner(ctx, nil); owner != nil {
				fmt.Fprintf(w, "Served profiles: %s\n", strings.Join(owner.Profiles, ", "))
			}
			for _, p := range parked {
				fmt.Fprintf(w, "Profile '%s': parked (icebath -p %s gateway start)\n", p, p)
			}
		}
	}
	return false, nil
}

// Handles start, stop or restart of a named profile inside the host gateway, true when handled
func ProfileLifecycle(ctx context.Context, w io.Writer, command string, ns map[string]any) (bool, error) {
	name, err := gateway.CurrentProfileName(ctx)
	if err != nil {
		return false, err
	}
	if name == "" || name == "default" || nsFlag(ns, "all") || nsFlag(ns, "force") {
		return false, nil
	}
	h := home.Dir(ctx)
	standalone, err := profiles.ProfileIsStandalone(ctx, h)
	if err != nil || standalone {
		return false, err
	}
	marker := profiles.ParkedMarkerPath(h)
	if command == "restart" && profiles.ProfileIsParked(h) {
		pids, err := gateway.FindGatewayPIDsFor(ctx, nil, false)
		if err != nil {
			return false, err
		}
		if len(pids) == 0 {
			command = "start"
		}
	}
	if command == "start" {
		if !profiles.ProfileIsParked(h) {
			return false, nil
		}
		if err := os.Remove(marker); err != nil {
			return false, err
		}
		owner := hostMultiplexerForAllVerb()
		if owner == nil {
			_, live, err := gateway.LiveDefaultGatewayPid()
			if err != nil {
				return false, err
			}
			if !live {
				return false, nil
			}
		}
		hostHome := home.Root()
		if owner != nil {
			hostHome = owner.Home
		}
		answer := gateway.RequestServeProfileHot(hostHome, name)
		if ackConfirmed(answer, "served", name) {
			fmt.Fprintf(w, "Profile '%s' served by the host gateway.\n", name)
		} else {
			fmt.Fprintf(w, "Profile '%s' unparked, but serving was not confirmed: %s.\n", name, ackFailure(answer))
			fmt.Fprintln(w, "The host retries on its next rescan (within 30s). Check gateway status.")
		}
		return true, nil
	}
	pids, err := gateway.FindGatewayPIDsFor(ctx, nil, false)
	if err != nil || len(pids) > 0 {
		return false, err
	}
	owner := gateway.ServedByAnotherHostGateway(ctx, nil)
	if owner == nil {
		served, err := gateway.NamedProfileServedByRunningMultiplexerCtx(ctx, nil)
		if err != nil || !served {
			return false, err
		}
	}
	hostHome := home.Root()
	if owner != nil {
		hostHome = owner.Home
	}
	if err := RefuseWhileSessionsLive(ctx, w, command, ns, hostHome, name); err != nil {
		return false, err
	}
	if command == "stop" {
		if err := touchFile(marker); err != nil {
			return false, err
		}
		answer := gateway.RequestUnserveProfile(hostHome, name)
		if ackConfirmed(answer, "unserved", name) {
			fmt.Fprintf(w, "Profile '%s' parked; its bots and cron are stopped. Start again with: icebath -p %s gateway start\n", name, name)
		} else {
			fmt.Fprintf(w, "Profile '%s' parked, but immediate stop was not confirmed: %s.\n", name, ackFailure(answer))
			fmt.Fprintln(w, "The host drops it on its next rescan (within 30s).")
		}
		return true, nil
	}
	answer := gateway.RequestUnserveProfile(hostHome, name)
	if !ackConfirmed(answer, "unserved", name) {
		fmt.Fprintf(w, "Profile '%s' restart was not confirmed: %s.\n", name, ackFailure(answer))
		return true, nil
	}
	answer = gateway.RequestServeProfileHot(hostHome, name)
	if ackConfirmed(answer, "served", name) {
		fmt.Fprintf(w, "Profile '%s' restarted by the host gateway.\n", name)
	} else {
		fmt.Fprintf(w, "Profile '%s' stopped, but serving was not confirmed: %s.\n", name, ackFailure(answer))
		fmt.Fprintln(w, "The host retries on its next rescan (within 30s). Check gateway status.")
	}
	return true, nil
}

// Creates path or bumps its modification time, the pathlib touch
func touchFile(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	now := time.Now()
	return os.Chtimes(path, now, now)
}
