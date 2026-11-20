package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Launch policy and pins of one client that never become the shared daemon's environment
var DaemonStrippedEnv = []string{
	"ICEBATH_PROFILE", "ICEBATH_YOLO_MODE", "ICEBATH_IGNORE_RULES", "ICEBATH_SAFE_MODE",
	"ICEBATH_IGNORE_USER_CONFIG", "ICEBATH_ACCEPT_HOOKS", "ICEBATH_SESSION_SOURCE", "TERMINAL_CWD",
	"ICEBATH_KANBAN_TASK", "ICEBATH_KANBAN_WORKSPACE", "ICEBATH_KANBAN_BRANCH", "ICEBATH_KANBAN_RUN_ID",
	"ICEBATH_KANBAN_CLAIM_LOCK", "ICEBATH_KANBAN_CLAIM_TTL_SECONDS", "ICEBATH_KANBAN_GOAL_MODE",
	"ICEBATH_KANBAN_GOAL_MAX_TURNS", "ICEBATH_KANBAN_DB", "ICEBATH_KANBAN_WORKSPACES_ROOT",
	"ICEBATH_KANBAN_BOARD", "ICEBATH_TENANT", "ICEBATH_SESSION_SOURCE_EXPLICIT", "ICEBATH_TURN_AUTHOR",
	"ICEBATH_INTERACTIVE", "ICEBATH_GATEWAY_SESSION", "ICEBATH_SINGLE_QUERY_SESSION",
	"ICEBATH_EPHEMERAL_SYSTEM_PROMPT", "ICEBATH_PREFILL_MESSAGES_FILE", "ICEBATH_INFERENCE_PROVIDER",
	"ICEBATH_ENABLE_PROJECT_PLUGINS", "ICEBATH_TUI_CLIENT_ENV",
	"INVOCATION_ID", "JOURNAL_STREAM",
	"XPC_SERVICE_NAME", "LAUNCHD_SOCKET", "ICEBATH_LAUNCHD_LABEL",
	"ICEBATH_GATEWAY_EXTERNAL_SUPERVISOR", "ICEBATH_SUPERVISED_CHILD",
	"NOTIFY_SOCKET", "WATCHDOG_USEC", "WATCHDOG_PID",
}

// Marker a Windows gateway child reads to know its launcher already broke it away from the job
const windowsGatewayBreakawayEnv = "_ICEBATH_GATEWAY_BREAKAWAY"

// SpawnedGateway is a requested daemon, polled for an exit status without waiting
type SpawnedGateway interface {
	Poll() (int, bool)
	Pid() int
}

// Detached child reaped in the background so polling never blocks
type unmanagedChild struct {
	cmd    *exec.Cmd
	mu     sync.Mutex
	done   bool
	status int
}

func (c *unmanagedChild) Pid() int { return c.cmd.Process.Pid }

// Exit status once the child ended, Popen returncode style with a negative signal number
func (c *unmanagedChild) Poll() (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status, c.done
}

// Blocks until the child ends and returns its status
func (c *unmanagedChild) Wait(timeout time.Duration) (int, bool) {
	end := time.Now().Add(timeout)
	for {
		if status, done := c.Poll(); done {
			return status, true
		}
		if time.Now().After(end) {
			return 0, false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Starts the background reaper recording the exit status
func (c *unmanagedChild) reap() {
	go func() {
		err := c.cmd.Wait()
		status := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			status = pysubprocess.Returncode(exitErr)
		}
		c.mu.Lock()
		c.status, c.done = status, true
		c.mu.Unlock()
	}()
}

// Swappable host facts of the spawn, the binary argv prefix and the process environment
var (
	gatewayLaunchPrefix = func() ([]string, error) {
		exe, err := os.Executable()
		return []string{exe}, err
	}
	gatewayLaunchEnviron = os.Environ
)

// Swappable unmanaged spawn, the seam ensure tests replace
var spawnUnmanagedGateway = SpawnUnmanagedGateway

// Argv of an unmanaged gateway run for a resolved home, a root home pinned to the default profile
func UnmanagedGatewayCommand(h string, idleExit, force bool) ([]string, error) {
	command, err := gatewayLaunchPrefix()
	if err != nil {
		return nil, err
	}
	if filepath.Base(filepath.Dir(h)) != "profiles" {
		command = append(command, "--profile", "default")
	}
	command = append(command, "gateway", "run", "--quiet")
	if idleExit {
		command = append(command, "--idle-exit")
	}
	if force {
		command = append(command, "--force")
	}
	return command, nil
}

// Requests a detached daemon for profile home with its own env, never readiness, persistence or a replace
func SpawnUnmanagedGateway(ctx context.Context, profileHome string, deadline float64, idleExit, force bool) (SpawnedGateway, error) {
	h, err := pypath.Resolve(profileHome)
	if err != nil {
		return nil, err
	}
	root := pm.RepoRoot()
	command, err := UnmanagedGatewayCommand(h, idleExit, force)
	if err != nil {
		return nil, runtimeStartError("spawn_failed")
	}
	base := map[string]string{}
	for _, kv := range gatewayLaunchEnviron() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			base[k] = v
		}
	}
	env, err := tools.ServedProfileChildEnv(ctx, &tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}, base, h, true)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS == "windows" {
		spec, _, overlay := windowlessGatewayRestartSpec(ctx, command, "")
		if len(overlay) == 0 {
			return nil, runtimeStartError("windows_interpreter_unavailable")
		}
		command = spec
		for k, v := range overlay {
			env[k] = v
		}
		env[windowsGatewayBreakawayEnv] = "1"
	}
	env["ICEBATH_HOME"], env["ICEBATH_GATEWAY_DETACHED"], env["PYTHONIOENCODING"] = h, "1", "utf-8"
	env = tools.CarryGatewayProfilePin(env)
	for _, key := range append(append([]string{}, DaemonStrippedEnv...), tools.SessionContextEnvVars...) {
		delete(env, key)
	}
	if _, err := runtimeRemaining(deadline); err != nil {
		return nil, err
	}
	logs := filepath.Join(h, "logs")
	if err := os.MkdirAll(logs, 0o777); err != nil {
		return nil, err
	}
	if runtime.GOOS == "darwin" {
		job, err := startGatewayJob(ctx, command, env, root, filepath.Join(logs, "gateway-stdio.log"), deadline)
		if err != nil {
			return nil, err
		}
		if job != nil {
			return job, nil
		}
	}
	output, err := os.OpenFile(filepath.Join(logs, "gateway-stdio.log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		return nil, spawnFailure(err)
	}
	defer output.Close()
	if _, err := runtimeRemaining(deadline); err != nil {
		return nil, err
	}
	environ := make([]string, 0, len(env))
	for k, v := range env {
		environ = append(environ, k+"="+v)
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = root, environ, output, output
	cmd.SysProcAttr = home.DetachAttr(true)
	child, err := gatewayPopen(cmd)
	if err != nil {
		return nil, spawnFailure(err)
	}
	return child, nil
}

// Swappable process start of the spawn, the boundary spawn tests capture
var gatewayPopen = func(cmd *exec.Cmd) (SpawnedGateway, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	child := &unmanagedChild{cmd: cmd}
	child.reap()
	return child, nil
}

// Start error of a failed spawn, the Windows breakaway refusal there
func spawnFailure(err error) error {
	if !pypath.IsOSError(err) {
		return err
	}
	if runtime.GOOS == "windows" {
		return runtimeStartError("windows_breakaway_unavailable")
	}
	return runtimeStartError("spawn_failed")
}

// Starts a stopped owner's replacement in the background, exit 1 with the reason when it cannot
func StartDetachedReplacement(ctx context.Context, profileHome string, force bool, stdout, stderr io.Writer) int {
	if _, err := SpawnUnmanagedGateway(ctx, profileHome, pythread.Monotonic()+30, false, force); err != nil {
		var start *RuntimeStartError
		if errors.As(err, &start) {
			fmt.Fprintf(stderr, "✗ Could not start the gateway in the background: %s\n", start.Reason)
			return 1
		}
		panic(err)
	}
	fmt.Fprintln(stdout, "✓ Started the gateway in the background")
	return 0
}

// Append only stdio log a detached daemon writes before its logging is up
func StdioLogPath(profileHome string) string {
	return filepath.Join(profileHome, "logs", "gateway-stdio.log")
}

// Offset where this launch's output starts in the stdio log, zero when it cannot be read
func StdioLogSize(profileHome string) int64 {
	info, err := os.Stat(StdioLogPath(profileHome))
	if err != nil {
		return 0
	}
	return info.Size()
}

var bannerPrefixes = []string{"┌", "│", "├", "└"}

const (
	startupTailLines = 15
	startupTailBytes = 64 * 1024
)

// Exit status, redacted tail of this launch's output and the command showing the whole log
func StartupFailureReport(ctx context.Context, profileHome string, offset int64, status int, pid *int) string {
	path := StdioLogPath(profileHome)
	text := ""
	if f, err := os.Open(path); err == nil {
		if info, err := os.Stat(path); err == nil {
			if _, err := f.Seek(max(offset, info.Size()-startupTailBytes), io.SeekStart); err == nil {
				if raw, err := io.ReadAll(f); err == nil {
					text, _ = pycodecs.DecodeReplace(raw, "utf-8")
				}
			}
		}
		f.Close()
	}
	var lines []string
	for _, line := range pystr.Splitlines(text, false) {
		if pystr.Strip(line) == "" {
			continue
		}
		stripped := pystr.Lstrip(line)
		banner := false
		for _, prefix := range bannerPrefixes {
			banner = banner || strings.HasPrefix(stripped, prefix)
		}
		if !banner {
			lines = append(lines, pystr.Rstrip(line))
		}
	}
	if len(lines) > startupTailLines {
		lines = lines[len(lines)-startupTailLines:]
	}
	if len(lines) == 0 {
		record := ReadRuntimeStatus(filepath.Join(profileHome, "gateway_state.json"))
		if reason := recGet(record, "exit_reason"); pyTruthy(reason) && pid != nil && pyEq(recGet(record, "pid"), int64(*pid)) {
			lines = []string{pystr.Slice(pyrepr.Str(reason), 0, 2000)}
			path = filepath.Join(profileHome, "logs", "gateway.log")
		}
	}
	tail := tools.RedactSensitiveTextWith(&tools.Env{Home: home.Dir(ctx)}, strings.Join(lines, "\n"), tools.RedactOptions{Force: true, URLCredentials: true})
	see := fmt.Sprintf("tail -n 200 '%s'", path)
	if runtime.GOOS == "windows" {
		see = fmt.Sprintf("type \"%s\"", path)
	}
	body := "  (it wrote no output)"
	if tail != "" {
		parts := pystr.Splitlines(tail, false)
		for i, line := range parts {
			parts[i] = "  " + line
		}
		body = strings.Join(parts, "\n")
	}
	return fmt.Sprintf("the gateway exited with status %d before it could serve:\n%s\nFull log: %s", status, body, see)
}
