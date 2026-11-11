package gateway

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	preBuildHint         = "  Pre-build first:  npm install --workspace web && npm run build -w web"
	dashboardSystemdUnit = "icebath-dashboard.service"
)

// Host facts of the dashboard restart and status paths, swapped by tests
var (
	respawnLivenessGraceSeconds = 1.0
	dashboardPidExists          = proc.Exists
	dashboardWaitLaunchdPID     = waitForLaunchdServicePID
	dashboardSelfExecutable     = os.Executable
	dashboardSpawnDetached      = spawnDetachedDashboard
	dashboardSleep              = time.Sleep
)

// Pids of running dashboard and serve backends the caller may stop, kept to one home when scoped
func findStaleDashboardPIDs(ctx context.Context, excludePIDs map[int]bool, scopeHome string) []int {
	ancestors := callerAncestorPIDs()
	pids := []int{}
	for _, row := range scanDashboardProcesses(ctx, excludePIDs) {
		if !isCallerWrapperShell(row.pid, ancestors) {
			pids = append(pids, row.pid)
		}
	}
	if scopeHome != "" {
		return pidsOwnedByHermesHome(ctx, pids, scopeHome)
	}
	return pids
}

// Loopback address for probing a wildcard or empty bind, the bracket stripped host otherwise
func dashboardProbeHost(host string) string {
	if host == "" {
		host = "127.0.0.1"
	}
	normalized := strings.Trim(pystr.Strip(host), "[]")
	switch normalized {
	case "", "0.0.0.0", "::":
		return "127.0.0.1"
	}
	return normalized
}

func probeText(raw []byte) string { return pystr.DecodeUTF8Replace(raw) }

// Restarts a systemd managed dashboard unit instead of killing its pid, true when a unit was found and handled
func restartManagedDashboardService(ctx context.Context, w io.Writer, reason, unit string) bool {
	if dashboardGOOS == "windows" {
		return false
	}
	systemctl := func(timeout time.Duration, args ...string) (int, string, string, error) {
		result, err := dashboardRunProbe(ctx, append([]string{"systemctl"}, args...), timeout)
		if err != nil {
			return 0, "", "", err
		}
		return result.Returncode, probeText(result.Stdout), probeText(result.Stderr), nil
	}
	var scope []string
	found := false
	for _, candidate := range [][]string{{"--user"}, {}} {
		code, stdout, _, err := systemctl(10*time.Second, append(slices.Clone(candidate), "list-unit-files", unit, "--no-legend", "--no-pager")...)
		if err != nil || code != 0 {
			continue
		}
		listed := false
		for _, row := range pystr.Splitlines(stdout, false) {
			if fields := pystr.SplitWhitespace(row, -1); len(fields) > 0 && fields[0] == unit {
				listed = true
				break
			}
		}
		if listed {
			scope, found = candidate, true
			break
		}
	}
	if !found {
		return false
	}
	_, activeOut, _, err := systemctl(10*time.Second, append(slices.Clone(scope), "is-active", unit)...)
	if err != nil {
		return false
	}
	_, enabledOut, _, err := systemctl(10*time.Second, append(slices.Clone(scope), "is-enabled", unit)...)
	if err != nil {
		return false
	}
	activeState, enabledState := pystr.Strip(activeOut), pystr.Strip(enabledOut)
	enabledStates := []string{"enabled", "enabled-runtime", "linked", "linked-runtime", "static", "generated"}
	if activeState != "active" && !slices.Contains(enabledStates, enabledState) {
		return false
	}
	fmt.Fprintf(w, "\n⟲ Restarting managed dashboard service (%s)\n", reason)
	scopeLabel := "sudo systemctl"
	if len(scope) > 0 {
		scopeLabel = "systemctl --user"
	}
	commands := [][]string{append(append([]string{"systemctl"}, scope...), "restart", unit)}
	if len(scope) == 0 {
		commands = append(commands, []string{"sudo", "-n", "systemctl", "restart", unit})
	}
	var errs []string
	for _, command := range commands {
		result, err := dashboardRunProbe(ctx, command, 60*time.Second)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %s", strings.Join(command, " "), err))
			continue
		}
		if result.Returncode == 0 {
			fmt.Fprintf(w, "    ✓ restarted %s\n", unit)
			return true
		}
		detail := probeText(result.Stderr)
		if detail == "" {
			detail = probeText(result.Stdout)
		}
		errs = append(errs, fmt.Sprintf("%s: %s", strings.Join(command, " "), pystr.Strip(detail)))
	}
	fmt.Fprintf(w, "    ✗ failed to restart %s\n", unit)
	for _, e := range errs {
		if pystr.Strip(e) != "" {
			fmt.Fprintf(w, "      %s\n", e)
		}
	}
	fmt.Fprintln(w, "  Dashboard is managed by systemd; not raw-killing its PID because "+
		"systemd would treat that as a clean stop.")
	fmt.Fprintf(w, "  Restart manually: %s restart %s\n", scopeLabel, unit)
	return true
}

// Restarts a unit through systemctl in the scope of its cgroup, system then user when unknown, true on success
func tryRestartSystemdService(ctx context.Context, svcName, cgroupPath string) bool {
	scope := ""
	if cgroupPath != "" {
		scope = extractScopeFromCgroup(cgroupPath)
	}
	systemCmd := []string{"systemctl", "restart", svcName}
	userCmd := []string{"systemctl", "--user", "restart", svcName}
	candidates := [][]string{systemCmd, userCmd}
	switch scope {
	case "user":
		candidates = [][]string{userCmd}
	case "system":
		candidates = [][]string{systemCmd}
	}
	for _, cmd := range candidates {
		if result, err := dashboardRunProbe(ctx, cmd, 15*time.Second); err == nil && result.Returncode == 0 {
			return true
		}
	}
	return false
}

// Kickstarts a launchd job without killing it and requires a live pid other than the old one inside the timeout
func restartLaunchdJob(ctx context.Context, domain, label string, oldPid int, haveOld bool, timeout float64) bool {
	result, err := dashboardRunProbe(ctx, []string{"launchctl", "kickstart", domain + "/" + label}, 30*time.Second)
	if err != nil || result.Returncode != 0 {
		return false
	}
	fresh, err := dashboardWaitLaunchdPID(ctx, label, oldPid, haveOld, timeout, domain)
	return err == nil && fresh
}

// Rebuilds a captured interpreter plus launcher argv on the binary of this install, other shapes unchanged
func respawnableCommandForCurrentInstall(argv []string) []string {
	self, err := dashboardSelfExecutable()
	if err == nil && len(argv) > 2 && strings.HasPrefix(filepath.Base(argv[0]), "python") && filepath.Base(argv[1]) == "icebath" && argv[1] != self {
		return append([]string{self}, argv[2:]...)
	}
	return slices.Clone(argv)
}

// Running child of a respawn, polled once after the liveness grace
type respawnedDashboard interface {
	Poll() (code int, exited bool)
}

type respawnedProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func (p *respawnedProcess) Poll() (int, bool) {
	select {
	case <-p.done:
		return p.cmd.ProcessState.ExitCode(), true
	default:
		return 0, false
	}
}

// Starts a detached child in its own session with no stdin and both outputs appended to the log
func spawnDetachedDashboard(command []string, logPath string) (respawnedDashboard, error) {
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()
	if len(command) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, logFile, logFile
	cmd.SysProcAttr = dashboardDetachedAttr()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &respawnedProcess{cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

// Respawns manually started dashboards detached after an update and returns the argvs that did not come back
func respawnDashboardProcesses(ctx context.Context, w io.Writer, commands [][]string) [][]string {
	type attempt struct {
		original, command []string
		child             respawnedDashboard
		err               string
	}
	var spawned, failed []attempt
	var respawned [][]string
	logPath := filepath.Join(home.Dir(ctx), "logs", "dashboard-restart.log")
	_ = os.MkdirAll(filepath.Dir(logPath), 0o777)
	for _, original := range commands {
		command := respawnableCommandForCurrentInstall(original)
		if slices.Contains(command, "dashboard") && !slices.Contains(command, "--no-open") {
			command = append(slices.Clone(command), "--no-open")
		}
		child, err := dashboardSpawnDetached(command, logPath)
		if err != nil {
			failed = append(failed, attempt{original: original, command: command, err: err.Error()})
			continue
		}
		spawned = append(spawned, attempt{original: original, command: command, child: child})
	}
	if len(spawned) > 0 {
		dashboardSleep(secondsDuration(respawnLivenessGraceSeconds))
	}
	for _, a := range spawned {
		if code, exited := a.child.Poll(); !exited {
			respawned = append(respawned, a.command)
		} else {
			a.err = fmt.Sprintf("child exited during the first %.0fs (code %d)", respawnLivenessGraceSeconds, code)
			failed = append(failed, a)
		}
	}
	for _, command := range respawned {
		fmt.Fprintf(w, "    ✓ restarted: %s\n", pyshlex.Join(command))
	}
	out := [][]string{}
	for _, a := range failed {
		fmt.Fprintf(w, "    ✗ failed to restart (%s): %s\n", pyshlex.Join(a.command), a.err)
		out = append(out, a.original)
	}
	return out
}

// Prints the live listening dashboard and serve processes and returns how many there are
func reportDashboardStatus(ctx context.Context, w io.Writer) int {
	binds := ledgerServeBinds()
	type liveRow struct {
		pid           int
		command, mode string
	}
	var live []liveRow
	for _, row := range scanDashboardProcesses(ctx, nil) {
		mode, host, port, ok := parseDashboardRuntime(row.command)
		if !ok {
			continue
		}
		if bind, recorded := binds[row.pid]; recorded {
			port = bind.port
			if bind.host != "" {
				host = bind.host
			}
		}
		if port <= 0 || !dashboardPidExists(row.pid) || !dashboardListening(host, port) {
			continue
		}
		live = append(live, liveRow{row.pid, row.command, mode})
	}
	if len(live) == 0 {
		fmt.Fprintln(w, "No icebath dashboard or serve processes running.")
		return 0
	}
	fmt.Fprintf(w, "%d icebath dashboard/serve process(es) running:\n", len(live))
	for _, row := range live {
		fmt.Fprintf(w, "    PID %d [%s]: %s\n", row.pid, row.mode, row.command)
	}
	return len(live)
}

// True when something accepts TCP connections at the probe address of host and port
func dashboardListening(host string, port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(dashboardProbeHost(host), strconv.Itoa(port)), 1500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
