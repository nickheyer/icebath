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
	"github.com/nickheyer/icebath/pkg/internal/pystr"
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

// Per user runtime dir from XDG_RUNTIME_DIR or the run user path
func userRuntimeDir() string {
	if v := os.Getenv("XDG_RUNTIME_DIR"); v != "" {
		return v
	}
	return fmt.Sprintf("/run/user/%d", serviceGetuid())
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

// Points XDG_RUNTIME_DIR and DBUS_SESSION_BUS_ADDRESS at this user so systemctl user calls reach its manager
func ensureUserSystemdEnv() {
	own := fmt.Sprintf("/run/user/%d", serviceGetuid())
	xdg := os.Getenv("XDG_RUNTIME_DIR")
	if (xdg == "" || !runtimeDirIsOurs(xdg)) && runtimeDirIsOurs(own) {
		os.Setenv("XDG_RUNTIME_DIR", own)
	}
	if _, ok := os.LookupEnv("DBUS_SESSION_BUS_ADDRESS"); !ok {
		base, set := os.LookupEnv("XDG_RUNTIME_DIR")
		if !set {
			base = own
		}
		bus := pypath.PathJoin(base, "bus")
		if pathExistsSafe(bus) {
			os.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+bus)
		}
	}
}

// Systemctl argv prefix of a scope, preparing the user bus environment for the user scope
func SystemctlCmd(system bool) []string {
	if system {
		return []string{"systemctl"}
	}
	ensureUserSystemdEnv()
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
	switch pystr.Lower(pystr.Strip(res.Stdout)) {
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
