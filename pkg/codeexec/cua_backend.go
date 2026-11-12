package codeexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	cuaTelemetryEnvVar     = "CUA_DRIVER_RS_TELEMETRY_ENABLED"
	cuaNativeWaylandEnvVar = "CUA_DRIVER_RS_ENABLE_WAYLAND"
	defaultAXMaxElements   = 200
)

// completedProcess is the subprocess.CompletedProcess of a text mode run
type completedProcess struct {
	Args       []string
	Returncode int
	Stdout     string
	Stderr     string
}

// subprocessTimeout is subprocess.TimeoutExpired, a SubprocessError
type subprocessTimeout struct {
	Cmd     []string
	Timeout float64
}

func (e *subprocessTimeout) Error() string {
	items := make([]any, len(e.Cmd))
	for i, a := range e.Cmd {
		items[i] = a
	}
	return fmt.Sprintf("Command '%s' timed out after %s seconds", pyrepr.Repr(items), pyrepr.Float(e.Timeout))
}

// spawnError is the OSError subprocess raises when the program cannot start
type spawnError struct{ err error }

func (e *spawnError) Error() string { return pypath.OSErrorText(e.err) }
func (e *spawnError) Unwrap() error { return e.err }

// runOpts are the subprocess.run keywords the probe helpers forward
type runOpts struct {
	Env        map[string]string
	Stdin      io.Reader
	NoCapture  bool
	HideWindow bool
}

// Text of a universal newline decode with invalid bytes replaced
func textModeDecode(b []byte) string {
	s := pystr.DecodeUTF8Replace(b)
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// Runs a short probe verb with stdin closed and output captured, killed with a timeout error past the deadline
var runQuiet = func(argv []string, timeout float64, o runOpts) (*completedProcess, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout*float64(time.Second)))
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = time.Second
	if o.Env != nil {
		cmd.Env = envList(o.Env)
	}
	cmd.Stdin = o.Stdin
	var stdout, stderr bytes.Buffer
	if !o.NoCapture {
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
	} else {
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	}
	if o.HideWindow {
		hideConsoleWindow(cmd)
	}
	if err := cmd.Start(); err != nil {
		return nil, &spawnError{err}
	}
	err := cmd.Wait()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, &subprocessTimeout{Cmd: argv, Timeout: timeout}
	}
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
		if code < 0 {
			code = -signalNumber(exitErr)
		}
	} else if err != nil {
		return nil, &spawnError{err}
	}
	return &completedProcess{Args: argv, Returncode: code, Stdout: textModeDecode(stdout.Bytes()), Stderr: textModeDecode(stderr.Bytes())}, nil
}

// Runs a short cua-driver verb with the sanitized env and a hidden console window
func runDriver(env *tools.Env, driverCmd string, timeout float64, args ...string) (*completedProcess, error) {
	return runQuiet(append([]string{driverCmd}, args...), timeout, runOpts{Env: sanitizedCuaDriverEnv(env), HideWindow: true})
}

// Active profile config through the writable loader, swapped by tests that pin a config
var loadCUConfig = func(env *tools.Env) (config.Tree, error) {
	h := envHome(env)
	lookup := config.NewSecrets(h).Lookup()
	if env != nil && env.Secrets != nil {
		lookup = env.Secrets.Lookup()
	}
	return config.LoadContext(context.Background(), h, lookup)
}

// The computer_use config block of the active profile, empty when config is unreadable
func computerUseCfg(env *tools.Env) map[string]any {
	tree, err := loadCUConfig(env)
	if err != nil || tree == nil {
		return map[string]any{}
	}
	if section, ok := tree["computer_use"].(map[string]any); ok && len(section) > 0 {
		return section
	}
	return map[string]any{}
}

// Whether --no-overlay is passed, the config value overrides the platform and display auto detection
var cuaNoOverlay = func(env *tools.Env) bool {
	val, set := computerUseCfg(env)["no_overlay"]
	if set && val == nil {
		set = false
	}
	if set || cuPlatform != "linux" {
		if set {
			return pyrepr.Truthy(val)
		}
		return cuPlatform == "darwin"
	}
	return hostinfo.IsWSL() || os.Getenv("DISPLAY") == "" ||
		(os.Getenv("XDG_SESSION_TYPE") != "wayland" && os.Getenv("WAYLAND_DISPLAY") == "")
}

// Telemetry is disabled unless computer_use.cua_telemetry opts in
var cuaTelemetryDisabled = func(env *tools.Env) bool {
	val, ok := computerUseCfg(env)["cua_telemetry"]
	return !(ok && pyrepr.Truthy(val))
}

// Configured permission mode, bounded or standard with every other value falling closed to standard
func cuaConfiguredPermissionMode(env *tools.Env) string {
	raw, ok := computerUseCfg(env)["permission_mode"]
	if !ok {
		raw = "standard"
	}
	text := ""
	if pyrepr.Truthy(raw) {
		text = pyrepr.Str(raw)
	}
	if pystr.Lower(pystr.Strip(text)) == "bounded" {
		return "bounded"
	}
	return "standard"
}

// Bound on the driver accessibility walk, zero for no bound, unparseable values fall back to the default
func cuaConfiguredAXMaxElements(env *tools.Env) (int, error) {
	raw, ok := computerUseCfg(env)["ax_max_elements"]
	if !ok {
		raw = defaultAXMaxElements
	}
	n, err := pyint.Of(raw)
	if err != nil {
		var pe *pyint.Error
		if errors.As(err, &pe) && (pe.Kind == "TypeError" || pe.Kind == "ValueError") {
			return defaultAXMaxElements, nil
		}
		return 0, err
	}
	if n.Sign() < 0 {
		return 0, nil
	}
	if !n.IsInt64() || n.Int64() > math.MaxInt {
		return math.MaxInt, nil
	}
	return int(n.Int64()), nil
}

// True when the capability manifest declares version three or newer and so suits any permission mode
func manifestIsModeIndependent(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		slog.Debug(fmt.Sprintf("could not read capability manifest %s", path), "err", err)
		return false
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		slog.Debug(fmt.Sprintf("could not read capability manifest %s", path), "err", err)
		return false
	}
	parsed, err := config.FastSafeLoad([]byte(text))
	if err != nil {
		slog.Debug(fmt.Sprintf("could not read capability manifest %s", path), "err", err)
		return false
	}
	m, ok := parsed.(map[string]any)
	if !ok {
		return false
	}
	if _, isBool := m["version"].(bool); isBool {
		return false
	}
	n, ok := pyrepr.Int(m["version"])
	return ok && n.Cmp(big.NewInt(3)) >= 0
}

// Longest edge cap of captured images, nil when zero or negative
func computerUseMaxImageDimension(env *tools.Env) (*int, error) {
	raw, ok := computerUseCfg(env)["max_image_dimension"]
	if !ok {
		raw = 1456
	}
	dim := 1456
	n, err := pyint.Of(raw)
	if err != nil {
		var pe *pyint.Error
		if !errors.As(err, &pe) || (pe.Kind != "TypeError" && pe.Kind != "ValueError") {
			return nil, err
		}
	} else if !n.IsInt64() || n.Int64() > math.MaxInt {
		dim = math.MaxInt
		if n.Sign() < 0 {
			dim = -1
		}
	} else {
		dim = int(n.Int64())
	}
	if dim > 0 {
		return &dim, nil
	}
	return nil, nil
}

// DISPLAY a backend spawned from childEnv acts on, the fresh driver child env when childEnv is nil
func desktopIdentity(env *tools.Env, childEnv map[string]string) string {
	if childEnv == nil {
		childEnv = cuaDriverChildEnv(env, nil)
	}
	return childEnv["DISPLAY"]
}

// True when a cached backend's recorded display no longer matches a fresh spawn's
func backendDisplayStale(recorded, current string) bool { return recorded != current }

// Driver spawn env over the process environment with the bot desktop display, telemetry policy and Wayland opt in
func CuaDriverChildEnv(env *tools.Env) map[string]string { return cuaDriverChildEnv(env, nil) }

// Driver spawn env with the bot desktop display, the telemetry policy and the native Wayland opt in
func cuaDriverChildEnv(env *tools.Env, base map[string]string) map[string]string {
	if base == nil {
		base = environMap()
	}
	out := make(map[string]string, len(base)+2)
	for k, v := range base {
		out[k] = v
	}
	out = BotDesktopEnv(env, out)
	if cuaTelemetryDisabled(env) {
		out[cuaTelemetryEnvVar] = "0"
	}
	if cuPlatform == "linux" && out["WAYLAND_DISPLAY"] != "" && pyrepr.Truthy(computerUseCfg(env)["native_wayland"]) {
		out[cuaNativeWaylandEnvVar] = "1"
	}
	return out
}

// mcpInvocation is a command and its argument list
type mcpInvocation struct {
	Command string
	Args    []string
}

// Driver invocation inside the terminal backend hosting the bot desktop, nil for a gateway hosted desktop
func sandboxMCPInvocation(ctx context.Context, env *tools.Env) (*mcpInvocation, map[string]string, error) {
	where, err := BotDesktopToolPlacement(env)
	if err != nil {
		return nil, nil, err
	}
	if where == botDesktopGateway {
		return nil, nil, nil
	}
	published := BotDesktopPublishedEnv(env)
	if published["DISPLAY"] == "" {
		return nil, nil, errors.New("the screen inside the terminal backend's sandbox is gone; start it again")
	}
	sandbox, err := BotDesktopSandboxEnv(env, true)
	if err != nil {
		return nil, nil, err
	}
	if sandbox == nil {
		return nil, nil, errors.New("the terminal backend's sandbox is not running, so there is nowhere to run cua-driver")
	}
	childPublished := make(map[string]string, len(published)+1)
	for k, v := range published {
		childPublished[k] = v
	}
	childPublished[cuaTelemetryEnvVar] = "0"
	command, args, err := BotDesktopCuaMCPInvocation(ctx, sandbox, BotDesktopProfileName(env), childPublished)
	if err != nil {
		return nil, nil, err
	}
	BotDesktopTouchActivity(env)
	return &mcpInvocation{command, args}, map[string]string{"PATH": os.Getenv("PATH")}, nil
}

// Driver child env with provider secrets stripped, the unsanitized env when the sanitizer fails
func sanitizedCuaDriverEnv(env *tools.Env) map[string]string {
	child := cuaDriverChildEnv(env, nil)
	if out, err := sanitizeCuaEnv(env, child); err == nil {
		return out
	}
	return child
}

// Secret scrub of the terminal sanitizer, a panic in it counts as a failure like the suppressed exception
func sanitizeCuaEnv(env *tools.Env, child map[string]string) (out map[string]string, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("%v", r)
		}
	}()
	return tools.SanitizeSubprocessEnv(env, child, nil)
}

// Socket liveness of a serve daemon, nil when the probe itself failed
func cuaDaemonListening(env *tools.Env, driverCmd, socketPath string, timeout float64) *bool {
	args := []string{"status"}
	if socketPath != "" {
		args = append(args, "--socket", socketPath)
	}
	proc, err := runDriver(env, driverCmd, timeout, args...)
	if err != nil {
		return nil
	}
	t, f := true, false
	if proc.Returncode == 0 {
		return &t
	}
	if strings.Contains(pystr.Lower(proc.Stdout+"\n"+proc.Stderr), "not running") {
		return &f
	}
	return nil
}

// Whether the graphical session is locked per loginctl, nil off Linux or when loginctl cannot answer
func linuxSessionLocked() *bool {
	if cuPlatform != "linux" {
		return nil
	}
	proc, err := runQuiet([]string{"loginctl", "list-sessions", "--no-legend"}, 2.0, runOpts{})
	if err != nil {
		return nil
	}
	var seats []string
	for _, line := range pystr.Splitlines(proc.Stdout, false) {
		fields := pystr.SplitWhitespace(line, -1)
		if len(fields) >= 2 && strings.Contains(line, "seat") {
			seats = append(seats, fields[0])
		}
	}
	if proc.Returncode != 0 || len(seats) == 0 {
		return nil
	}
	for _, s := range seats {
		show, err := runQuiet([]string{"loginctl", "show-session", s, "-p", "LockedHint"}, 2.0, runOpts{})
		if err != nil {
			return nil
		}
		if strings.Contains(show.Stdout, "LockedHint=no") {
			f := false
			return &f
		}
	}
	t := true
	return &t
}

// One line diagnosis for a window discovery that found nothing
func emptyDiscoveryReason() string {
	if locked := linuxSessionLocked(); locked != nil && *locked {
		return "the desktop session is LOCKED (loginctl LockedHint=yes) — unlock the screen; " +
			"a locked compositor hides windows and freezes app renderers"
	}
	if cuPlatform == "linux" && os.Getenv("DISPLAY") == "" {
		return "no DISPLAY is set — X11/XWayland is not reachable from this process"
	}
	if cuPlatform == "darwin" {
		return "window discovery returned no windows; on macOS this usually means no shareable display (headless Mac or " +
			"panel asleep) — wake the display or attach a monitor/HDMI dummy, then run `icebath computer-use doctor`"
	}
	return "window discovery returned no windows; run `icebath computer-use doctor` (display reachability, AX capability)"
}
