package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Captured subprocess run with text output decoded with replacement characters
type CommandResult struct {
	Stdout     string
	Stderr     string
	ReturnCode int
}

var (
	ErrCommandTimeout  = errors.New("subprocess timed out")
	ErrCommandNotFound = errors.New("No such file or directory")
)

// Raised when systemctl is not installed
type SystemctlUnavailableError struct{}

func (SystemctlUnavailableError) Error() string { return "systemctl is not available on this system" }

var (
	serviceLookPath = exec.LookPath
	hostIsTermux    = hostinfo.IsTermux
	hostIsWSL       = hostinfo.IsWSL
	hostIsContainer = hostinfo.IsContainer
	serviceGetuid   = os.Getuid
)

// Runs argv under a timeout capturing text output, ErrCommandNotFound and ErrCommandTimeout for the two subprocess failures
var RunServiceCommand = func(ctx context.Context, timeout time.Duration, argv ...string) (CommandResult, error) {
	return runCommandDecoded(ctx, timeout, false, argv...)
}

// Subprocess run whose output decoding either replaces or drops undecodable bytes
func runCommandDecoded(ctx context.Context, timeout time.Duration, dropInvalid bool, argv ...string) (CommandResult, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.Env = UserSystemdEnv()
	cmd.WaitDelay = time.Second
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	repl := "�"
	if dropInvalid {
		repl = ""
	}
	res := CommandResult{Stdout: strings.ToValidUTF8(out.String(), repl), Stderr: strings.ToValidUTF8(errb.String(), repl)}
	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		return res, fmt.Errorf("%w: %q after %v", ErrCommandTimeout, argv, timeout)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ReturnCode = exitErr.ExitCode()
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			res.ReturnCode = -int(ws.Signal())
		}
		return res, nil
	}
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return res, fmt.Errorf("%w: %q", ErrCommandNotFound, argv[0])
		}
		return res, err
	}
	return res, nil
}

// True on Linux hosts
func IsLinux() bool { return serviceGOOS == "linux" || serviceGOOS == "android" }

// True on macOS hosts
func IsMacOS() bool { return serviceGOOS == "darwin" }

// True on Windows hosts
func IsWindows() bool { return serviceGOOS == "windows" }

// Per user runtime dir a systemctl user call reaches: this user's run dir when XDG_RUNTIME_DIR is unset or not ours and that dir is, else XDG_RUNTIME_DIR, else the run user path
func userRuntimeDir() string {
	own := fmt.Sprintf("/run/user/%d", serviceGetuid())
	xdg := os.Getenv("XDG_RUNTIME_DIR")
	if (xdg == "" || !runtimeDirIsOurs(xdg)) && runtimeDirIsOurs(own) {
		return own
	}
	if xdg != "" {
		return xdg
	}
	return own
}

// Path exists with any stat failure counted as absent
func pathExistsSafe(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Runtime dir exists and belongs to this uid
func runtimeDirIsOurs(dir string) bool {
	uid, ok := fileOwnerUID(dir)
	return ok && uid == serviceGetuid()
}

// True when the user bus socket or the user systemd private socket exists
func userSystemdSocketReady() bool {
	dir := userRuntimeDir()
	return pathExistsSafe(filepath.Join(dir, "bus")) || pathExistsSafe(filepath.Join(dir, "systemd", "private"))
}

// XDG_RUNTIME_DIR and DBUS_SESSION_BUS_ADDRESS values a service command needs to reach this user's manager that the process env lacks
func userSystemdOverrides() [][2]string {
	var out [][2]string
	dir := userRuntimeDir()
	if os.Getenv("XDG_RUNTIME_DIR") != dir && runtimeDirIsOurs(dir) {
		out = append(out, [2]string{"XDG_RUNTIME_DIR", dir})
	}
	if _, ok := os.LookupEnv("DBUS_SESSION_BUS_ADDRESS"); !ok {
		if bus := pypath.PathJoin(dir, "bus"); pathExistsSafe(bus) {
			out = append(out, [2]string{"DBUS_SESSION_BUS_ADDRESS", "unix:path=" + bus})
		}
	}
	return out
}

// Process env with the user manager values over it, the env a service command child is spawned with
func UserSystemdEnv() []string {
	env := os.Environ()
	for _, kv := range userSystemdOverrides() {
		env = append(env, kv[0]+"="+kv[1])
	}
	return env
}

// Systemctl argv prefix of a scope, run with UserSystemdEnv so the user scope reaches this user's manager
func SystemctlCmd(system bool) []string {
	if system {
		return []string{"systemctl"}
	}
	return []string{"systemctl", "--user"}
}

// Runs systemctl in a scope, SystemctlUnavailableError when the binary is missing
func RunSystemctl(ctx context.Context, args []string, system bool, timeout time.Duration) (CommandResult, error) {
	res, err := RunServiceCommand(ctx, timeout, append(SystemctlCmd(system), args...)...)
	if errors.Is(err, ErrCommandNotFound) {
		return res, SystemctlUnavailableError{}
	}
	return res, err
}

// Scope label of a systemd scope
func ServiceScopeLabel(system bool) string {
	if system {
		return "system"
	}
	return "user"
}

// True when systemctl reports the scope running, degraded, starting or initializing
func systemdOperational(ctx context.Context, system bool) bool {
	res, err := RunSystemctl(ctx, []string{"is-system-running"}, system, 5*time.Second)
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(res.Stdout)) {
	case "running", "degraded", "starting", "initializing":
		return true
	}
	return false
}

// WSL with systemd enabled in wsl.conf has a working system manager
func wslSystemdOperational(ctx context.Context) bool { return systemdOperational(ctx, true) }

// True when this host can manage the gateway as a systemd service
func SupportsSystemdServices(ctx context.Context) bool {
	if !IsLinux() || hostIsTermux() {
		return false
	}
	if _, err := serviceLookPath("systemctl"); err != nil {
		return false
	}
	if hostIsWSL() {
		return wslSystemdOperational(ctx)
	}
	if hostIsContainer() {
		return systemdOperational(ctx, false) || systemdOperational(ctx, true)
	}
	return true
}
