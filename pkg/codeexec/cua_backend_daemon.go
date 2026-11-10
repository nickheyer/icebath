package codeexec

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Teams that sign official CuaDriver.app releases, exact matches only
var cuaDriverTeamIDs = []string{"4YEC26S9KF", "YCK386LBJ7"}

// CuaDriver.app bundle carrying the resolved driver binary, empty when the binary is not inside one
func resolveCuaDriverAppPath(driverCmd string) string {
	resolved, err := pypath.Realpath(driverCmd, pypath.Lax)
	if err != nil {
		resolved = driverCmd
	}
	head, _, found := strings.Cut(resolved, ".app/Contents/MacOS/")
	if !found {
		return ""
	}
	executable := filepath.Join(head+".app", "Contents", "MacOS", "cua-driver")
	if info, err := os.Stat(executable); err == nil && info.Mode().IsRegular() && isExecutable(executable) {
		return head + ".app"
	}
	return ""
}

// Codesign lookup, swapped by tests that fake the binary
var lookCodesign = func() (string, error) { return exec.LookPath("codesign") }

// Fails closed unless the app path is the genuinely signed CuaDriver.app
func validateCuaDriverAppSignature(env *tools.Env, appPath string) error {
	codesign, err := lookCodesign()
	if err != nil || codesign == "" {
		return errors.New("codesign is required to verify CuaDriver.app before launching it.")
	}
	proc, err := runQuiet([]string{codesign, "-dv", appPath}, 15, runOpts{})
	if err != nil {
		return fmt.Errorf("could not verify CuaDriver.app signature: %v", err)
	}
	if proc.Returncode != 0 {
		return fmt.Errorf("CuaDriver.app at %s is not code-signed; refusing to launch it (%s)", appPath, pystr.Strip(proc.Stderr))
	}
	fields := map[string]string{}
	lines := pystr.Splitlines(proc.Stderr, false)
	for i := len(lines) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(lines[i], "="); ok {
			fields[pystr.Strip(k)] = pystr.Strip(v)
		}
	}
	identifier, team := fields["Identifier"], fields["TeamIdentifier"]
	if identifier != cuaDriverBundleID {
		return fmt.Errorf("CuaDriver.app at %s has identifier %s, expected %s; refusing to launch it.",
			appPath, pyrepr.Repr(identifier), pyrepr.Repr(cuaDriverBundleID))
	}
	for _, t := range cuaDriverTeamIDs {
		if team == t {
			return nil
		}
	}
	if (team == "" || team == "not set") && computerUseCfg(env)["allow_unsigned_driver"] == true {
		return nil
	}
	return fmt.Errorf("CuaDriver.app at %s is signed by team %s, expected one of ('4YEC26S9KF', 'YCK386LBJ7'); "+
		"refusing to launch it. (Set computer_use.allow_unsigned_driver: true in config.yaml only for "+
		"local unsigned driver builds.)", appPath, pyrepr.Repr(team))
}

// Private daemon launch argv, wrapped in open on macOS so TCC stays attached to the signed bundle
func embeddedDaemonSpawnCommand(env *tools.Env, driverCmd string, serveArgs []string, platform, appPath string) ([]string, error) {
	if platform != "darwin" {
		return append([]string{driverCmd}, serveArgs...), nil
	}
	resolvedApp := appPath
	if resolvedApp == "" {
		resolvedApp = resolveCuaDriverAppPath(driverCmd)
	}
	if resolvedApp == "" {
		return nil, errors.New("CuaDriver.app is required for private computer-use sessions on macOS. Run `icebath computer-use install` to restore it.")
	}
	if err := validateCuaDriverAppSignature(env, resolvedApp); err != nil {
		return nil, err
	}
	return append([]string{"/usr/bin/open", "-n", "-g", "-a", resolvedApp, "--args"}, serveArgs...), nil
}

// cuaDaemonProcess is the Popen face of a spawned daemon
type cuaDaemonProcess interface {
	PID() int
	Poll() (int, bool)
	Wait(timeout time.Duration) bool
	Terminate()
	Kill()
}

// Waits five seconds for a graceful exit, then terminates for two, then kills
func waitOrKill(p cuaDaemonProcess) {
	if p.Wait(5 * time.Second) {
		return
	}
	p.Terminate()
	if p.Wait(2 * time.Second) {
		return
	}
	p.Kill()
	p.Wait(2 * time.Second)
}

// execDaemonProcess is a started exec.Cmd whose exit is observed by one waiter goroutine
type execDaemonProcess struct {
	cmd    *exec.Cmd
	exited chan struct{}
	code   int
}

func (p *execDaemonProcess) PID() int { return p.cmd.Process.Pid }

func (p *execDaemonProcess) Poll() (int, bool) {
	select {
	case <-p.exited:
		return p.code, true
	default:
		return 0, false
	}
}

func (p *execDaemonProcess) Wait(timeout time.Duration) bool {
	select {
	case <-p.exited:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (p *execDaemonProcess) Terminate() { terminateProcess(p.cmd.Process) }

func (p *execDaemonProcess) Kill() { _ = p.cmd.Process.Kill() }

// stderrTail keeps the last twenty stripped stderr lines of the daemon
type stderrTail struct {
	mu      sync.Mutex
	lines   []string
	partial []byte
}

// Splits written bytes into universal newline lines and keeps the non empty stripped ones
func (t *stderrTail) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.partial = append(t.partial, b...)
	for {
		i := strings.IndexAny(string(t.partial), "\r\n")
		if i < 0 {
			break
		}
		line := textModeDecode(t.partial[:i])
		next := i + 1
		if t.partial[i] == '\r' && next < len(t.partial) && t.partial[next] == '\n' {
			next++
		}
		t.partial = t.partial[next:]
		t.add(line)
	}
	return len(b), nil
}

func (t *stderrTail) add(line string) {
	if text := pystr.Strip(line); text != "" {
		t.lines = append(t.lines, text)
		if len(t.lines) > 20 {
			t.lines = t.lines[len(t.lines)-20:]
		}
		slog.Debug("embedded cua-driver: " + text)
	}
}

func (t *stderrTail) joined() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, "; ")
}

// Starts the daemon argv, owned by this process off macOS, swapped by tests
var spawnCuaDaemon = func(ctx context.Context, argv []string, env map[string]string, platform string, tail *stderrTail) (cuaDaemonProcess, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = envList(env)
	cmd.Stderr = tail
	cmd.WaitDelay = time.Second
	var err error
	if platform == "darwin" {
		err = cmd.Start()
	} else {
		err = tools.SpawnOwned(ctx, cmd, "cua-driver-embedded")
	}
	if err != nil {
		return nil, err
	}
	p := &execDaemonProcess{cmd: cmd, exited: make(chan struct{})}
	go func() {
		werr := cmd.Wait()
		var exitErr *exec.ExitError
		if errors.As(werr, &exitErr) {
			p.code = exitErr.ExitCode()
			if p.code < 0 {
				p.code = -signalNumber(exitErr)
			}
		}
		close(p.exited)
	}()
	return p, nil
}

// embeddedCuaDaemon is the private daemon of an unrestricted or bounded permission mode
type embeddedCuaDaemon struct {
	env                *tools.Env
	permissionMode     string
	capabilityManifest string
	manifestApplies    bool
	driverCmd          string
	command            string
	mcpArgs            []string
	process            cuaDaemonProcess
	ownsRuntime        bool
	running            bool
	tail               *stderrTail
	socketPath         string
	startTimeout       time.Duration
}

// Random twelve hex digits of a uuid4
func cuaToken() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Validates the mode and manifest and fixes the private socket path
func newEmbeddedCuaDaemon(env *tools.Env, driverCmd, permissionMode, capabilityManifest string) (*embeddedCuaDaemon, error) {
	if permissionMode != "unrestricted" && permissionMode != "bounded" {
		return nil, errors.New("embedded permission override supports unrestricted or bounded only")
	}
	manifest := pystr.Strip(capabilityManifest)
	if manifest == "" && permissionMode == "bounded" {
		return nil, errors.New("bounded permission mode requires computer_use.capability_manifest")
	}
	if manifest != "" {
		abs, err := pypath.Abspath(pypath.Expanduser(manifest))
		if err != nil {
			return nil, err
		}
		manifest = abs
		if info, err := os.Stat(manifest); err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("capability manifest not found: %s", manifest)
		}
	}
	d := &embeddedCuaDaemon{env: env, permissionMode: permissionMode, capabilityManifest: manifest, driverCmd: driverCmd,
		command: driverCmd, mcpArgs: append([]string(nil), cuaDriverArgs...), tail: &stderrTail{}, startTimeout: 15 * time.Second}
	d.manifestApplies = manifest != "" && (permissionMode == "bounded" || manifestIsModeIndependent(manifest))
	if manifest != "" && !d.manifestApplies {
		slog.Warn(fmt.Sprintf("computer_use.capability_manifest is a legacy (v1/v2) manifest, which cua-driver only accepts in "+
			"bounded mode — it will NOT bound this %s session. Migrate the manifest to version 3 to keep a "+
			"ceiling on approval-bypassed runs.", permissionMode))
	}
	token := cuaToken()
	if cuPlatform == "win32" {
		d.socketPath = `\\.\pipe\icebath-cua-` + token
	} else {
		d.socketPath = filepath.Join(os.TempDir(), "hc-"+token+".sock")
	}
	return d, nil
}

// Driver child env carrying the permission mode and the approval bypass of unrestricted
func (d *embeddedCuaDaemon) childEnv() map[string]string {
	env := cuaDriverChildEnv(d.env, nil)
	env["CUA_DRIVER_PERMISSION_MODE"] = d.permissionMode
	if d.permissionMode == "unrestricted" {
		env["CUA_DRIVER_DANGEROUSLY_BYPASS_APPROVALS"] = "1"
	}
	return env
}

func (d *embeddedCuaDaemon) sanitizedEnv() (map[string]string, error) {
	return tools.SanitizeSubprocessEnv(d.env, d.childEnv(), nil)
}

// serve argv of the private daemon with the manifest and the overlay policy
func (d *embeddedCuaDaemon) serveArgs() []string {
	args := []string{"serve", "--embedded", "--socket", d.socketPath, "--no-permissions-gate", "--permission-mode", d.permissionMode}
	if d.permissionMode == "unrestricted" {
		args = append(args, "--dangerously-bypass-approvals")
	}
	if d.manifestApplies {
		args = append(args, "--capability-manifest", d.capabilityManifest, "--approve-capability-manifest")
	}
	return mcpArgsWithOverlayFlag(d.env, args, d.command)
}

// Launches the private daemon and waits until its socket answers status
func (d *embeddedCuaDaemon) start(ctx context.Context) error {
	if d.running {
		return nil
	}
	driverCmd := d.driverCmd
	if driverCmd == "" {
		resolved, err := resolveCuaDriverCmd(nil)
		if err != nil {
			return err
		}
		driverCmd = resolved
	}
	if driverCmd == "" {
		return errors.New(cuaDriverInstallHint())
	}
	d.command, d.mcpArgs = resolveMCPInvocation(d.env, driverCmd, 6.0)
	env, err := d.sanitizedEnv()
	if err != nil {
		return err
	}
	command, err := embeddedDaemonSpawnCommand(d.env, d.command, d.serveArgs(), cuPlatform, "")
	if err != nil {
		return err
	}
	if d.process, err = spawnCuaDaemon(tools.WithEnvHome(ctx, d.env), command, env, cuPlatform, d.tail); err != nil {
		return err
	}
	d.ownsRuntime = true
	deadline := time.Now().Add(d.startTimeout)
	for time.Now().Before(deadline) {
		if code, exited := d.process.Poll(); exited && (cuPlatform != "darwin" || code != 0) {
			return d.startupFailure("embedded cua-driver exited during startup", "no diagnostic output")
		}
		if d.socketReady(env) {
			d.running = true
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := d.stop(); err != nil {
		return err
	}
	return d.startupFailure("embedded cua-driver startup timed out", "daemon did not become ready")
}

func (d *embeddedCuaDaemon) startupFailure(what, fallback string) error {
	tail := d.tail.joined()
	if tail == "" {
		tail = fallback
	}
	return fmt.Errorf("%s: %s", what, tail)
}

// cua-driver status --socket exits zero once the private daemon accepts connections
func (d *embeddedCuaDaemon) socketReady(env map[string]string) bool {
	probe, err := runQuiet([]string{d.command, "status", "--socket", d.socketPath}, 2.0, runOpts{Env: env})
	return err == nil && probe.Returncode == 0
}

// MCP proxy invocation bound to the private socket
func (d *embeddedCuaDaemon) proxyInvocation() (string, []string, error) {
	if !d.running {
		return "", nil, errors.New("embedded cua-driver daemon is not running")
	}
	return d.command, append(append([]string(nil), d.mcpArgs...), "--embedded", "--socket", d.socketPath), nil
}

// Stops the private daemon, reaps its process group and removes the socket file
func (d *embeddedCuaDaemon) stop() error {
	process := d.process
	d.process = nil
	ownsRuntime := d.ownsRuntime
	d.ownsRuntime, d.running = false, false
	if ownsRuntime {
		env, err := d.sanitizedEnv()
		if err != nil {
			return err
		}
		_, _ = runQuiet([]string{d.command, "stop", "--socket", d.socketPath}, 3.0, runOpts{Env: env})
	}
	if process != nil {
		waitOrKill(process)
		tools.ReleaseProcessGroupOnceEmpty(process.PID(), nil, time.Second)
	}
	if cuPlatform != "win32" {
		if _, err := os.Stat(d.socketPath); err == nil {
			_ = os.Remove(d.socketPath)
		}
	}
	return nil
}
