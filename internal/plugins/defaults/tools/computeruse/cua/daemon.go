package cua

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	cuconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/computeruse/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/proc"
	"github.com/nickheyer/icebath/pkg/strs"
	"gopkg.in/yaml.v3"
)

// Bundle id of CuaDriver.app, and the teams that sign its official releases
const bundleID = "com.trycua.driver"

var teams = []string{"4YEC26S9KF", "YCK386LBJ7"}

// CuaDriver.app holding the binary, empty when the binary is in no such bundle
func appPath(binary string) string {
	resolved, err := filepath.EvalSymlinks(binary)
	if err != nil {
		resolved = binary
	}
	head, _, found := strings.Cut(resolved, ".app/Contents/MacOS/")
	if !found {
		return ""
	}
	executable := filepath.Join(head+".app", "Contents", "MacOS", "cua-driver")
	if st, err := os.Stat(executable); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
		return head + ".app"
	}
	return ""
}

// Fails unless the bundle is CuaDriver.app signed by its publisher, or unsigned where the profile allows a local build
func (d *Driver) verifyApp(ctx context.Context, cfg *config.Config, app string) error {
	codesign, err := exec.LookPath("codesign")
	if err != nil {
		return errors.New("Checking the CuaDriver app before it starts needs codesign, which was not found.")
	}
	out, err := run(ctx, []string{codesign, "-dv", app}, 15*time.Second, nil, nil)
	if err != nil {
		return fmt.Errorf("Could not check the signature of the CuaDriver app: %v.", err)
	}
	if out.code != 0 {
		return fmt.Errorf("The CuaDriver app at %s is not signed, so icebath does not start it. codesign said: %s", app, strings.TrimSpace(out.stderr))
	}
	fields := map[string]string{}
	lines := strings.Split(out.stderr, "\n")
	for _, line := range slices.Backward(lines) {
		if k, v, ok := strings.Cut(line, "="); ok {
			fields[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	identifier, team := fields["Identifier"], fields["TeamIdentifier"]
	if identifier != bundleID {
		return fmt.Errorf("The CuaDriver app at %s has the ID “%s” instead of “%s”, so icebath does not start it.", app, identifier, bundleID)
	}
	if slices.Contains(teams, team) || (team == "" || team == "not set") && cfg.GetBool(cuconfig.AllowUnsignedDriver) {
		return nil
	}
	return fmt.Errorf("The CuaDriver app at %s is signed by the team “%s” instead of its makers, 4YEC26S9KF or YCK386LBJ7, so icebath does not "+
		"start it. Only for a driver you built yourself, turn on Allow unsigned driver of computer use.", app, team)
}

// Whether a capability manifest is version 3 or newer, which bounds any permission mode
func modeIndependent(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		slog.Debug("could not read capability manifest "+path, "err", err)
		return false
	}
	var manifest map[string]any
	if err := yaml.Unmarshal([]byte(strings.TrimPrefix(string(raw), "\ufeff")), &manifest); err != nil {
		slog.Debug("could not read capability manifest "+path, "err", err)
		return false
	}
	version, ok := manifest["version"].(int)
	return ok && version >= 3
}

// A daemon of the driver of this session's own, run in the unrestricted or bounded mode the shared one isn't in
type daemon struct {
	driver   *Driver
	config   *config.Config
	mode     string
	manifest string
	// The manifest bounds this mode
	applies bool
	command string
	args    []string
	proc    *terminal.HostProc
	owns    bool
	running bool
	tail    *tail
	socket  string
}

func newDaemon(d *Driver, cfg *config.Config, mode, manifest string) (*daemon, error) {
	if mode != cuconfig.Unrestricted && mode != cuconfig.Bounded {
		return nil, errors.New("embedded permission override supports unrestricted or bounded only")
	}
	manifest = strings.TrimSpace(manifest)
	if manifest == "" && mode == cuconfig.Bounded {
		return nil, errors.New("bounded permission mode requires tools.computer_use.capability_manifest")
	}
	if manifest != "" {
		abs, err := filepath.Abs(fsx.ExpandHome(manifest))
		if err != nil {
			return nil, err
		}
		manifest = abs
		if st, err := os.Stat(manifest); err != nil || !st.Mode().IsRegular() {
			return nil, fmt.Errorf("capability manifest not found: %s", manifest)
		}
	}
	dm := &daemon{driver: d, config: cfg, mode: mode, manifest: manifest, args: []string{"mcp"}, tail: &tail{label: "embedded cua-driver: "}}
	dm.applies = manifest != "" && (mode == cuconfig.Bounded || modeIndependent(manifest))
	if manifest != "" && !dm.applies {
		slog.Warn(fmt.Sprintf("computer_use.capability_manifest is a legacy (v1/v2) manifest, which cua-driver only accepts in bounded mode — it "+
			"will NOT bound this %s session. Migrate the manifest to version 3 to keep a ceiling on approval-bypassed runs.", mode))
	}
	if token := strs.Random(6); windows {
		dm.socket = `\\.\pipe\icebath-cua-` + token
	} else {
		dm.socket = filepath.Join(os.TempDir(), "hc-"+token+".sock")
	}
	return dm, nil
}

// Driver environment with the permission mode, approvals bypassed in the unrestricted one
func (dm *daemon) env() map[string]string {
	env := dm.driver.Env(dm.config)
	env["CUA_DRIVER_PERMISSION_MODE"] = dm.mode
	if dm.mode == cuconfig.Unrestricted {
		env["CUA_DRIVER_DANGEROUSLY_BYPASS_APPROVALS"] = "1"
	}
	return env
}

// Arguments of the daemon's serve verb with the manifest and the overlay setting
func (dm *daemon) serveArgs(ctx context.Context) []string {
	args := []string{"serve", "--embedded", "--socket", dm.socket, "--no-permissions-gate", "--permission-mode", dm.mode}
	if dm.mode == cuconfig.Unrestricted {
		args = append(args, "--dangerously-bypass-approvals")
	}
	if dm.applies {
		args = append(args, "--capability-manifest", dm.manifest, "--approve-capability-manifest")
	}
	return dm.driver.overlaid(ctx, dm.config, args, dm.command)
}

// Command line starting the daemon, through open on macOS so the privacy grants stay with the signed bundle
func (dm *daemon) spawnArgv(ctx context.Context) ([]string, error) {
	serve := dm.serveArgs(ctx)
	if !darwin {
		return append([]string{dm.command}, serve...), nil
	}
	app := appPath(dm.command)
	if app == "" {
		return nil, errors.New("CuaDriver.app is required for private computer-use sessions on macOS. Run `icebath computer-use install` to restore it.")
	}
	if err := dm.driver.verifyApp(ctx, dm.config, app); err != nil {
		return nil, err
	}
	return append([]string{"/usr/bin/open", "-n", "-g", "-a", app, "--args"}, serve...), nil
}

// Starts the daemon and waits for its socket to answer, fifteen seconds at most
func (dm *daemon) start(ctx context.Context) error {
	if dm.running {
		return nil
	}
	binary, err := dm.driver.Command("")
	if err != nil {
		return err
	}
	if binary == "" {
		return errors.New(InstallHint)
	}
	dm.command, dm.args = dm.driver.launch(ctx, dm.config, binary)
	env := dm.env()
	argv, err := dm.spawnArgv(ctx)
	if err != nil {
		return err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env, cmd.Stderr, cmd.WaitDelay = terminal.Environ(env), dm.tail, time.Second
	if !darwin {
		terminal.NewSession(cmd)
	}
	proc.HideWindow(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	dm.proc, dm.owns = terminal.Watch(cmd), true
	if !darwin {
		dm.proc.Own(dm.driver.host.Supervisor)
	}
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if code, exited := dm.proc.Poll(); exited && (!darwin || code != 0) {
			return dm.failure("embedded cua-driver exited during startup", "no diagnostic output")
		}
		if probe, err := run(ctx, []string{dm.command, "status", "--socket", dm.socket}, 2*time.Second, env, nil); err == nil && probe.code == 0 {
			dm.running = true
			return nil
		}
	}
	if err := dm.stop(); err != nil {
		return err
	}
	return dm.failure("embedded cua-driver startup timed out", "daemon did not become ready")
}

func (dm *daemon) failure(what, fallback string) error {
	return fmt.Errorf("%s: %s", what, cmp.Or(strings.Join(dm.tail.last(20), "; "), fallback))
}

// MCP proxy of the driver bound to the daemon's socket
func (dm *daemon) proxy() (string, []string, error) {
	if !dm.running {
		return "", nil, errors.New("embedded cua-driver daemon is not running")
	}
	return dm.command, append(slices.Clone(dm.args), "--embedded", "--socket", dm.socket), nil
}

// Stops the daemon, waits out or kills its process group and removes its socket
func (dm *daemon) stop() error {
	p, owns := dm.proc, dm.owns
	dm.proc, dm.owns, dm.running = nil, false, false
	if owns {
		run(context.Background(), []string{dm.command, "stop", "--socket", dm.socket}, 3*time.Second, dm.env(), nil)
	}
	if p != nil {
		if _, err := p.Wait(5 * time.Second); err != nil {
			p.KillTree()
		}
		p.Release()
	}
	if !windows {
		os.Remove(dm.socket)
	}
	return nil
}
