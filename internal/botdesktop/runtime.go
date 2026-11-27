package botdesktop

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	bdconfig "github.com/nickheyer/icebath/internal/botdesktop/config"
	"github.com/nickheyer/icebath/internal/browser"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/terminal/sandbox"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Band of X display numbers screens of this machine take
const (
	displayMin = 20
	displayMax = 89
)

// How long a start waits for the launcher to publish, on this machine and in a sandbox
const (
	DefaultWait = 15 * time.Second
	sandboxWait = 20 * time.Second
)

// A person's hold on an idle screen counts as abandoned after this many idle windows, ten minutes at the least
const (
	abandonedWindows = 4
	abandonedMin     = 10 * time.Minute
)

// Programs a screen on this machine needs
var Required = []string{"Xvnc", "xfwm4", "xfce4-panel", "xfdesktop", "xfsettingsd", "dbus-run-session", "xauth", "xdpyinfo", "setxkbmap", "xprop"}

// Packages each package manager installs a screen from
var Packages = map[string][]string{
	"apt": {"tigervnc-standalone-server", "xfce4-panel", "xfwm4", "xfdesktop4", "xfce4-settings", "xfce4-terminal", "dbus-x11", "x11-xserver-utils",
		"x11-utils", "x11-xkb-utils", "xauth", "fonts-dejavu-core"},
	"dnf": {"tigervnc-x11-server", "xfce4-panel", "xfwm4", "xfdesktop", "xfce4-settings", "xfce4-terminal", "dbus-daemon", "xsetroot", "xset",
		"xdpyinfo", "xprop", "xorg-x11-xauth", "setxkbmap", "dejavu-sans-fonts"},
	"pacman": {"tigervnc", "xfce4-panel", "xfwm4", "xfdesktop", "xfce4-settings", "xfce4-terminal", "dbus", "xorg-xsetroot", "xorg-xset",
		"xorg-xdpyinfo", "xorg-xprop", "xorg-xauth", "xorg-setxkbmap", "ttf-dejavu"},
}

// Folder of the profile's screen state
func StateDir(cfg *config.Config) string {
	return filepath.Join(cfg.ProfileDir(cfg.Profile()), "bot-desktop")
}

// Whether screens run here: Linux only
func Supported() bool { return runtime.GOOS == "linux" }

func which(name string) string {
	found, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return found
}

// Programs of a screen missing from PATH
func Missing() []string {
	var out []string
	for _, b := range Required {
		if which(b) == "" {
			out = append(out, b)
		}
	}
	return out
}

// Package manager of the machine, apt, dnf or pacman, empty for none
func Manager() string {
	for _, m := range []string{"apt-get", "dnf", "pacman"} {
		if which(m) != "" {
			return strings.TrimSuffix(m, "-get")
		}
	}
	return ""
}

// Install command as a person types it here, sudo first unless root; empty without a package manager
func InstallCommand() string {
	m := Manager()
	if m == "" {
		return ""
	}
	pkgs := strings.Join(Packages[m], " ")
	body := map[string]string{
		"apt":    "apt-get install -y --no-install-recommends " + pkgs,
		"dnf":    "dnf install -y " + pkgs,
		"pacman": "pacman -S --needed --noconfirm " + pkgs,
	}[m]
	if root() {
		return body
	}
	return "sudo " + body
}

// Whether the install could succeed here: a package manager, and root or sudo
func Installable() bool {
	return Manager() != "" && (root() || which("sudo") != "")
}

// What the profile's screen is: whether it can run and is installed, running and where, who holds it, and what keeps it from starting
type Status struct {
	Profile   string
	Supported bool
	Installed bool
	Missing   []string
	Running   bool
	// Launcher process, zero for none
	PID      int
	Display  string
	Socket   string
	Geometry string
	// Command that installs what is missing
	Install string
	// Chromium the screen runs
	Browser string
	// Why it cannot start now
	Blocker string
	// Free memory and its limit in MB, -1 when unknown
	Available, Limit int
	// gateway, or terminal:<backend>
	Placement string
	// Image change of the sandbox it waits on, nil for none
	Switch *terminal.Switch
	Lease  Lease
}

// A launcher process by its pid and start time
type launcher struct {
	pid  int
	born int64
}

// Launcher launcher.pid names, alive or not
func recorded(cfg *config.Config) (launcher, bool) {
	raw, _ := trimmed(filepath.Join(StateDir(cfg), "launcher.pid"))
	pidText, bornText, _ := strings.Cut(raw, " ")
	pid, err1 := strconv.Atoi(pidText)
	born, err2 := strconv.ParseInt(strings.TrimSpace(bornText), 10, 64)
	if err1 != nil || err2 != nil || pid <= 0 {
		return launcher{}, false
	}
	return launcher{pid, born}, true
}

// Live launcher launcher.pid names, still the process started then
func live(cfg *config.Config) (launcher, bool) {
	l, ok := recorded(cfg)
	if !ok || !terminal.Alive(l.pid) {
		return launcher{}, false
	}
	if born, ok := terminal.StartTime(l.pid); !ok || born != l.born {
		return launcher{}, false
	}
	return l, true
}

// Pid in the X lock of a display
func xLock(num int) (int, bool) {
	raw, ok := trimmed(fmt.Sprintf("/tmp/.X%d-lock", num))
	if !ok {
		return 0, false
	}
	pid, err := strconv.Atoi(raw)
	return pid, err == nil
}

// Whether the abstract X socket of the display is bound
func xBound(num int) bool {
	raw, ok := trimmed("/proc/net/unix")
	if !ok {
		return false
	}
	want := fmt.Sprintf("/tmp/.X11-unix/X%d", num)
	for _, line := range strings.Split(raw, "\n") {
		if f := strings.Fields(line); len(f) > 0 && strings.TrimLeft(f[len(f)-1], "@") == want {
			return true
		}
	}
	return false
}

// Whether a live X server owns the display through its lock or its bound socket
func displayTaken(num int) bool {
	pid, ok := xLock(num)
	return ok && terminal.Alive(pid) || xBound(num)
}

// Kills the X server a dead launcher left behind when it is provably the profile's, whether something was signalled
func reapOrphan(cfg *config.Config, dir string) bool {
	sock := filepath.Join(dir, "rfb.sock")
	ours := func(argv []string) bool {
		return len(argv) > 0 && strings.Contains(filepath.Base(argv[0]), "Xvnc") && slices.Contains(argv, sock)
	}
	display, _ := trimmed(filepath.Join(dir, "display"))
	pid, found := 0, false
	if num, err := strconv.Atoi(display); err == nil {
		pid, found = xLock(num)
	}
	if !found || !terminal.Alive(pid) {
		found = false
		for _, p := range terminal.Pids() {
			if ours(terminal.Cmdline(p)) {
				pid, found = p, true
				break
			}
		}
		if !found {
			return false
		}
	}
	dead, haveDead := recorded(cfg)
	group, err := pgid(pid)
	if err != nil {
		return false
	}
	ownGroup := haveDead && group == dead.pid
	if !ownGroup && !ours(terminal.Cmdline(pid)) {
		return false
	}
	slog.Warn(fmt.Sprintf("Bot Desktop launcher is gone but its X server (pid %d) survived on :%s; reaping", pid, cmp.Or(display, "?")))
	if !ownGroup {
		group = 0
	}
	stopGroup(group, pid, 2*time.Second)
	if display != "" {
		os.Remove(fmt.Sprintf("/tmp/.X%s-lock", display))
	}
	for _, name := range []string{"launcher.pid", "env", "rfb.sock"} {
		os.Remove(filepath.Join(dir, name))
	}
	return true
}

// SIGTERM to the group, or the lone process without one, SIGKILL to what is left after the grace
func stopGroup(group, pid int, grace time.Duration) {
	gone := func() bool {
		reap(pid)
		if group == 0 {
			return !terminal.Alive(pid)
		}
		return terminal.GroupGone(group)
	}
	send := func(force bool) {
		if group > 0 {
			terminal.SignalGroup(group, force)
			return
		}
		if p, err := os.FindProcess(pid); err == nil {
			if force {
				p.Kill()
			} else {
				p.Signal(sigterm)
			}
		}
	}
	send(false)
	for deadline := time.Now().Add(grace); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if gone() {
			return
		}
	}
	send(true)
	time.Sleep(100 * time.Millisecond)
	reap(pid)
}

// Lock taken across the machine while a display number is picked and claimed
func allocLock() string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "icebath-bot-desktop-alloc.lock")
}

// Display the profile had when no other server holds it, else the first free one in the band
func pickDisplay(cfg *config.Config) (int, error) {
	raw, _ := trimmed(filepath.Join(StateDir(cfg), "display"))
	if num, err := strconv.Atoi(raw); err == nil && !displayTaken(num) {
		return num, nil
	}
	for num := displayMin; num <= displayMax; num++ {
		if !displayTaken(num) {
			return num, nil
		}
	}
	return 0, errors.New("no free X display number in the Bot Desktop band")
}

// Variables of base with the running screen's DISPLAY, XAUTHORITY and bus merged in and WAYLAND_DISPLAY dropped, so a program started
// with them opens on the screen; base unchanged while it is down. Base is this process's environment when nil
func (d *Desktop) Env(ctx context.Context, cfg *config.Config, base map[string]string) map[string]string {
	if base == nil {
		base = terminal.ProcessEnv()
	}
	out := maps.Clone(base)
	published, err := d.Published(ctx, cfg)
	if err != nil {
		slog.Debug("Bot Desktop: published variables unreadable", "error", err)
	}
	if len(published) > 0 {
		d.Touch(cfg)
		maps.Copy(out, published)
		delete(out, "WAYLAND_DISPLAY")
	}
	return out
}

// Brings the screen up ahead of a tool that drives it: one in the sandbox always, one on this machine when auto_start allows; a failure only
// logged, for the tool to report
func (d *Desktop) Ready(ctx context.Context, cfg *config.Config) {
	if published, _ := d.Published(ctx, cfg); published["DISPLAY"] != "" {
		d.Touch(cfg)
		return
	}
	switch d.Resolve(cfg).Where {
	case Refused:
		return
	case Terminal:
		if _, err := d.Start(ctx, cfg, DefaultWait); err != nil {
			slog.Info("Bot Desktop sandbox start deferred to the tool: " + err.Error())
		}
		return
	}
	if !d.autoStart(cfg) {
		return
	}
	if _, err := d.Start(ctx, cfg, DefaultWait); err != nil {
		slog.Info("Bot Desktop auto-start skipped: " + err.Error())
	}
}

// Where the browser or driver about to start runs, gateway or terminal, a sandbox screen that is down started first; an error saying why
// for a refused placement
func (d *Desktop) Place(ctx context.Context, cfg *config.Config) (string, error) {
	where := d.Resolve(cfg)
	switch where.Where {
	case Refused:
		return "", errors.New(where.Reason)
	case Gateway:
		return Gateway, nil
	}
	running, err := d.sandboxRunning(ctx, cfg)
	if err != nil {
		return "", err
	}
	published := map[string]string{}
	if running {
		if published, err = d.Published(ctx, cfg); err != nil {
			return "", err
		}
	}
	if !running || published["DISPLAY"] == "" {
		if _, err := d.Start(ctx, cfg, DefaultWait); err != nil {
			return "", err
		}
	}
	return Terminal, nil
}

// A Linux machine with no display of its own, the packages there and bot_desktop.auto_start on
func (d *Desktop) autoStart(cfg *config.Config) bool {
	if !Supported() || os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "" || len(Missing()) > 0 {
		return false
	}
	return cfg.GetBool(bdconfig.AutoStart)
}

// Notes that the screen was just used, which keeps it from the idle stop
func (d *Desktop) Touch(cfg *config.Config) {
	path := filepath.Join(StateDir(cfg), "activity")
	if f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600); err == nil {
		f.Close()
		now := time.Now()
		os.Chtimes(path, now, now)
	}
}

// How long since the screen was last used, its publish standing in, false when neither is known
func idle(cfg *config.Config) (time.Duration, bool) {
	for _, name := range []string{"activity", "env"} {
		if st, err := os.Stat(filepath.Join(StateDir(cfg), name)); err == nil {
			return max(0, time.Since(st.ModTime())), true
		}
	}
	return 0, false
}

// How long the screen may go unused before it stops, zero when it never does
func idleStop(cfg *config.Config) time.Duration {
	minutes := float64(bdconfig.DefaultIdleStop)
	if raw := strings.TrimSpace(cfg.GetString(bdconfig.IdleStop)); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil {
			minutes = v
		}
	}
	if math.IsNaN(minutes) || math.IsInf(minutes, 0) || minutes <= 0 {
		return 0
	}
	return time.Duration(minutes * float64(time.Minute))
}

// How long the screen, or the launcher named, has been idle when it is due for the idle stop now; false when it is not
func (d *Desktop) idleDue(ctx context.Context, cfg *config.Config, of *launcher) (time.Duration, bool) {
	limit := idleStop(cfg)
	running, err := d.Running(ctx, cfg)
	if limit <= 0 || err != nil || !running {
		return 0, false
	}
	if of != nil {
		if current, ok := live(cfg); !ok || current != *of {
			return 0, false
		}
	}
	unused, known := idle(cfg)
	if !known || unused < limit {
		return 0, false
	}
	if d.HumanHolds(cfg) && unused < max(limit*abandonedWindows, abandonedMin) {
		return 0, false
	}
	return unused, true
}

// Stops the screen idle past its limit that no live hold of a person keeps, handing an abandoned hold back; whether it stopped
func (d *Desktop) StopIfIdle(ctx context.Context, cfg *config.Config, of *launcher) (bool, error) {
	unused, due := d.idleDue(ctx, cfg, of)
	if !due {
		return false, nil
	}
	var held Lease
	still := func() bool {
		held = d.Lease(cfg)
		_, due := d.idleDue(ctx, cfg, of)
		return due
	}
	slog.Info(fmt.Sprintf("Bot Desktop for profile %s idle for %.0f min; stopping", cfg.Profile(), unused.Minutes()))
	stopped, err := d.Stop(ctx, cfg, still)
	if err != nil || !stopped {
		return false, err
	}
	if held.Holder == Human {
		epoch := held.Epoch
		if _, err := d.Release(cfg, Handback{Viewer: held.Viewer, Epoch: &epoch}); err != nil {
			slog.Warn(fmt.Sprintf("Bot Desktop for profile %s stopped, but its abandoned lease was not released: %s", cfg.Profile(), err))
		}
	}
	return true, nil
}

// Variables the screen published once its bus was up, none while it is down
func (d *Desktop) Published(ctx context.Context, cfg *config.Config) (map[string]string, error) {
	if m := readMarker(cfg); m != nil {
		published := map[string]string{}
		err := d.sandbox(ctx, cfg, false, func(env terminal.Environment) error {
			if env == nil {
				return nil
			}
			var err error
			published, err = d.sandboxPublished(env, remoteDir(env, cfg.Profile()))
			return err
		})
		return published, err
	}
	if _, ok := live(cfg); !ok {
		return map[string]string{}, nil
	}
	raw, ok := trimmed(filepath.Join(StateDir(cfg), "env"))
	if !ok || raw == "" {
		return map[string]string{}, nil
	}
	return parseEnv(raw), nil
}

// Socket of this machine a viewer reaches the screen through, empty while it is down or in a sandbox
func Socket(cfg *config.Config) string {
	if readMarker(cfg) != nil {
		return ""
	}
	if _, ok := live(cfg); !ok {
		return ""
	}
	sock := filepath.Join(StateDir(cfg), "rfb.sock")
	if _, err := os.Stat(sock); err != nil {
		return ""
	}
	return sock
}

// Whether the sandbox a marker names still holds the screen, a marker of a sandbox that is gone dropped
func (d *Desktop) sandboxRunning(ctx context.Context, cfg *config.Config) (bool, error) {
	m := readMarker(cfg)
	if m == nil {
		return false, nil
	}
	kept := false
	if err := d.sandbox(ctx, cfg, false, func(env terminal.Environment) error { kept = env != nil; return nil }); err != nil {
		return false, err
	}
	if kept || d.markerAlive(m) {
		return true, nil
	}
	return false, dropMarker(cfg)
}

// Runs work on the environment the marked screen lives in, attached again when its sandbox is alive; nil environment when there is none
func (d *Desktop) owned(ctx context.Context, cfg *config.Config, work func(env terminal.Environment) error) error {
	m := readMarker(cfg)
	if m == nil {
		return work(nil)
	}
	return d.sandbox(ctx, cfg, d.markerAlive(m), work)
}

// Whether the screen published a display
func (d *Desktop) Running(ctx context.Context, cfg *config.Config) (bool, error) {
	published, err := d.Published(ctx, cfg)
	return published["DISPLAY"] != "", err
}

// Size of the screen, 1440x900 by default
func geometry(cfg *config.Config) string {
	return cmp.Or(strings.TrimSpace(cfg.GetString(bdconfig.Geometry)), bdconfig.DefaultGeometry)
}

// Status of the profile's screen, the sandbox's when it lives there
func (d *Desktop) Status(ctx context.Context, cfg *config.Config) (Status, error) {
	where := d.Resolve(cfg)
	if where.Where == Terminal || readMarker(cfg) != nil {
		return d.sandboxStatus(ctx, cfg, where)
	}
	supported := Supported()
	missing := slices.Clone(Required)
	if supported {
		missing = Missing()
	}
	l, alive := live(cfg)
	published, err := d.Published(ctx, cfg)
	if err != nil {
		return Status{}, err
	}
	running := alive && published["DISPLAY"] != ""
	mem := Memory{Available: -1, Limit: -1}
	if supported {
		mem = memory()
	}
	st := Status{Profile: cfg.Profile(), Supported: supported, Installed: len(missing) == 0, Missing: missing, Running: running,
		Display: published["DISPLAY"], Socket: Socket(cfg), Geometry: geometry(cfg), Available: mem.Available, Limit: mem.Limit,
		Placement: Gateway, Lease: d.Lease(cfg)}
	if alive {
		st.PID = l.pid
	}
	if len(missing) > 0 {
		st.Install = InstallCommand()
	}
	if supported {
		st.Browser = d.Executable(cfg)
	}
	if !running && len(missing) == 0 && supported {
		st.Blocker = blocker(mem, minFree(cfg))
	}
	return st, nil
}

// Status of a screen placed in the sandbox, what it has known once the sandbox exists
func (d *Desktop) sandboxStatus(ctx context.Context, cfg *config.Config, where Placement) (Status, error) {
	missing, published := []string{}, map[string]string{}
	err := d.owned(ctx, cfg, func(env terminal.Environment) error {
		if env == nil {
			return nil
		}
		var err error
		if missing, err = d.sandboxMissing(env); err != nil {
			return err
		}
		published, err = d.sandboxPublished(env, remoteDir(env, cfg.Profile()))
		return err
	})
	if err != nil {
		return Status{}, err
	}
	st := Status{Profile: cfg.Profile(), Supported: true, Installed: true, Missing: missing, Running: published["DISPLAY"] != "",
		Display: published["DISPLAY"], Geometry: geometry(cfg), Available: -1, Limit: -1, Placement: Terminal + ":" + where.Backend,
		Lease: d.Lease(cfg)}
	if len(missing) > 0 {
		advice := cmp.Or(imageAdvice(where.Backend), fmt.Sprintf("use %s as terminal.%s_image (the sandbox icebath builds, desktop stack included)",
			sandbox.Image, where.Backend))
		st.Blocker = fmt.Sprintf("The terminal backend's sandbox image lacks %s. %s%s, or set bot_desktop.placement: gateway.",
			strings.Join(missing, ", "), strings.ToUpper(advice[:1]), advice[1:])
		if backend, ok := d.terminal.Named(cfg, where.Backend); ok && backend.Switch != nil {
			if sw := backend.Switch(cfg); sw != nil {
				st.Switch = sw
				st.Blocker = fmt.Sprintf("Your sandbox container still runs %s, which has no desktop. Switch it to %s: files in /root and /workspace "+
					"stay, packages installed inside the container are reinstalled on demand.", sw.Current, sw.Target)
			}
		}
	}
	return st, nil
}

// Starts the profile's screen, one already up kept, waiting for the launcher to publish
func (d *Desktop) Start(ctx context.Context, cfg *config.Config, wait time.Duration) (Status, error) {
	d.watch(cfg)
	where := d.Resolve(cfg)
	switch {
	case where.Where == Refused:
		return Status{}, errors.New(where.Reason)
	case where.Where == Terminal:
		return d.startInSandbox(ctx, cfg, wait)
	case !Supported():
		return Status{}, errors.New("Bot Desktop runs on Linux gateway hosts only")
	}
	if missing := Missing(); len(missing) > 0 {
		need := "Bot Desktop needs " + strings.Join(missing, ", ") + " on the gateway host"
		switch {
		case Manager() == "":
			return Status{}, errors.New(need + ", and no supported package manager (apt/dnf/pacman) is available to install them. Install TigerVNC " +
				"(Xvnc) and the Xfce core components with this distro's own tooling.")
		case !Installable():
			return Status{}, errors.New(need + ", and this host cannot install them: the process is unprivileged and there is no sudo. On the " +
				"published Docker image the packages have to be baked in, so this needs a newer image rather than an install.")
		}
		return Status{}, errors.New(need + ". Install: " + InstallCommand())
	}
	dir := StateDir(cfg)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Status{}, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return Status{}, err
	}
	unlock, err := fsx.Lock(ctx, filepath.Join(dir, "start.lock"), -1)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	if _, ok := live(cfg); ok {
		if published, err := d.Published(ctx, cfg); err != nil {
			return Status{}, err
		} else if published["DISPLAY"] != "" {
			return d.Status(ctx, cfg)
		}
	}
	floor := minFree(cfg)
	mem := memory()
	if reason := blocker(mem, floor); reason != "" {
		return Status{}, errors.New(reason)
	}
	if mem.Available >= 0 && mem.Available < tight(floor) {
		slog.Warn(fmt.Sprintf("Bot Desktop starting with %d MB available; a browser with a few pages open can use most of that.", mem.Available))
	}
	if _, ok := live(cfg); !ok {
		reapOrphan(cfg, dir)
	}
	if err := os.MkdirAll(filepath.Dir(allocLock()), 0o700); err != nil {
		return Status{}, err
	}
	st, err := d.spawn(ctx, cfg, dir, wait)
	if _, failed := errors.AsType[*launchError](err); failed {
		os.Remove(filepath.Join(dir, "display"))
	}
	return st, err
}

// A launch that failed, after which the display claimed is let go
type launchError struct{ message string }

func (e *launchError) Error() string { return e.message }

// Starts the launcher holding the machine's display lock until Xvnc claims its number, then waits for it to publish
func (d *Desktop) spawn(ctx context.Context, cfg *config.Config, dir string, wait time.Duration) (Status, error) {
	unlockAlloc, err := fsx.Lock(ctx, allocLock(), -1)
	if err != nil {
		return Status{}, err
	}
	released := false
	release := func() {
		if !released {
			released = true
			unlockAlloc()
		}
	}
	defer release()
	num, err := pickDisplay(cfg)
	if err != nil {
		return Status{}, &launchError{err.Error()}
	}
	if err := os.WriteFile(filepath.Join(dir, "display"), []byte(strconv.Itoa(num)), 0o600); err != nil {
		return Status{}, err
	}
	envFile := filepath.Join(dir, "env")
	os.Remove(envFile)
	env := d.terminal.ChildEnv(cfg, nil)
	env["HOME"] = env["ICEBATH_REAL_HOME"]
	for _, k := range []string{"DISPLAY", "XAUTHORITY", "WAYLAND_DISPLAY", "DBUS_SESSION_BUS_ADDRESS", "SESSION_MANAGER"} {
		delete(env, k)
	}
	idleCheck, err := idleCommand(cfg)
	if err != nil {
		return Status{}, err
	}
	maps.Copy(env, map[string]string{
		"ICEBATH_BD_PROFILE":          cfg.Profile(),
		"ICEBATH_BD_DISPLAY_NUM":      strconv.Itoa(num),
		"ICEBATH_BD_SOCKET":           filepath.Join(dir, "rfb.sock"),
		"ICEBATH_BD_XAUTH":            filepath.Join(dir, "Xauthority"),
		"ICEBATH_BD_ENV_FILE":         envFile,
		"ICEBATH_BD_CONFIG_HOME":      filepath.Join(dir, "xdg"),
		"ICEBATH_BD_GEOMETRY":         geometry(cfg),
		"ICEBATH_BD_IDLE_CMD":         terminal.Join(idleCheck),
		"ICEBATH_BD_HOME":             cfg.ProfileDir(cfg.Profile()),
		"ICEBATH_BD_IDLE_FIRST_DELAY": strconv.Itoa(nextCheck(idleStop(cfg), 0)),
	})
	if exe, profile, ok := d.dock(cfg); ok {
		if err := seed(profile); err != nil {
			return Status{}, fmt.Errorf("could not write the preferences that keep the dock browser from calling home into %s: %w", profile, err)
		}
		env["ICEBATH_BD_BROWSER_EXEC"], env["ICEBATH_BD_BROWSER_EXEC_LINE"] = exe, execLine(exe, profile, browser.Bypass())
	}
	logPath := filepath.Join(dir, "launcher.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return Status{}, err
	}
	script := filepath.Join(dir, "launcher.sh")
	if err := os.WriteFile(script, launcherScript, 0o700); err != nil {
		logFile.Close()
		return Status{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "wallpaper.png"), wallpaperPNG, 0o600); err != nil {
		logFile.Close()
		return Status{}, err
	}
	cmd := exec.Command("bash", script)
	cmd.Env = terminal.Environ(env)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = sessionAttr()
	err = cmd.Start()
	logFile.Close()
	if err != nil {
		return Status{}, err
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	pid := cmd.Process.Pid
	born, _ := terminal.StartTime(pid)
	if err := os.WriteFile(filepath.Join(dir, "launcher.pid"), fmt.Appendf(nil, "%d %d", pid, born), 0o600); err != nil {
		return Status{}, err
	}
	for deadline := time.Now().Add(wait); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if _, ok := xLock(num); ok {
			release()
		}
		select {
		case <-exited:
			data, _ := os.ReadFile(logPath)
			return Status{}, &launchError{fmt.Sprintf("Bot Desktop launcher exited with %d:\n%s", cmd.ProcessState.ExitCode(), tail(data, 2000))}
		default:
		}
		_, envErr := os.Stat(envFile)
		_, sockErr := os.Stat(filepath.Join(dir, "rfb.sock"))
		if envErr == nil && sockErr == nil {
			slog.Info(fmt.Sprintf("Bot Desktop for profile %s up on :%d", cfg.Profile(), num))
			d.Touch(cfg)
			return d.Status(ctx, cfg)
		}
	}
	stopGroup(pid, pid, 2*time.Second)
	<-exited
	for _, name := range []string{"launcher.pid", "env", "rfb.sock"} {
		os.Remove(filepath.Join(dir, name))
	}
	return Status{}, &launchError{fmt.Sprintf("Bot Desktop did not publish its display within %.0fs (see %s)", wait.Seconds(), logPath)}
}

// Brings the screen up inside the terminal backend's sandbox and records what it published on this machine
func (d *Desktop) startInSandbox(ctx context.Context, cfg *config.Config, wait time.Duration) (Status, error) {
	dir := StateDir(cfg)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Status{}, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return Status{}, err
	}
	var published map[string]string
	err := d.sandbox(ctx, cfg, true, func(env terminal.Environment) error {
		if env == nil {
			return errNoSandbox
		}
		unlock, err := fsx.Lock(ctx, filepath.Join(dir, "start.lock"), -1)
		if err != nil {
			return err
		}
		defer unlock()
		exe, err := d.SandboxChromium(env)
		if err != nil {
			return err
		}
		line := ""
		if exe != "" {
			profile, err := d.SandboxProfile(env)
			if err != nil {
				return err
			}
			if err := d.SeedSandboxProfile(env, profile); err != nil {
				return fmt.Errorf("could not write the preferences that keep the sandbox browser from calling home into %s: %w", profile, err)
			}
			line = execLine(exe, profile, true)
		}
		published, err = d.sandboxStart(cfg, env, cfg.Profile(), geometry(cfg), max(wait, sandboxWait), exe, line)
		return err
	})
	if err != nil {
		return Status{}, err
	}
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(published)) {
		b.WriteString(k + "=" + published[k] + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "env"), []byte(b.String()), 0o600); err != nil {
		return Status{}, err
	}
	d.Touch(cfg)
	return d.Status(ctx, cfg)
}

// Stops the profile's screen, still saying first whether a stop waited on is still due; whether a launcher or a server left behind was
// signalled
func (d *Desktop) Stop(ctx context.Context, cfg *config.Config, still func() bool) (bool, error) {
	if readMarker(cfg) != nil || d.Resolve(cfg).Where == Terminal {
		if still != nil && !still() {
			return false, nil
		}
		stopped := false
		err := d.owned(ctx, cfg, func(env terminal.Environment) error {
			if env == nil {
				return nil
			}
			var err error
			stopped, err = d.sandboxStop(cfg, env, cfg.Profile())
			return err
		})
		if err != nil {
			return false, err
		}
		if err := dropMarker(cfg); err != nil {
			return false, err
		}
		for _, name := range []string{"env", "activity"} {
			if err := os.Remove(filepath.Join(StateDir(cfg), name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return false, err
			}
		}
		return stopped, nil
	}
	if !Supported() {
		return false, nil
	}
	dir := StateDir(cfg)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	unlock, err := fsx.Lock(ctx, filepath.Join(dir, "start.lock"), -1)
	if err != nil {
		return false, err
	}
	defer unlock()
	if still != nil && !still() {
		slog.Info(fmt.Sprintf("Bot Desktop for profile %s changed while the idle stop waited; kept", cfg.Profile()))
		return false, nil
	}
	return stopLocked(cfg, dir), nil
}

// Writes the quiet preferences into the dock browser's profile, one a browser already runs on having read them as it started
func seed(profile string) error {
	if browser.RunningOn(profile) {
		return nil
	}
	return browser.Seed(profile, true)
}

// Stops the live launcher's group, or reaps the server a dead one left, under start.lock
func stopLocked(cfg *config.Config, dir string) bool {
	l, ok := live(cfg)
	if !ok {
		reaped := reapOrphan(cfg, dir)
		os.Remove(filepath.Join(dir, "env"))
		return reaped
	}
	stopGroup(l.pid, l.pid, 5*time.Second)
	for _, name := range []string{"launcher.pid", "env", "activity", "idle-watch.out"} {
		os.Remove(filepath.Join(dir, name))
	}
	return true
}

// Moves the profile's sandbox to the image with the desktop, or keeps the one it runs, answering the image now set
func (d *Desktop) SwitchImage(ctx context.Context, cfg *config.Config, approve bool) (string, error) {
	where := d.Resolve(cfg)
	backend, ok := d.terminal.Named(cfg, where.Backend)
	if !ok || backend.Switch == nil || backend.Decide == nil {
		return "", errors.New("no sandbox image switch is pending for this profile")
	}
	sw := backend.Switch(cfg)
	if sw == nil {
		return "", errors.New("no sandbox image switch is pending for this profile")
	}
	image, err := backend.Decide(cfg, sw, approve)
	if err != nil {
		return "", err
	}
	if approve {
		d.terminal.Evict(cfg, "", "")
	}
	return image, nil
}

// Command of this machine running argv as the desktop user on the profile's screen in the terminal backend's sandbox, with the variables
// over what the screen published, which names what it is for in a refusal: the screen brought up first when start says so. Nil without
// error for a screen on this machine, and when not starting it, for one that is not up
func (d *Desktop) Command(ctx context.Context, cfg *config.Config, argv []string, vars map[string]string, what string, start bool) ([]string, error) {
	if start {
		where, err := d.Place(ctx, cfg)
		if err != nil || where == Gateway {
			return nil, err
		}
	} else if !d.InSandbox(cfg) {
		return nil, nil
	}
	published, err := d.Published(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if published["DISPLAY"] == "" {
		if !start {
			return nil, nil
		}
		return nil, errors.New("the screen inside the terminal backend's sandbox is gone; start it again")
	}
	merged := maps.Clone(published)
	maps.Copy(merged, vars)
	var command []string
	err = d.sandbox(ctx, cfg, start, func(env terminal.Environment) error {
		if env == nil {
			if start {
				return errors.New("the terminal backend's sandbox is not running, so there is nowhere to run " + argv[0])
			}
			return nil
		}
		var err error
		command, err = d.DisplayCommand(env, merged, argv, what)
		return err
	})
	if err == nil && command != nil {
		d.Touch(cfg)
	}
	return command, err
}
