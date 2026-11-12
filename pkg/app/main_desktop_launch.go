package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate"
)

// How a launched child's standard stream is wired
type guiStream int

const (
	streamInherit guiStream = iota
	streamDevnull
	streamCapture
)

// One subprocess.run or Popen call of the desktop launcher
type guiRun struct {
	Argv          []string
	Dir           string
	Env           map[string]string
	Stdin         guiStream
	Stdout        guiStream
	Stderr        guiStream
	StdoutFile    *os.File
	StderrFile    *os.File
	ExtraFiles    []*os.File
	Attr          *syscall.SysProcAttr
	Timeout       time.Duration
	Interruptible bool
}

// Finished launcher call, interrupted when Ctrl-C arrived while it ran
type guiResult struct {
	Returncode  int
	Stdout      []byte
	Stderr      []byte
	Interrupted bool
}

// Exit status the launcher leaves with
type guiExit struct{ code int }

func (e *guiExit) Error() string { return fmt.Sprintf("exit %d", e.code) }

// Host facts and steps the desktop launcher reads
type guiSteps struct {
	platform            func() string
	which               func(name string) string
	homeDir             func() string
	projectRoot         func() string
	stdinIsTTY          func() (bool, error)
	run                 func(spec guiRun) (*guiResult, error)
	popen               func(spec guiRun) (int, error)
	capture             func(argv []string, input []byte) (*pysubprocess.Completed, error)
	setConfigValue      func(ctx context.Context, key, value string) error
	loadConfig          func(ctx context.Context) (config.Tree, error)
	buildNeeded         func(ctx context.Context, desktopDir, projectRoot string, sourceMode bool) bool
	packagedExecutable  func(desktopDir string) (string, bool)
	relaunchableFixup   func(desktopDir string) bool
	launchTarget        func(ctx context.Context, desktopDir, packagedExecutable string) string
	sourceBuildEnv      func(ctx context.Context, env map[string]string, explicit bool) (map[string]string, error)
	prepare             func(ctx context.Context, projectRoot string, workspaces []string, env map[string]string, explicit bool) error
	buildPrepared       func(ctx context.Context, desktopDir string, sourceMode bool, npm string, env map[string]string) (string, error)
	newBuildLock        func(projectRoot string) guiBuildLock
	sandboxFixup        func(w io.Writer, packagedExecutable string) bool
	usernsAvailable     func() bool
	passwordStore       func() string
	registerEntry       func(w io.Writer, deferred bool) *deferredDesktopEntryInstall
	launchEnv           func(ctx context.Context, ns map[string]any) (map[string]string, []string, error)
	launchInstalledMac  func(w io.Writer) (bool, error)
	setupTCCIdentity    func(ctx context.Context, w io.Writer, identity string) (bool, error)
	installAppArmor     func(ctx context.Context, w io.Writer, packagedExecutable string) (bool, error)
	setupLogging        func(ctx context.Context)
	needsNoSandbox      func() bool
	helperIsRegularFile func(packagedExecutable string) bool
	needsDisableSetuid  func(packagedExecutable string) bool
}

// Steps the desktop launcher runs, assigned at package init
var gui guiSteps

func init() {
	gui = guiSteps{
		platform:           pythonPlatform,
		which:              func(name string) string { p, _ := pyshutil.Which(name, nil); return p },
		homeDir:            pythonHome,
		projectRoot:        pm.RepoRoot,
		stdinIsTTY:         stdinIsTerminal,
		run:                runGuiCommand,
		popen:              popenGuiCommand,
		capture:            captureGuiCommand,
		setConfigValue:     setGuiConfigValue,
		loadConfig:         func(ctx context.Context) (config.Tree, error) { return config.LoadContext(ctx, home.Dir(ctx), nil) },
		buildNeeded:        selfupdate.DesktopBuildNeeded,
		packagedExecutable: selfupdate.DesktopPackagedExecutable,
		relaunchableFixup:  selfupdate.DesktopMacosRelaunchableFixup,
		launchTarget:       selfupdate.InstalledDesktopLaunchTarget,
		sourceBuildEnv: func(ctx context.Context, env map[string]string, explicit bool) (map[string]string, error) {
			return selfupdate.SourceBuildEnv(ctx, env, explicit, true)
		},
		prepare:             selfupdate.PrepareSourceDependencies,
		buildPrepared:       buildPreparedDesktop,
		newBuildLock:        func(projectRoot string) guiBuildLock { return selfupdate.NewDesktopBuildLock(projectRoot) },
		sandboxFixup:        desktopLinuxSandboxFixup,
		usernsAvailable:     desktopLinuxUsernsSandboxAvailable,
		passwordStore:       detectLinuxPasswordStore,
		registerEntry:       registerLinuxDesktopEntry,
		launchEnv:           desktopLaunchEnv,
		launchInstalledMac:  launchInstalledMacosDesktopApp,
		setupTCCIdentity:    desktopMacosSetupTCCIdentity,
		installAppArmor:     apparmorInstallProfile,
		setupLogging:        func(ctx context.Context) { _, _ = logging.SetupLogging(ctx, logging.Options{Mode: "gui"}) },
		needsNoSandbox:      desktopLinuxNeedsNoSandbox,
		helperIsRegularFile: desktopLinuxSandboxHelperIsRegularFile,
		needsDisableSetuid:  desktopLinuxNeedsDisableSetuidSandbox,
	}
}

// Desktop build lock the launcher holds across the mutable preflight
type guiBuildLock interface {
	Acquire(ctx context.Context, wait bool) (bool, error)
	Release(ctx context.Context)
}

// Python sys.platform of this host
func pythonPlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	default:
		return runtime.GOOS
	}
}

// Platform the desktop launcher acts for
func guiPlatform() string { return gui.platform() }

// Home directory the way pathlib Path.home reads it
func pythonHome() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

// Whether standard input is a terminal, an error when it is closed
func stdinIsTerminal() (bool, error) {
	if os.Stdin == nil {
		return false, nil
	}
	if _, err := os.Stdin.Stat(); err != nil {
		return false, err
	}
	return isatty.IsTerminal(os.Stdin.Fd()), nil
}

// Sets a dotted config key the way config set does for the active home
func setGuiConfigValue(ctx context.Context, key, value string) error {
	h := home.Dir(ctx)
	return config.SetConfigValue(ctx, h, key, value, false, commands.ConfigCommandSources(h))
}

// Builds prepared desktop sources with this launcher's output and no icon override
func buildPreparedDesktop(ctx context.Context, desktopDir string, sourceMode bool, npm string, env map[string]string) (string, error) {
	return selfupdate.BuildPreparedDesktop(ctx, desktopDir, sourceMode, npm, env, "")
}

// Exec command of a launcher call with its stdio wired
func guiCommand(ctx context.Context, spec guiRun) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
	cmd := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	if spec.Env != nil {
		cmd.Env = pm.EnvList(spec.Env)
	}
	if spec.Stdin == streamInherit {
		cmd.Stdin = os.Stdin
	}
	var out, errOut bytes.Buffer
	switch {
	case spec.StdoutFile != nil:
		cmd.Stdout = spec.StdoutFile
	case spec.Stdout == streamInherit:
		cmd.Stdout = os.Stdout
	case spec.Stdout == streamCapture:
		cmd.Stdout = &out
	}
	switch {
	case spec.StderrFile != nil:
		cmd.Stderr = spec.StderrFile
	case spec.Stderr == streamInherit:
		cmd.Stderr = os.Stderr
	case spec.Stderr == streamCapture:
		cmd.Stderr = &errOut
	}
	cmd.ExtraFiles = spec.ExtraFiles
	cmd.SysProcAttr = spec.Attr
	return cmd, &out, &errOut
}

// Runs a launcher call like subprocess.run, a timeout raising TimeoutExpired and Ctrl-C ending an interruptible one
func runGuiCommand(spec guiRun) (*guiResult, error) {
	ctx, cancel := context.Background(), context.CancelFunc(func() {})
	if spec.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
	}
	defer cancel()
	cmd, out, errOut := guiCommand(ctx, spec)
	var interrupts chan os.Signal
	if spec.Interruptible {
		interrupts = make(chan os.Signal, 1)
		signal.Notify(interrupts, os.Interrupt)
		defer signal.Stop(interrupts)
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-interrupts:
		select {
		case <-done:
		case <-time.After(250 * time.Millisecond):
			_ = cmd.Process.Kill()
			<-done
		}
		return &guiResult{Interrupted: true}, nil
	}
	res := &guiResult{Stdout: out.Bytes(), Stderr: errOut.Bytes()}
	if spec.Timeout > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return res, &pysubprocess.TimeoutExpired{Cmd: spec.Argv, Timeout: int(spec.Timeout / time.Second), Stdout: res.Stdout, Stderr: res.Stderr}
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		res.Returncode = pysubprocess.Returncode(exitErr)
		return res, nil
	}
	return res, waitErr
}

// Starts a launcher call like subprocess.Popen and returns its pid without waiting
func popenGuiCommand(spec guiRun) (int, error) {
	cmd, _, _ := guiCommand(context.Background(), spec)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

// Runs argv with both streams captured like subprocess.run capture_output
func captureGuiCommand(argv []string, input []byte) (*pysubprocess.Completed, error) {
	return pysubprocess.RunWith(context.Background(), argv, pysubprocess.Options{Input: input})
}

// Exit code a SystemExit style error carries
func guiSystemExit(err error) (int, bool) {
	var ge *guiExit
	if errors.As(err, &ge) {
		return ge.code, true
	}
	var ce *config.ConfigExit
	if errors.As(err, &ce) {
		return ce.Code, true
	}
	var pe *pm.SystemExit
	if errors.As(err, &pe) {
		return pe.Code, true
	}
	return 0, false
}

// Python str of an OS level error, the plain message for any other error
func guiOSErrorText(err error) string {
	if pypath.IsOSError(err) {
		return pypath.OSErrorText(err)
	}
	return err.Error()
}

// True when Electron should run without its sandbox, an explicit opt out or a restricted unprivileged userns host
func desktopLinuxNeedsNoSandbox() bool {
	if os.Getenv("ELECTRON_DISABLE_SANDBOX") == "1" {
		return true
	}
	if guiPlatform() != "linux" {
		return false
	}
	if lde.geteuid() == 0 {
		return false
	}
	raw, err := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns")
	if err != nil {
		return false
	}
	text, err := pystr.DecodeUTF8(raw)
	return err == nil && pystr.Strip(text) == "1"
}

// True when the unprivileged userns sandbox works, probed with unshare and failing closed
func desktopLinuxUsernsSandboxAvailable() bool {
	if guiPlatform() != "linux" {
		return false
	}
	unshare := gui.which("unshare")
	if unshare == "" {
		return false
	}
	res, err := gui.run(guiRun{Argv: []string{unshare, "--user", "--map-root-user", "true"}, Stdout: streamDevnull, Stderr: streamDevnull, Timeout: 5 * time.Second})
	return err == nil && res.Returncode == 0
}

// Path of the chrome-sandbox helper beside the executable and its lstat, nil when it cannot be read
func sandboxHelperLstat(packagedExecutable string) (string, os.FileInfo) {
	sandbox := filepath.Join(filepath.Dir(packagedExecutable), "chrome-sandbox")
	st, err := os.Lstat(sandbox)
	if err != nil {
		return sandbox, nil
	}
	return sandbox, st
}

// Permission bits of a stat including setuid, setgid and sticky, the S_IMODE of its mode
func statIMode(st os.FileInfo) uint32 {
	m := st.Mode()
	mode := uint32(m.Perm())
	if m&os.ModeSetuid != 0 {
		mode |= 0o4000
	}
	if m&os.ModeSetgid != 0 {
		mode |= 0o2000
	}
	if m&os.ModeSticky != 0 {
		mode |= 0o1000
	}
	return mode
}

// True for a root owned 4755 helper
func sandboxHelperIsSetuidRoot(st os.FileInfo) bool {
	return fileOwnerUID(st) == 0 && statIMode(st) == 0o4755
}

// True when chrome-sandbox exists as a regular file
func desktopLinuxSandboxHelperIsRegularFile(packagedExecutable string) bool {
	if guiPlatform() != "linux" {
		return false
	}
	_, st := sandboxHelperLstat(packagedExecutable)
	return st != nil && st.Mode().IsRegular()
}

// Configures Electron's Linux setuid sandbox helper when the namespace sandbox cannot serve, false when it could not
func desktopLinuxSandboxFixup(w io.Writer, packagedExecutable string) bool {
	if guiPlatform() != "linux" {
		return true
	}
	sandbox, st := sandboxHelperLstat(packagedExecutable)
	if _, err := os.Stat(sandbox); err != nil {
		fmt.Fprintf(w, "✗ Icebath Desktop is missing Electron's Linux sandbox helper: %s\n", sandbox)
		return false
	}
	if st == nil {
		fmt.Fprintf(w, "✗ Cannot stat Electron's Linux sandbox helper: %s\n", sandbox)
		return false
	}
	if !st.Mode().IsRegular() {
		fmt.Fprintf(w, "✗ Electron's Linux sandbox helper is not a regular file: %s\n", sandbox)
		return false
	}
	if sandboxHelperIsSetuidRoot(st) {
		return true
	}
	verdict, known := chromiumUsernsSandboxVerdict(packagedExecutable)
	if (known && verdict) || (!known && gui.usernsAvailable()) {
		fmt.Fprintln(w, "✓ Using Chromium's user-namespace sandbox (setuid helper not needed).")
		return true
	}
	sudo := gui.which("sudo")
	if sudo == "" {
		fmt.Fprintln(w, "✗ Icebath Desktop requires sudo to configure Electron's Linux sandbox helper.")
		return false
	}
	fmt.Fprintln(w, "→ Configuring Electron Linux sandbox helper (sudo required)...")
	tty, err := gui.stdinIsTTY()
	nonInteractive := err != nil || !tty
	for _, command := range [][]string{{sudo, "chown", "root:root", sandbox}, {sudo, "chmod", "4755", sandbox}} {
		spec := guiRun{Argv: command}
		if nonInteractive {
			spec.Argv = append([]string{command[0], "-n"}, command[1:]...)
			spec.Stdin = streamDevnull
			spec.Timeout = 60 * time.Second
		}
		res, err := gui.run(spec)
		if err != nil || res.Returncode != 0 {
			fmt.Fprintf(w, "✗ Failed to configure Electron's Linux sandbox helper: %s\n", sandbox)
			return false
		}
	}
	return true
}

// True when a present non setuid chrome-sandbox would abort Chromium despite a working userns sandbox
func desktopLinuxNeedsDisableSetuidSandbox(packagedExecutable string) bool {
	if guiPlatform() != "linux" {
		return false
	}
	_, st := sandboxHelperLstat(packagedExecutable)
	return st != nil && st.Mode().IsRegular() && !sandboxHelperIsSetuidRoot(st)
}

var (
	linuxPasswordStores = []string{"gnome-libsecret", "kwallet", "kwallet5", "kwallet6", "basic"}
	gpuFlagWords        = map[string]string{"1": "1", "true": "1", "yes": "1", "on": "1", "0": "0", "false": "0", "no": "0", "off": "0"}
	a11yOffWords        = []string{"0", "false", "no", "off", "disabled"}
)

// Chromium password store backend for this Linux session from KDE, GNOME Keyring or a secrets D-Bus ping, empty when none
func detectLinuxPasswordStore() string {
	if kde := pystr.Strip(os.Getenv("KDE_SESSION_VERSION")); kde != "" {
		switch kde {
		case "6":
			return "kwallet6"
		case "5":
			return "kwallet5"
		}
		return "kwallet"
	}
	if os.Getenv("KDE_FULL_SESSION") != "" {
		return "kwallet"
	}
	if os.Getenv("GNOME_KEYRING_CONTROL") != "" {
		return "gnome-libsecret"
	}
	res, err := gui.run(guiRun{Argv: []string{"dbus-send", "--session", "--print-reply", "--reply-timeout=2000",
		"--dest=org.freedesktop.secrets", "/org/freedesktop/secrets", "org.freedesktop.DBus.Peer.Ping"},
		Stdout: streamCapture, Stderr: streamCapture, Timeout: 5 * time.Second})
	if err == nil && res.Returncode == 0 {
		return "gnome-libsecret"
	}
	return ""
}

// Desktop launch options from config, flags, gpu, password store, ozone hint and renderer accessibility
type desktopLaunchOptions struct {
	ElectronFlags         []string
	DisableGPU            string
	PasswordStore         string
	OzoneHint             string
	RendererAccessibility bool
}

// Reads the desktop launch options, unknown values and config errors yielding the auto defaults
func readDesktopLaunchOptions(ctx context.Context) (desktopLaunchOptions, error) {
	opts := desktopLaunchOptions{DisableGPU: "auto", PasswordStore: "auto", OzoneHint: "auto", RendererAccessibility: true}
	cfg, err := gui.loadConfig(ctx)
	if err != nil {
		return opts, nil
	}
	raw, _ := cfg["desktop"]
	if !pyrepr.Truthy(raw) {
		raw = map[string]any{}
	}
	desktopCfg, ok := raw.(map[string]any)
	if !ok {
		return opts, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(raw))
	}
	switch flags := desktopCfg["electron_flags"].(type) {
	case string:
		var split []string
		if runtime.GOOS == "windows" {
			split, err = pyshlex.SplitNonPOSIX(flags)
		} else {
			split, err = pyshlex.SplitPOSIX(flags, false)
		}
		if err != nil {
			return opts, err
		}
		opts.ElectronFlags = split
	case []any:
		for _, f := range flags {
			if s := pyrepr.Str(f); pystr.Strip(s) != "" {
				opts.ElectronFlags = append(opts.ElectronFlags, s)
			}
		}
	}
	choice := func(key string, allowed []string) string {
		v, present := desktopCfg[key]
		if !present {
			v = "auto"
		}
		low := ""
		if s, ok := v.(string); ok {
			low = pystr.Lower(pystr.Strip(s))
		}
		if slices.Contains(allowed, low) {
			return low
		}
		return "auto"
	}
	rawGPU, present := desktopCfg["disable_gpu"]
	if !present {
		rawGPU = "auto"
	}
	switch v := rawGPU.(type) {
	case bool:
		opts.DisableGPU = "0"
		if v {
			opts.DisableGPU = "1"
		}
	case string:
		if word, ok := gpuFlagWords[pystr.Lower(pystr.Strip(v))]; ok {
			opts.DisableGPU = word
		}
	}
	opts.PasswordStore = choice("password_store", linuxPasswordStores)
	opts.OzoneHint = choice("ozone_platform_hint", []string{"auto", "x11", "wayland"})
	rawA11y, present := desktopCfg["renderer_accessibility"]
	if !present {
		rawA11y = true
	}
	switch v := rawA11y.(type) {
	case bool:
		opts.RendererAccessibility = v
	case int:
		opts.RendererAccessibility = v != 0
	case int64:
		opts.RendererAccessibility = v != 0
	case uint64:
		opts.RendererAccessibility = v != 0
	case float64:
		opts.RendererAccessibility = v != 0
	case string:
		opts.RendererAccessibility = !slices.Contains(a11yOffWords, pystr.Lower(pystr.Strip(v)))
	}
	return opts, nil
}

// Registers the XDG launcher entry now, or returns a deferred install for an app grid launch, never failing a launch
func registerLinuxDesktopEntry(w io.Writer, deferred bool) *deferredDesktopEntryInstall {
	projectRoot := gui.projectRoot()
	if !desktopEntrySupported() {
		return nil
	}
	if deferred {
		install, err := newDeferredDesktopEntryInstall(w, projectRoot, nil, 2*time.Second)
		if err != nil {
			fmt.Fprintf(w, "⚠ Could not install the desktop launcher entry: %s\n", guiOSErrorText(err))
			return nil
		}
		install.start()
		return install
	}
	entry, err := lde.installDesktopEntry(projectRoot)
	if err != nil {
		fmt.Fprintf(w, "⚠ Could not install the desktop launcher entry: %s\n", err.Error())
		return nil
	}
	if entry != "" {
		fmt.Fprintf(w, "✓ Desktop launcher entry installed: %s\n", entry)
	}
	return nil
}

var (
	wslDXGDevice    = "/dev/dxg"
	wslD3D12Drivers = []string{
		"/usr/lib/x86_64-linux-gnu/dri/d3d12_dri.so",
		"/usr/lib/aarch64-linux-gnu/dri/d3d12_dri.so",
		"/usr/lib64/dri/d3d12_dri.so",
		"/usr/lib/dri/d3d12_dri.so",
	}
	mesaDriverOverrides = []string{"GALLIUM_DRIVER", "MESA_LOADER_DRIVER_OVERRIDE", "LIBGL_ALWAYS_SOFTWARE", "LIBGL_DRIVERS_PATH"}
)

// Selects Mesa's d3d12 driver under WSLg when the device and driver exist and no Mesa choice is set
func preferWSLD3D12(env map[string]string) {
	for _, key := range mesaDriverOverrides {
		if _, ok := env[key]; ok {
			return
		}
	}
	if !hostinfo.IsWSL() {
		return
	}
	if _, err := os.Stat(wslDXGDevice); err != nil {
		return
	}
	for _, driver := range wslD3D12Drivers {
		if st, err := os.Stat(driver); err == nil && st.Mode().IsRegular() {
			env["GALLIUM_DRIVER"] = "d3d12"
			return
		}
	}
}

const (
	// Longest TMPDIR that leaves room for Chromium's singleton socket path
	electronTmpdirMaxBytes = 107 - len("/scoped_dirXXXXXX/SingletonSocket")
	// Variable carrying the real TMPDIR when the socket dir moved
	desktopTmpdirEnv = "ICEBATH_DESKTOP_TMPDIR"
)

// Expanded and resolved form of a user supplied path
func expandResolve(p string) string {
	expanded := pypath.Expanduser(p)
	if resolved, err := pypath.Resolve(expanded); err == nil {
		return resolved
	}
	if abs, err := filepath.Abs(expanded); err == nil {
		return abs
	}
	return expanded
}

// Electron child environment and config supplied extra flags, an explicit variable winning over config
func desktopLaunchEnv(ctx context.Context, ns map[string]any) (map[string]string, []string, error) {
	env := pm.WithIcebathNodePath(nil)
	tmpdir := env["TMPDIR"]
	if guiPlatform() == "linux" && len(tmpdir) > electronTmpdirMaxBytes {
		safe, err := home.SocketSafeTmpdir()
		if err != nil {
			return nil, nil, err
		}
		env[desktopTmpdirEnv] = tmpdir
		env["TMPDIR"] = safe
	}
	preferWSLD3D12(env)
	if nsTruthy(ns, "fake_boot") {
		env["ICEBATH_DESKTOP_BOOT_FAKE"] = "1"
	}
	if nsTruthy(ns, "ignore_existing") {
		env["ICEBATH_DESKTOP_IGNORE_EXISTING"] = "1"
	}
	if root := nsText(ns, "icebath_root"); root != "" {
		env["ICEBATH_DESKTOP_ICEBATH_ROOT"] = expandResolve(root)
	}
	if cwd := nsText(ns, "cwd"); cwd != "" {
		env["ICEBATH_DESKTOP_CWD"] = expandResolve(cwd)
	} else {
		wd, err := os.Getwd()
		if err != nil {
			return nil, nil, err
		}
		env["ICEBATH_DESKTOP_CWD"] = wd
	}
	opts, err := readDesktopLaunchOptions(ctx)
	if err != nil {
		return nil, nil, err
	}
	if _, set := os.LookupEnv("ICEBATH_DESKTOP_DISABLE_GPU"); opts.DisableGPU != "auto" && !set {
		env["ICEBATH_DESKTOP_DISABLE_GPU"] = opts.DisableGPU
	}
	if _, set := os.LookupEnv("ELECTRON_OZONE_PLATFORM_HINT"); opts.OzoneHint != "auto" && !set {
		env["ELECTRON_OZONE_PLATFORM_HINT"] = opts.OzoneHint
	}
	if _, set := os.LookupEnv("ICEBATH_DESKTOP_RENDERER_ACCESSIBILITY"); !opts.RendererAccessibility && !set {
		env["ICEBATH_DESKTOP_RENDERER_ACCESSIBILITY"] = "0"
	}
	if _, set := os.LookupEnv("ICEBATH_DESKTOP_PASSWORD_STORE"); guiPlatform() == "linux" && !set {
		store := opts.PasswordStore
		if store == "auto" {
			store = gui.passwordStore()
		}
		if store != "" {
			env["ICEBATH_DESKTOP_PASSWORD_STORE"] = store
		}
	}
	return env, opts.ElectronFlags, nil
}

// Validates the prebuilt artifact skip build promised, an exit code with a hint when it is missing
func checkDesktopSkipBuild(w io.Writer, desktopDir, projectRoot string, sourceMode bool, packagedExecutable string) (int, bool) {
	if sourceMode {
		if !selfupdate.DesktopDistExists(desktopDir) {
			fmt.Fprintf(w, "✗ --skip-build --source was passed but no desktop dist found at: %s\n", filepath.Join(desktopDir, "dist"))
			fmt.Fprintln(w, "  Pre-build first:  cd apps/desktop && npm run build")
			fmt.Fprintln(w, "  Or drop --skip-build to install dependencies and build automatically.")
			return 1, true
		}
		if _, err := os.Stat(filepath.Join(selfupdate.ElectronDir(projectRoot), "package.json")); err != nil {
			fmt.Fprintln(w, "✗ --skip-build --source requires existing desktop workspace dependencies.")
			fmt.Fprintf(w, "  Install first:  cd %s && npm ci\n", projectRoot)
			fmt.Fprintln(w, "  Or drop --skip-build to install dependencies and build automatically.")
			return 1, true
		}
		fmt.Fprintf(w, "→ Skipping desktop source build (--skip-build --source); using dist at %s\n", filepath.Join(desktopDir, "dist"))
		return 0, false
	}
	if packagedExecutable == "" {
		fmt.Fprintf(w, "✗ --skip-build was passed but no packaged desktop app was found at: %s\n", filepath.Join(desktopDir, "release"))
		fmt.Fprintln(w, "  Pre-build first:  cd apps/desktop && npm run pack")
		fmt.Fprintln(w, "  Or drop --skip-build to package automatically.")
		return 1, true
	}
	desktopLaunchNotice(w, fmt.Sprintf("→ Skipping desktop package build (--skip-build); using %s", packagedExecutable), false)
	return 0, false
}

// Executable and sandbox flags after the Linux sandbox fixup, an exit when the sandbox cannot be configured
func packagedDesktopLaunchCommand(w io.Writer, packagedExecutable string) ([]string, bool) {
	launch := []string{packagedExecutable}
	if !gui.sandboxFixup(w, packagedExecutable) {
		if gui.needsNoSandbox() && gui.helperIsRegularFile(packagedExecutable) {
			printNoSandboxFallback(w, packagedExecutable)
			return append(launch, "--no-sandbox"), true
		}
		return nil, false
	}
	if gui.needsDisableSetuid(packagedExecutable) {
		launch = append(launch, "--disable-setuid-sandbox")
	}
	return launch, true
}

// Package manager owning a non editable install at the project root, empty for a checkout
func sitePackagesInstallKind(projectRoot string) string {
	_, parts := pypath.PathParts(projectRoot)
	if slices.Contains(parts, "site-packages") || slices.Contains(parts, "dist-packages") {
		if slices.Contains(parts, "Cellar") {
			return "homebrew"
		}
		return "pip"
	}
	return ""
}

// Executable of the separately installed macOS app bundle
var installedMacosAppExecutable = "/Applications/Icebath.app/Contents/MacOS/Icebath"

// Launches a separately installed Icebath.app detached, true only when the bundle exists and started
func launchInstalledMacosDesktopApp(w io.Writer) (bool, error) {
	if guiPlatform() != "darwin" {
		return false, nil
	}
	executable := installedMacosAppExecutable
	if st, err := os.Stat(executable); err != nil || !st.Mode().IsRegular() {
		return false, nil
	}
	pid, err := launchDetached([]string{executable}, nil, filepath.Dir(executable))
	if err != nil {
		return false, err
	}
	fmt.Fprintf(w, "→ Launched the installed Icebath Desktop app: %s (pid %d)\n", executable, pid)
	return true, nil
}

// Runs a host setup one shot flag and reports the exit it ends with, no exit when none was asked
func runDesktopOneShot(ctx context.Context, w io.Writer, ns map[string]any, desktopDir string, bundled bool) (int, bool, error) {
	if nsTruthy(ns, "setup_tcc_identity") {
		identity := nsText(ns, "identity")
		if identity == "" {
			identity = defaultTCCIdentity
		}
		ok, err := gui.setupTCCIdentity(ctx, w, identity)
		if err != nil {
			return 0, false, err
		}
		if ok {
			return 0, true, nil
		}
		return 1, true, nil
	}
	if nsTruthy(ns, "install_apparmor_profile") {
		exe, found := gui.packagedExecutable(desktopDir)
		if bundled {
			if layout, err := resolveBundleLayout(gui.projectRoot(), ""); err == nil {
				exe, found = layout.Launcher, layout.Launcher != ""
			} else if _, notBundled := err.(*notBundledApp); !notBundled {
				return 0, false, err
			}
		}
		if !found {
			fmt.Fprintf(w, "✗ No packaged Icebath Desktop found under %s; run `icebath desktop` first.\n", filepath.Join(desktopDir, "release"))
			return 1, true, nil
		}
		ok, err := gui.installAppArmor(ctx, w, exe)
		if err != nil {
			return 0, false, err
		}
		if ok {
			return 0, true, nil
		}
		return 1, true, nil
	}
	return 0, false, nil
}

// Python exception classes the build step of the desktop launch reports instead of raising
var guiCaughtBuildErrors = []string{
	"RuntimeError", "NotImplementedError", "RecursionError", "WebappBuildError", "ProductBuildError", "FeatureProbeError",
	"StaleLockRow", "NoCommittedEnvironment", "InstallError", "ResolutionConflict", "BuildFailure", "DownloadError",
	"SubprocessError", "CalledProcessError", "TimeoutExpired",
	"OSError", "FileNotFoundError", "PermissionError", "FileExistsError", "IsADirectoryError", "NotADirectoryError",
	"TimeoutError", "BlockingIOError", "ChildProcessError", "ConnectionError", "BrokenPipeError", "ConnectionAbortedError",
	"ConnectionRefusedError", "ConnectionResetError", "InterruptedError", "ProcessLookupError",
}

// Whether the build step reports an error as a failed build rather than raising it
func guiBuildErrorCaught(err error) bool {
	return pypath.IsOSError(err) || slices.Contains(guiCaughtBuildErrors, selfupdate.ExceptionName(err))
}

// Builds and launches the native Electron desktop app
func CmdGUI(ctx context.Context, ns map[string]any, explicitProfile string, w io.Writer) (int, error) {
	defer selfupdate.SetDesktopOut(w)()
	code, err := cmdGUI(ctx, ns, explicitProfile, w)
	if exit, ok := guiSystemExit(err); ok {
		return exit, nil
	}
	return code, err
}

// Desktop launch flow returning the exit code cmd_gui leaves with
func cmdGUI(ctx context.Context, ns map[string]any, explicitProfile string, w io.Writer) (int, error) {
	projectRoot := gui.projectRoot()
	desktopDir := filepath.Join(projectRoot, "apps", "desktop")
	bundled := selfupdate.IsBundledPayload(projectRoot)
	if _, err := os.Stat(filepath.Join(desktopDir, "package.json")); !bundled && err != nil {
		installKind := sitePackagesInstallKind(projectRoot)
		if installKind != "" {
			launched, err := gui.launchInstalledMac(w)
			if err != nil {
				return 0, err
			}
			if launched {
				return 0, nil
			}
		}
		fmt.Fprintf(w, "Desktop GUI source not found at: %s\n", desktopDir)
		if installKind == "homebrew" {
			fmt.Fprintln(w, "  This Icebath came from Homebrew, which does not ship the desktop app's\n"+
				"  source tree, so it cannot be built from this install.\n"+
				"  Install the desktop app from https://hermes-agent.nousresearch.com,\n"+
				"  or run `icebath desktop` from a source checkout.")
		}
		return 1, nil
	}
	gui.setupLogging(ctx)
	env, configElectronFlags, err := gui.launchEnv(ctx, ns)
	if err != nil {
		return 0, err
	}
	sourceMode := nsTruthy(ns, "source")
	skipBuild := nsTruthy(ns, "skip_build")
	forceBuild := nsTruthy(ns, "force_build")
	buildOnly := nsTruthy(ns, "build_only")
	if code, exit, err := runDesktopOneShot(ctx, w, ns, desktopDir, bundled); err != nil || exit {
		return code, err
	}
	if bundled {
		return launchBundledDesktop(w, ns, env, configElectronFlags, explicitProfile)
	}
	packagedExecutable, _ := gui.packagedExecutable(desktopDir)
	var buildLock guiBuildLock
	if !skipBuild {
		buildLock = gui.newBuildLock(projectRoot)
		acquired, err := buildLock.Acquire(ctx, false)
		if err != nil {
			if !pypath.IsOSError(err) && selfupdate.ExceptionName(err) != "OSError" {
				return 0, err
			}
			fmt.Fprintf(w, "✗ Could not create the desktop build lock: %s\n", guiOSErrorText(err))
			fmt.Fprintln(w, "  Refusing to run npm without serialization; check the checkout permissions and retry.")
			return 1, nil
		}
		if !acquired {
			fmt.Fprintln(w, "✗ An Icebath update or another desktop dependency install or build is already running.")
			fmt.Fprintln(w, "  Wait for it to finish, then retry.")
			return 2, nil
		}
	}
	releaseLock := func() {
		if buildLock != nil {
			buildLock.Release(ctx)
		}
	}
	needsBuild := !skipBuild && (forceBuild || gui.buildNeeded(ctx, desktopDir, projectRoot, sourceMode))
	buildErr := func() error {
		var buildEnv map[string]string
		npm := ""
		if needsBuild {
			var err error
			if buildEnv, err = gui.sourceBuildEnv(ctx, env, forceBuild || buildOnly); err != nil {
				return err
			}
			path, ok := buildEnv["PATH"]
			if !ok {
				return guiKeyError("PATH")
			}
			npm, _ = pyshutil.Which("npm", &path)
			env["PATH"] = path
		}
		if skipBuild {
			if code, exit := checkDesktopSkipBuild(w, desktopDir, projectRoot, sourceMode, packagedExecutable); exit {
				return &guiExit{code: code}
			}
		} else if needsBuild {
			if err := gui.prepare(ctx, projectRoot, []string{"ui-tui", "web", "apps/desktop"}, buildEnv, forceBuild || buildOnly); err != nil {
				return err
			}
			built, err := gui.buildPrepared(ctx, desktopDir, sourceMode, npm, buildEnv)
			if err != nil {
				return err
			}
			if !sourceMode && built != "" {
				packagedExecutable = built
			}
		} else {
			label := "packaged app"
			if sourceMode {
				label = "source build"
			}
			desktopLaunchNotice(w, fmt.Sprintf("✓ Desktop %s is up to date (content stamp matches)", label), sourceMode)
		}
		return nil
	}()
	if buildErr != nil {
		if _, ok := guiSystemExit(buildErr); ok || !guiBuildErrorCaught(buildErr) {
			return 0, buildErr
		}
		fmt.Fprintf(w, "✗ Desktop GUI build failed: %s\n", guiErrorText(buildErr))
		releaseLock()
		return 1, nil
	}
	deferredEntry := gui.registerEntry(w, launchedFromShell(nil) && !buildOnly)
	if buildOnly {
		if sourceMode {
			if !selfupdate.DesktopDistExists(desktopDir) {
				fmt.Fprintf(w, "✗ --build-only --source produced no dist at: %s\n", filepath.Join(desktopDir, "dist"))
				return 1, nil
			}
			fmt.Fprintf(w, "✓ Desktop source build ready at %s (not launching; --build-only)\n", filepath.Join(desktopDir, "dist"))
		} else if packagedExecutable == "" {
			fmt.Fprintf(w, "✗ --build-only produced no launchable app at: %s\n", filepath.Join(desktopDir, "release"))
			fmt.Fprintln(w, "  Expected an unpacked Electron app for the current OS.")
			return 1, nil
		} else {
			fmt.Fprintf(w, "✓ Desktop packaged app ready: %s (not launching; --build-only)\n", packagedExecutable)
		}
		releaseLock()
		return 0, nil
	}
	var launch []string
	if sourceMode {
		fmt.Fprintln(w, "→ Launching Icebath Desktop from source build...")
		executable, missing, err := preparedElectronRuntime(projectRoot)
		if err != nil {
			return 0, err
		}
		if missing != nil {
			fmt.Fprintf(w, "✗ Prepared Electron runtime is missing: %s\n", missing.Error())
			fmt.Fprintln(w, "  From the project root, run: icebath electron-runtime --source .")
			return 1, nil
		}
		launch = []string{executable, "."}
	} else {
		if packagedExecutable == "" {
			fmt.Fprintf(w, "✗ Desktop package build completed but no launchable app was found at: %s\n", filepath.Join(desktopDir, "release"))
			fmt.Fprintln(w, "  Expected an unpacked Electron app for the current OS.")
			return 1, nil
		}
		var ok bool
		if launch, ok = packagedDesktopLaunchCommand(w, gui.launchTarget(ctx, desktopDir, packagedExecutable)); !ok {
			return 1, nil
		}
		launch = append(launch, configElectronFlags...)
	}
	if nsTruthy(ns, "local") {
		launch = append(launch, "--local")
	}
	if nsTruthy(ns, "close_preview") {
		launch = append(launch, "--close-preview")
	}
	launch = append(launch, explicitProfileArgs(explicitProfile)...)
	if !sourceMode {
		desktopLaunchNotice(w, "→ Launching packaged Icebath Desktop: "+strings.Join(launch, " "), false)
	}
	releaseLock()
	var passFiles []*os.File
	if deferredEntry != nil {
		env = deferredEntry.childEnv(env)
		passFiles = deferredEntry.passFds()
	}
	if !sourceMode && guiPlatform() == "win32" {
		spec := guiRun{Argv: launch, Dir: desktopDir, Env: env, Stdin: streamDevnull, Stdout: streamDevnull, Stderr: streamDevnull,
			Attr: creationFlagsAttr(windowsDetachFlags())}
		if _, err := gui.popen(spec); err != nil {
			if spawnWinerror(err) != 5 {
				return 0, err
			}
			spec.Attr = creationFlagsAttr(windowsDetachFlagsWithoutBreakaway())
			if _, err := gui.popen(spec); err != nil {
				return 0, err
			}
		}
		if deferredEntry != nil {
			deferredEntry.finish()
		}
		desktopLaunchNotice(w, "✓ Icebath Desktop launched in a detached window; you can close this shell.", false)
		return 0, nil
	}
	streams, err := desktopConsoleOutput(sourceMode)
	if err != nil {
		return 0, err
	}
	res, err := gui.run(guiRun{Argv: launch, Dir: desktopDir, Env: env, StdoutFile: streams.Stdout, StderrFile: streams.Stderr,
		ExtraFiles: passFiles, Interruptible: true})
	streams.Close()
	if err != nil {
		return 0, err
	}
	if res.Interrupted {
		fmt.Fprintln(w, "\n✓ Icebath Desktop closed.")
		return 0, nil
	}
	if deferredEntry != nil {
		deferredEntry.finish()
	}
	return res.Returncode, nil
}

// Python str of an exception the build step reports
func guiErrorText(err error) string {
	var called *pysubprocess.CalledProcessError
	if errors.As(err, &called) {
		return called.Error()
	}
	return guiOSErrorText(err)
}

// Prepared Electron runtime the source launch runs, the OS error naming what is missing, or a decode failure
func preparedElectronRuntime(projectRoot string) (string, error, error) {
	electron := selfupdate.ElectronDir(projectRoot)
	pathTxt := filepath.Join(electron, "path.txt")
	raw, err := os.ReadFile(pathTxt)
	if err != nil {
		return "", errors.New(pypath.OSErrorText(err)), nil
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return "", nil, err
	}
	executable := pypath.PathJoin(electron, "dist", pystr.Strip(text))
	if st, err := os.Stat(executable); err != nil || !st.Mode().IsRegular() {
		return "", errors.New(executable), nil
	}
	return executable, nil, nil
}

// KeyError Python raises for a missing mapping key
type guiKeyErr struct{ key string }

func (e *guiKeyErr) Error() string      { return pyrepr.StrRepr(e.key) }
func (e *guiKeyErr) PyTypeName() string { return "KeyError" }

// KeyError for a missing key
func guiKeyError(key string) error { return &guiKeyErr{key: key} }

// Win32 error number of a failed spawn, minus one when it carries none
func spawnWinerror(err error) int {
	var errno syscall.Errno
	if guiPlatform() == "win32" && errors.As(err, &errno) {
		return int(errno)
	}
	return -1
}

// Profile flag for Electron when the profile was passed explicitly on the command line
func explicitProfileArgs(explicitProfile string) []string {
	if explicitProfile != "" {
		return []string{"--profile", explicitProfile}
	}
	return nil
}

// Starts the desktop app this CLI ships inside detached and returns the exit code
func launchBundledDesktop(w io.Writer, ns map[string]any, env map[string]string, electronFlags []string, explicitProfile string) (int, error) {
	var refused []string
	for _, pair := range [][2]string{{"--source", "source"}, {"--build-only", "build_only"}, {"--force-build", "force_build"}} {
		if nsTruthy(ns, pair[1]) {
			refused = append(refused, pair[0])
		}
	}
	if len(refused) > 0 {
		fmt.Fprintf(w, "✗ %s cannot apply to a bundled Icebath install.\n", strings.Join(refused, ", "))
		fmt.Fprintln(w, "  This app ships prebuilt and has no desktop source tree to build.")
		return 2, nil
	}
	layout, err := resolveBundleLayout(gui.projectRoot(), "")
	if err != nil {
		if nb, ok := err.(*notBundledApp); ok {
			fmt.Fprintf(w, "✗ This Icebath is stamped as a bundled desktop install, but %s.\n", nb.Error())
			fmt.Fprintln(w, "  The install is damaged — reinstall Icebath from the website.")
			return 1, nil
		}
		return 0, err
	}
	if layout.Launcher == "" {
		fmt.Fprintf(w, "✗ Found no Icebath Desktop launcher in %s.\n", layout.AppRoot)
		fmt.Fprintln(w, "  The install is damaged — reinstall Icebath from the website.")
		return 1, nil
	}
	launch, ok := packagedDesktopLaunchCommand(w, layout.Launcher)
	if !ok {
		return 1, nil
	}
	launch = append(launch, electronFlags...)
	launch = append(launch, explicitProfileArgs(explicitProfile)...)
	pid, err := launchDetached(launch, env, layout.AppRoot)
	if err != nil {
		return 0, err
	}
	fmt.Fprintf(w, "→ Launched Icebath Desktop: %s (pid %d)\n", strings.Join(launch, " "), pid)
	return 0, nil
}
