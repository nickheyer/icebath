// cua-driver, the program computer use drives the desktop through: where it is, whether it keeps the contract icebath needs, its install
// and the sessions of it a backend holds
package cua

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/botdesktop"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/packages"
	cuconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/computeruse/config"
	cupackages "github.com/nickheyer/icebath/internal/plugins/defaults/tools/computeruse/packages"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/proc"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	darwin  = runtime.GOOS == "darwin"
	windows = runtime.GOOS == "windows"
	linux   = runtime.GOOS == "linux"
)

// Variable opting the driver into Wayland directly
const waylandVar = "CUA_DRIVER_RS_ENABLE_WAYLAND"

// Platforms the driver runs on
func Supported() bool { return darwin || windows || linux }

// The driver of this machine, as the profile icebath runs as picks it: the driver is one per machine
type Driver struct {
	config   *config.Config
	host     *terminal.Terminal
	packages *packages.Packages
	// Each profile's own screen, which the driver acts on while it runs, from inside the terminal backend's sandbox when it lives there
	Desktop *botdesktop.Desktop

	mu sync.Mutex
	// Contract of the binary last checked, by its path, change time and size, for thirty seconds
	checked struct {
		fingerprint string
		at          time.Time
		contract    Contract
	}
	// Whether the driver last asked takes --no-overlay
	overlay struct {
		command string
		known   bool
		takes   bool
	}
}

func New(cfg *config.Config, host *terminal.Terminal, pm *packages.Packages, desktop *botdesktop.Desktop) *Driver {
	return &Driver{config: cfg, host: host, packages: pm, Desktop: desktop}
}

// Program the profile names in place of the managed driver, empty for none
func (d *Driver) Override() string {
	return strings.TrimSpace(d.config.GetString(cuconfig.DriverCommand))
}

// Binary of the driver: the program given or the profile's override, found on PATH when it is a bare name, else the managed one; empty
// when there is none, never installing
func (d *Driver) Command(given string) (string, error) {
	configured := cmp.Or(strings.TrimSpace(given), d.Override())
	if configured != "" {
		expanded := terminal.ExpandUser(configured)
		resolved, err := exec.LookPath(expanded)
		switch {
		case err != nil:
			return "", nil
		case strings.ContainsAny(expanded, separators()):
			return expanded, nil
		}
		return resolved, nil
	}
	installed, err := d.packages.Installed(cupackages.CuaDriver, false)
	if err != nil || installed == nil {
		return "", err
	}
	return installed.Binary, nil
}

func separators() string {
	if windows {
		return `\/`
	}
	return "/"
}

// Whether a driver is there to run, the override or the managed one
func (d *Driver) Installed() (bool, error) {
	binary, err := d.Command("")
	return binary != "", err
}

// Guidance for a missing driver
const InstallHint = "cua-driver is not installed. Install the pinned driver with:\n  icebath computer-use install\n" +
	"Or install it with the package manager: icebath pm install cua-driver"

// Variables of the driver's children: this machine's child environment for the profile with the display of its Bot Desktop while it runs,
// the driver's telemetry off unless the profile allows it, no update check of its own, and Wayland directly when asked for on a Wayland
// session
func (d *Driver) Env(cfg *config.Config) map[string]string {
	env := d.Desktop.Env(context.Background(), cfg, d.host.ChildEnv(cfg, telemetry(cfg)))
	if linux && env["WAYLAND_DISPLAY"] != "" && cfg.GetBool(cuconfig.NativeWayland) {
		env[waylandVar] = "1"
	}
	return env
}

// Variables keeping the driver's telemetry off unless the profile allows it, and its own update check off
func telemetry(cfg *config.Config) map[string]string {
	vars := map[string]string{cupackages.UpdateCheck: "false"}
	if cfg.GetBool(cuconfig.Telemetry) {
		maps.Copy(vars, map[string]string{"CUA_DRIVER_RS_TELEMETRY_ENABLED": "true", "CUA_TELEMETRY_ENABLED": "true", "CUA_TELEMETRY": "", "DO_NOT_TRACK": ""})
	} else {
		maps.Copy(vars, cupackages.TelemetryOff)
	}
	return vars
}

// Command of this machine that runs the driver's MCP server on the profile's screen inside the terminal backend's sandbox, the screen
// started first; nil for a screen on this machine
func (d *Driver) Sandboxed(ctx context.Context, cfg *config.Config) ([]string, error) {
	return d.Desktop.Command(ctx, cfg, []string{"cua-driver", "mcp", "--no-overlay"}, telemetry(cfg), "host cua-driver", true)
}

// Whether the driver acts on Linux: on this machine, or always inside the terminal backend's sandbox
func (d *Driver) Linux(cfg *config.Config) bool {
	return linux || d.Desktop.InSandbox(cfg)
}

// Display a driver started now acts on, which a kept backend must still match
func (d *Driver) Display(cfg *config.Config) string {
	return d.Env(cfg)["DISPLAY"]
}

// How a short run of a program ended, its output as text with every line ending a newline
type completed struct {
	code           int
	stdout, stderr string
}

// A program that could not be started at all
type spawnError struct{ err error }

func (e *spawnError) Error() string { return e.err.Error() }
func (e *spawnError) Unwrap() error { return e.err }

// A program killed at its deadline
type timeoutError struct {
	argv    []string
	timeout time.Duration
}

func (e *timeoutError) Error() string {
	return fmt.Sprintf("Command '%s' timed out after %g seconds", strings.Join(e.argv, " "), e.timeout.Seconds())
}

func textOf(b []byte) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.ToValidUTF8(string(b), "�"), "\r\n", "\n"), "\r", "\n")
}

// Runs a short probe with its output captured, killed at the deadline, the negated signal number for one a signal ended
func run(ctx context.Context, argv []string, timeout time.Duration, env map[string]string, stdin io.Reader) (*completed, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = time.Second
	if env != nil {
		cmd.Env = terminal.Environ(env)
	}
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	proc.HideWindow(cmd)
	if err := cmd.Start(); err != nil {
		return nil, &spawnError{err}
	}
	err := cmd.Wait()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, &timeoutError{argv: argv, timeout: timeout}
	}
	code := 0
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		code = proc.ExitCode(exit.ProcessState)
	} else if err != nil {
		return nil, &spawnError{err}
	}
	return &completed{code: code, stdout: textOf(stdout.Bytes()), stderr: textOf(stderr.Bytes())}, nil
}

// Runs one verb of the driver with the profile's driver environment
func (d *Driver) run(ctx context.Context, cfg *config.Config, binary string, timeout time.Duration, args ...string) (*completed, error) {
	return run(ctx, append([]string{binary}, args...), timeout, d.Env(cfg), nil)
}

// Object a verb printed as JSON, nil when it failed, printed nothing or no object
func (d *Driver) object(ctx context.Context, cfg *config.Config, binary string, timeout time.Duration, args ...string) map[string]any {
	out, err := d.run(ctx, cfg, binary, timeout, args...)
	if err != nil || out.code != 0 {
		return nil
	}
	return object(out.stdout)
}

func object(text string) map[string]any {
	var obj map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(text)), &obj) != nil {
		return nil
	}
	return obj
}

// What a driver must be to work with icebath: 0.20 or newer, with these flags on these verbs
var (
	contractMin  = [3]int{0, 20, 0}
	contractArgs = []struct {
		verb  string
		flags []string
	}{
		{"mcp", []string{"--socket", "--grant"}},
		{"serve", []string{"--socket", "--permission-mode", "--capability-manifest", "--approve-capability-manifest", "--embedded"}},
		{"stop", []string{"--socket"}},
	}
	semver = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)(?:[-+].*)?$`)
)

// Whether the driver keeps the contract, and why not
type Contract struct {
	Ready   bool
	Binary  string
	Version string
	Reason  string
}

// Arguments of a manifest's MCP launch when every one is text
func launchArgs(invocation any) ([]string, bool) {
	inv, ok := invocation.(map[string]any)
	if !ok {
		return nil, false
	}
	list, ok := inv["args"].([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		s, ok := a.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// Why a manifest breaks the contract, empty when it keeps it
func manifestReason(manifest map[string]any) string {
	if manifest == nil {
		return "driver manifest is missing or invalid"
	}
	version, _ := manifest["binary_version"].(string)
	m := semver.FindStringSubmatch(strings.TrimSpace(version))
	if m == nil {
		return "driver manifest does not report a semantic version"
	}
	for i := range 3 {
		n, _ := strconv.Atoi(m[i+1])
		if n != contractMin[i] {
			if n < contractMin[i] {
				return "Icebath computer use requires cua-driver 0.20.0 or newer"
			}
			break
		}
	}
	if args, ok := launchArgs(manifest["mcp_invocation"]); !ok || len(args) == 0 {
		return "driver manifest does not provide an MCP launch command"
	}
	advertised := map[string]map[string]bool{}
	subcommands, _ := manifest["subcommands"].([]any)
	for _, raw := range subcommands {
		sub, _ := raw.(map[string]any)
		name, ok := sub["name"].(string)
		if !ok {
			continue
		}
		flags := map[string]bool{}
		args, _ := sub["args"].([]any)
		for _, a := range args {
			if arg, ok := a.(map[string]any); ok {
				if n, ok := arg["name"].(string); ok {
					flags[n] = true
				}
			}
		}
		advertised[name] = flags
	}
	var missing []string
	for _, need := range contractArgs {
		for _, flag := range slices.Sorted(slices.Values(need.flags)) {
			if !advertised[need.verb][flag] {
				missing = append(missing, need.verb+" "+flag)
			}
		}
	}
	if len(missing) > 0 {
		return "driver manifest is missing: " + strings.Join(missing, ", ")
	}
	return ""
}

// Reads the driver's manifest and checks it against the contract
func (d *Driver) check(ctx context.Context, cfg *config.Config, binary string) Contract {
	c := Contract{Binary: binary, Reason: "cua-driver is not installed"}
	if binary == "" {
		return c
	}
	timeout := 5 * time.Second
	if windows {
		timeout = 15 * time.Second
	}
	out, err := d.run(ctx, cfg, binary, timeout, "manifest")
	switch {
	case err != nil:
		c.Reason = "manifest check failed: " + err.Error()
		return c
	case out.code != 0:
		lines := strings.Split(strings.TrimSpace(cmp.Or(out.stderr, out.stdout, "manifest command failed")), "\n")
		c.Reason = strs.Head(lines[len(lines)-1], 200)
		return c
	}
	manifest := object(out.stdout)
	c.Reason = manifestReason(manifest)
	if version, ok := manifest["binary_version"].(string); ok {
		c.Version = strings.TrimSpace(version)
	}
	c.Ready = c.Reason == ""
	return c
}

// Contract of the binary, the selected one when empty, kept thirty seconds while the binary stays the same file
func (d *Driver) Contract(ctx context.Context, cfg *config.Config, binary string) (Contract, error) {
	if binary == "" {
		var err error
		if binary, err = d.Command(""); err != nil {
			return Contract{}, err
		}
	}
	st, err := os.Stat(binary)
	if binary == "" || err != nil {
		return d.check(ctx, cfg, binary), nil
	}
	fingerprint := fmt.Sprintf("%s\x00%d\x00%d", binary, st.ModTime().UnixNano(), st.Size())
	d.mu.Lock()
	if d.checked.fingerprint == fingerprint && time.Since(d.checked.at) < 30*time.Second {
		c := d.checked.contract
		d.mu.Unlock()
		return c, nil
	}
	d.mu.Unlock()
	c := d.check(ctx, cfg, binary)
	d.mu.Lock()
	d.checked.fingerprint, d.checked.at, d.checked.contract = fingerprint, time.Now(), c
	d.mu.Unlock()
	return c, nil
}

// Forgets the contract last checked, before an install reads the binary again
func (d *Driver) forget() {
	d.mu.Lock()
	d.checked.fingerprint = ""
	d.mu.Unlock()
}

// Lines an install or a check prints: notes, warnings and successes, painted when the reader is a terminal
type Printer struct {
	Out   io.Writer
	Color bool
}

func (p Printer) paint(text, code string) string {
	if !p.Color {
		return text
	}
	return code + text + "\033[0m"
}

func (p Printer) Info(text string)    { fmt.Fprintln(p.Out, p.paint("  "+text, "\033[2m")) }
func (p Printer) Success(text string) { fmt.Fprintln(p.Out, p.paint("✓ "+text, "\033[32m")) }
func (p Printer) Warning(text string) { fmt.Fprintln(p.Out, p.paint("⚠ "+text, "\033[33m")) }

// Prints the warning and its hints, answering false as a failed step does
func (p Printer) fail(message string, hints ...string) bool {
	p.Warning(message)
	for _, hint := range hints {
		p.Info(hint)
	}
	return false
}

// Last three lines of what a run printed on stderr, else stdout, each cut at 200 characters
func (p Printer) tail(out *completed) {
	lines := strings.Split(strings.TrimSpace(cmp.Or(out.stderr, out.stdout)), "\n")
	for _, line := range lines[max(0, len(lines)-3):] {
		if line != "" {
			p.Info("      " + strs.Head(line, 200))
		}
	}
}

// Readies the pinned driver through the package manager, or checks the override, then what its platform needs: the logon task on Windows,
// the signed app registered with Launch Services on macOS; whether it ended ready
func (d *Driver) Install(ctx context.Context, cfg *config.Config, p Printer, progress bool) (bool, error) {
	override := d.Override()
	binary, err := d.Command("")
	if err != nil {
		return false, err
	}
	fresh := binary == ""
	if override != "" {
		if binary == "" {
			return p.fail("    "+cuconfig.DriverCommandEnv+" does not resolve to an executable: "+override,
				"    Fix or unset the override before running computer-use install."), nil
		}
	} else {
		if progress {
			p.Info("    Preparing the pinned cua-driver with Icebath PM...")
		}
		if _, err := d.packages.Ensure(ctx, cfg, cupackages.CuaDriver, packages.Ensuring{Explicit: true}); err != nil {
			return p.fail("    cua-driver preparation failed: " + err.Error()), nil
		}
		if binary, err = d.Command(""); err != nil {
			return false, err
		}
	}
	if binary == "" {
		return p.fail("    PM did not select a usable cua-driver executable."), nil
	}
	d.forget()
	c, err := d.Contract(ctx, cfg, binary)
	if err != nil {
		return false, err
	}
	if !c.Ready {
		hint := "    Run: icebath computer-use doctor"
		if override != "" {
			hint = "    Update the binary selected by " + cuconfig.DriverCommandEnv + ", or unset the override."
		}
		return p.fail("    cua-driver runtime contract is unusable: "+cmp.Or(c.Reason, "unknown error")+".", hint), nil
	}
	if windows && !d.repairAutostart(ctx, cfg, p, binary, progress) {
		return p.fail("    cua-driver is compatible, but Windows autostart setup failed."), nil
	}
	if darwin {
		app := appPath(binary)
		if app == "" {
			return p.fail("    macOS computer use requires the signed CuaDriver.app, not a bare binary.",
				"    The PM cua-driver package must include its signed macOS app bundle."), nil
		}
		if err := d.verifyApp(ctx, cfg, app); err != nil {
			return p.fail("    cua-driver macOS app registration failed: " + err.Error()), nil
		}
		out, err := run(ctx, []string{lsregister, "-f", app}, 15*time.Second, nil, nil)
		if err != nil {
			return p.fail("    cua-driver macOS app registration failed: " + err.Error()), nil
		}
		if out.code != 0 {
			p.tail(out)
			return p.fail("    cua-driver macOS app registration failed."), nil
		}
	}
	if progress {
		p.Success("    cua-driver ready: " + cmp.Or(c.Version, "unknown version") + ".")
		platformNotes(p, fresh)
	}
	return true, nil
}

const lsregister = "/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister"

// What a ready install says of its platform
func platformNotes(p Printer, fresh bool) {
	switch {
	case windows:
		p.Info("    cua-driver may spawn a UIAccess worker (cua-driver-uia.exe);")
		p.Info("    Windows/SmartScreen may prompt the first time it runs.")
	case linux:
		p.Warning("    Linux support is alpha.")
	default:
		if fresh {
			p.Info("    IMPORTANT — grant macOS permissions now:")
		} else {
			p.Info("    Grant macOS permissions if not done yet:")
		}
		p.Info("      System Settings > Privacy & Security > Accessibility")
		p.Info("      System Settings > Privacy & Security > Screen Recording")
	}
}

// Whether the driver's cursor overlay is hidden: the profile's choice, else on macOS, and on Linux without a Wayland session
func noOverlay(cfg *config.Config) bool {
	switch strings.ToLower(strings.TrimSpace(cfg.GetString(cuconfig.NoOverlay))) {
	case cuconfig.OverlayOff, "yes", "on", "1":
		return true
	case cuconfig.OverlayOn, "no", "off", "0":
		return false
	}
	if !linux {
		return darwin
	}
	return wsl() || os.Getenv("DISPLAY") == "" || os.Getenv("XDG_SESSION_TYPE") != "wayland" && os.Getenv("WAYLAND_DISPLAY") == ""
}

// Whether this Linux runs under Windows
func wsl() bool {
	if !linux {
		return false
	}
	if os.Getenv("WSL_DISTRO_NAME") != "" || os.Getenv("WSL_INTEROP") != "" {
		return true
	}
	version, err := os.ReadFile("/proc/version")
	return err == nil && strings.Contains(strings.ToLower(string(version)), "microsoft")
}

// Whether the driver's help names --no-overlay, asked once for the driver last asked about
func (d *Driver) takesNoOverlay(ctx context.Context, cfg *config.Config, binary string) bool {
	d.mu.Lock()
	if d.overlay.known && d.overlay.command == binary {
		takes := d.overlay.takes
		d.mu.Unlock()
		return takes
	}
	d.mu.Unlock()
	takes := false
	if out, err := d.run(ctx, cfg, binary, 3*time.Second, "--help"); err == nil {
		takes = strings.Contains(out.stdout+out.stderr, "--no-overlay")
	}
	d.mu.Lock()
	d.overlay.command, d.overlay.known, d.overlay.takes = binary, true, takes
	d.mu.Unlock()
	return takes
}

// Arguments with --no-overlay after them when the profile hides the overlay and the driver takes the flag
func (d *Driver) overlaid(ctx context.Context, cfg *config.Config, args []string, binary string) []string {
	out := slices.Clone(args)
	if noOverlay(cfg) && d.takesNoOverlay(ctx, cfg, binary) {
		out = append(out, "--no-overlay")
	}
	return out
}

var windowsAbsolute = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

// Path of a Windows program under WSL's drive mounts, any other path as it is
func wslPath(path string) string {
	if !windowsAbsolute.MatchString(path) || !wsl() {
		return path
	}
	parts := []string{"/mnt", strings.ToLower(path[:1])}
	for _, p := range strings.FieldsFunc(path[2:], func(r rune) bool { return r == '\\' || r == '/' }) {
		if p != "." {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "/")
}

// Command and arguments starting the driver's stdio MCP server as its manifest says, the mcp verb of the binary when it says nothing usable
func (d *Driver) launch(ctx context.Context, cfg *config.Config, binary string) (string, []string) {
	manifest := d.object(ctx, cfg, binary, 6*time.Second, "manifest")
	args, ok := launchArgs(manifest["mcp_invocation"])
	command := ""
	if ok {
		inv, _ := manifest["mcp_invocation"].(map[string]any)
		if c, isText := inv["command"].(string); isText {
			command = wslPath(c)
		}
	} else {
		args = []string{"mcp"}
	}
	if command == "" || !strings.ContainsAny(command, separators()) {
		command = binary
	}
	return command, d.overlaid(ctx, cfg, args, command)
}
