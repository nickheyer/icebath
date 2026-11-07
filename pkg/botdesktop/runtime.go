package botdesktop

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pydeadline"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	displayMin = 20
	displayMax = 89
)

// RequiredBinaries are the host programs the gateway hosted screen needs
var RequiredBinaries = []string{"Xvnc", "xfwm4", "xfce4-panel", "xfdesktop", "xfsettingsd", "dbus-run-session",
	"xauth", "xdpyinfo", "setxkbmap", "xprop"}

// BinaryPackages maps each required binary to its package per package manager
var BinaryPackages = map[string]map[string]string{
	"apt": {"Xvnc": "tigervnc-standalone-server", "xfwm4": "xfwm4", "xfce4-panel": "xfce4-panel",
		"xfdesktop": "xfdesktop4", "xfsettingsd": "xfce4-settings", "dbus-run-session": "dbus-x11",
		"xauth": "xauth", "xdpyinfo": "x11-utils", "setxkbmap": "x11-xkb-utils", "xprop": "x11-utils"},
	"dnf": {"Xvnc": "tigervnc-x11-server", "xfwm4": "xfwm4", "xfce4-panel": "xfce4-panel",
		"xfdesktop": "xfdesktop", "xfsettingsd": "xfce4-settings", "dbus-run-session": "dbus-daemon",
		"xauth": "xorg-x11-xauth", "xdpyinfo": "xdpyinfo", "setxkbmap": "setxkbmap", "xprop": "xprop"},
	"pacman": {"Xvnc": "tigervnc", "xfwm4": "xfwm4", "xfce4-panel": "xfce4-panel", "xfdesktop": "xfdesktop",
		"xfsettingsd": "xfce4-settings", "dbus-run-session": "dbus",
		"xauth": "xorg-xauth", "xdpyinfo": "xorg-xdpyinfo", "setxkbmap": "xorg-setxkbmap", "xprop": "xorg-xprop"},
}

// Packages are the full install sets per package manager
var Packages = map[string][]string{
	"apt": {"tigervnc-standalone-server", "xfce4-panel", "xfwm4", "xfdesktop4", "xfce4-settings",
		"xfce4-terminal", "dbus-x11", "x11-xserver-utils", "x11-utils", "x11-xkb-utils", "xauth",
		"fonts-dejavu-core"},
	"dnf": {"tigervnc-x11-server", "xfce4-panel", "xfwm4", "xfdesktop", "xfce4-settings",
		"xfce4-terminal", "dbus-daemon", "xsetroot", "xset", "xdpyinfo", "xprop", "xorg-x11-xauth", "setxkbmap",
		"dejavu-sans-fonts"},
	"pacman": {"tigervnc", "xfce4-panel", "xfwm4", "xfdesktop", "xfce4-settings", "xfce4-terminal", "dbus",
		"xorg-xsetroot", "xorg-xset", "xorg-xdpyinfo", "xorg-xprop", "xorg-xauth", "xorg-setxkbmap",
		"ttf-dejavu"},
}

// Idle stop defaults and the human lease abandonment rule
const (
	DefaultIdleStopMinutes      = 30
	HumanLeaseAbandonedWindows  = 4
	HumanLeaseAbandonedMinS     = 600.0
	defaultLauncherWaitSeconds  = 15.0
	sandboxLauncherWaitSecondsF = 20.0
)

// Python sys.platform of the host, swapped by tests
var hostPlatform = func() string {
	if goruntime.GOOS == "windows" {
		return "win32"
	}
	return goruntime.GOOS
}()

// Pending default image switch of the docker sandbox, assigned by the wiring from the sandbox image switch port
var SandboxImageSwitchPending func(env *tools.Env) (*ImageSwitch, error)

// A pending default image switch the pane can approve
type ImageSwitch struct {
	CurrentImage string
	TargetImage  string
	Containers   []string
}

// Per profile screen state directory under the env home
func StateDir(env *tools.Env) string { return filepath.Join(envHome(env), "bot-desktop") }

// True on Linux, the only host the screen runs on
func IsSupportedHost() bool { return strings.HasPrefix(hostPlatform, "linux") }

// Required binaries missing from PATH
func MissingBinaries() []string {
	out := []string{}
	for _, b := range RequiredBinaries {
		if whichBinary(b) == "" {
			out = append(out, b)
		}
	}
	return out
}

// apt, dnf or pacman by the first manager on PATH, empty for none
func PackageManager() string {
	for _, m := range []string{"apt-get", "dnf", "pacman"} {
		if whichBinary(m) != "" {
			if m == "apt-get" {
				return "apt"
			}
			return m
		}
	}
	return ""
}

// Distro install command as the human types it here, sudo prefixed unless root, empty without a manager
func InstallCommand() string {
	m := PackageManager()
	if m == "" {
		return ""
	}
	pkgs := strings.Join(Packages[m], " ")
	body := map[string]string{
		"apt":    "apt-get install -y --no-install-recommends " + pkgs,
		"dnf":    "dnf install -y " + pkgs,
		"pacman": "pacman -S --needed --noconfirm " + pkgs,
	}[m]
	if IsRoot() {
		return body
	}
	return "sudo " + body
}

// True when the process runs as root
func IsRoot() bool { return isRoot() }

// Whether the install command could succeed here, false unprivileged without sudo
func Installable() bool {
	if PackageManager() == "" {
		return false
	}
	return IsRoot() || whichBinary("sudo") != ""
}

// Screen status the pane renders
type DesktopStatus struct {
	Profile           string
	Supported         bool
	Installed         bool
	Missing           []string
	Running           bool
	PID               *int
	Display           *string
	Socket            *string
	Geometry          string
	InstallCommand    *string
	Browser           *string
	Blocker           *string
	MemoryAvailableMB *int
	MemoryLimitMB     *int
	Placement         string
	ImageSwitch       *pyjson.Object
}

// Field dict in dataclass order
func (s DesktopStatus) AsDict() *pyjson.Object {
	o := pyjson.NewObject()
	opt := func(k string, v any) {
		switch t := v.(type) {
		case *int:
			if t == nil {
				o.Set(k, nil)
			} else {
				o.Set(k, *t)
			}
		case *string:
			if t == nil {
				o.Set(k, nil)
			} else {
				o.Set(k, *t)
			}
		case *pyjson.Object:
			if t == nil {
				o.Set(k, nil)
			} else {
				o.Set(k, t)
			}
		default:
			o.Set(k, v)
		}
	}
	missing := make([]any, len(s.Missing))
	for i, m := range s.Missing {
		missing[i] = m
	}
	o.Set("profile", s.Profile)
	o.Set("supported", s.Supported)
	o.Set("installed", s.Installed)
	o.Set("missing", missing)
	o.Set("running", s.Running)
	opt("pid", s.PID)
	opt("display", s.Display)
	opt("socket", s.Socket)
	o.Set("geometry", s.Geometry)
	opt("install_command", s.InstallCommand)
	opt("browser", s.Browser)
	opt("blocker", s.Blocker)
	opt("memory_available_mb", s.MemoryAvailableMB)
	opt("memory_limit_mb", s.MemoryLimitMB)
	o.Set("placement", s.Placement)
	opt("image_switch", s.ImageSwitch)
	return o
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Stripped file text, ok false when unreadable or empty
func readState(path string) (string, bool) {
	text, ok := readTrimmed(path)
	return text, ok && text != ""
}

// Alive and not a zombie
func pidAlive(pid int) bool {
	return pid > 0 && proc.Exists(pid) && !proc.Zombie(pid)
}

// Process start time in epoch seconds
var createTime = func(pid int) (float64, bool) {
	if pid <= 0 {
		return 0, false
	}
	t, err := proc.Birth(pid)
	return t, err == nil
}

type launcherID struct {
	pid  int
	born float64
}

// Live launcher named by launcher.pid with a matching start time, nil otherwise
func launcherIdentity(env *tools.Env) *launcherID {
	raw, _ := readState(filepath.Join(StateDir(env), "launcher.pid"))
	pidS, bornS, _ := strings.Cut(raw, " ")
	if !isDigits(pidS) || bornS == "" {
		return nil
	}
	n, ok := pyint.Parse(pidS)
	born, err := pyint.ParseFloat(bornS)
	if !ok || err != nil || !n.IsInt64() {
		return nil
	}
	pid := int(n.Int64())
	actual, ok := createTime(pid)
	if ok && math.Abs(actual-born) < 0.01 && pidAlive(pid) {
		return &launcherID{pid, born}
	}
	return nil
}

func isDigits(s string) bool { return s != "" && strings.Trim(s, "0123456789") == "" }

// Live launcher pid, ok false when none
func launcherPid(env *tools.Env) (int, bool) {
	if id := launcherIdentity(env); id != nil {
		return id.pid, true
	}
	return 0, false
}

// Pid launcher.pid names, alive or not
func recordedLauncherPid(env *tools.Env) (int, bool) {
	raw, _ := readState(filepath.Join(StateDir(env), "launcher.pid"))
	pidS, bornS, _ := strings.Cut(raw, " ")
	if !isDigits(pidS) || bornS == "" {
		return 0, false
	}
	n, ok := pyint.Parse(pidS)
	if !ok || !n.IsInt64() {
		return 0, false
	}
	return int(n.Int64()), true
}

var (
	xLockDir   = "/tmp"
	xUnixTable = "/proc/net/unix"
)

// Pid in the X lock of a display number
func xLockPid(num int) (int, bool) {
	text, ok := readTrimmed(filepath.Join(xLockDir, fmt.Sprintf(".X%d-lock", num)))
	if !ok {
		return 0, false
	}
	n, ok := pyint.Parse(text)
	if !ok || !n.IsInt64() {
		return 0, false
	}
	return int(n.Int64()), true
}

// True while the abstract X11 socket of the display is bound
func xSocketBound(num int) bool {
	text, ok := readTrimmed(xUnixTable)
	if !ok {
		return false
	}
	want := fmt.Sprintf("/tmp/.X11-unix/X%d", num)
	for _, line := range splitLines(text) {
		f := strings.Fields(line)
		if len(f) > 0 && strings.TrimLeft(f[len(f)-1], "@") == want {
			return true
		}
	}
	return false
}

// A live X server owns the display through its lock or its bound socket
func displayInUse(num int) bool {
	pid, ok := xLockPid(num)
	return (ok && pidAlive(pid)) || xSocketBound(num)
}

// Command line of every live process, nil entries for those it cannot read
var processCmdlines = func() []struct {
	pid  int
	argv []string
} {
	table, err := proc.Snapshot()
	if err != nil {
		return nil
	}
	out := []struct {
		pid  int
		argv []string
	}{}
	for _, p := range table {
		argv, _ := proc.Cmdline(p.PID)
		out = append(out, struct {
			pid  int
			argv []string
		}{p.PID, argv})
	}
	return out
}

// Kills the X server a dead launcher left behind when it is provably ours, true when something was signalled
func reapOrphanedServer(env *tools.Env, sd string) bool {
	sock := filepath.Join(sd, "rfb.sock")
	bindsOurSocket := func(argv []string) bool {
		first := ""
		if len(argv) > 0 {
			first = argv[0]
		}
		if !strings.Contains(filepath.Base(first), "Xvnc") {
			return false
		}
		for _, a := range argv {
			if a == sock {
				return true
			}
		}
		return false
	}
	recorded, _ := readState(filepath.Join(sd, "display"))
	pid, havePid := 0, false
	if isDigits(recorded) {
		if n, ok := pyint.Parse(recorded); ok && n.IsInt64() {
			pid, havePid = xLockPid(int(n.Int64()))
		}
	}
	if !havePid || !pidAlive(pid) {
		havePid = false
		for _, p := range processCmdlines() {
			if bindsOurSocket(p.argv) {
				pid, havePid = p.pid, true
				break
			}
		}
		if !havePid {
			return false
		}
	}
	launcher, haveLauncher := recordedLauncherPid(env)
	pgid, err := getpgid(pid)
	if err != nil {
		return false
	}
	cmdline, err := proc.Cmdline(pid)
	if err != nil {
		return false
	}
	ownGroup := haveLauncher && pgid == launcher
	if !ownGroup && !bindsOurSocket(cmdline) {
		return false
	}
	shown := recorded
	if shown == "" {
		shown = "?"
	}
	launcherShown := "None"
	if haveLauncher {
		launcherShown = fmt.Sprint(launcher)
	}
	slog.Warn(fmt.Sprintf("Bot Desktop launcher %s is gone but its X server (pid %d) survived on :%s; reaping", launcherShown, pid, shown))
	var group *int
	if ownGroup {
		group = &pgid
	}
	killGroupThenWait(group, pid, 2.0)
	if recorded != "" {
		os.Remove(filepath.Join(xLockDir, fmt.Sprintf(".X%s-lock", recorded)))
	}
	for _, name := range []string{"launcher.pid", "env", "rfb.sock"} {
		os.Remove(filepath.Join(sd, name))
	}
	return true
}

// SIGTERM the group or the lone pid, SIGKILL whatever is left after grace
func killGroupThenWait(pgid *int, pid int, grace float64) {
	signal := func(sig syscall.Signal) {
		if pgid != nil {
			killpg(*pgid, sig)
		} else {
			killPid(pid, sig)
		}
	}
	anythingLeft := func() bool {
		if pgid == nil {
			return pidAlive(pid)
		}
		err := killpg(*pgid, 0)
		if err != nil && isESRCH(err) {
			return false
		}
		return true
	}
	gone := func() (bool, bool) {
		waitpidNoHang(pid)
		ok := !anythingLeft()
		return ok, ok
	}
	signal(syscall.SIGTERM)
	if pydeadline.PollUntil(gone, time.Duration(grace*float64(time.Second)), 50*time.Millisecond) {
		return
	}
	signal(syscall.SIGKILL)
	time.Sleep(100 * time.Millisecond)
	waitpidNoHang(pid)
}

// Host wide display allocation lock path
var allocLock = func() string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		u, _ := os.UserHomeDir()
		base = filepath.Join(u, ".cache")
	}
	return filepath.Join(base, "icebath-bot-desktop-alloc.lock")
}()

// Recorded display when no other server holds it, else the first free number in the band
func pickDisplay(env *tools.Env) (int, error) {
	recorded, _ := readState(filepath.Join(StateDir(env), "display"))
	if isDigits(recorded) {
		if n, ok := pyint.Parse(recorded); ok && n.IsInt64() && !displayInUse(int(n.Int64())) {
			return int(n.Int64()), nil
		}
	}
	for num := displayMin; num <= displayMax; num++ {
		if !displayInUse(num) {
			return num, nil
		}
	}
	return 0, errors.New("no free X display number in the Bot Desktop band")
}

// Picks a display under the host wide allocation lock
func allocateDisplay(env *tools.Env) (int, error) {
	if err := os.MkdirAll(filepath.Dir(allocLock), 0o700); err != nil {
		return 0, err
	}
	unlock, err := flockPath(allocLock)
	if err != nil {
		return 0, err
	}
	defer unlock()
	return pickDisplay(env)
}

// Base env with the running screen DISPLAY, XAUTHORITY and DBUS merged in, unchanged when it is down
func DesktopEnv(env *tools.Env, base map[string]string) map[string]string {
	out := map[string]string{}
	if base == nil {
		for _, kv := range os.Environ() {
			if k, v, ok := strings.Cut(kv, "="); ok {
				out[k] = v
			}
		}
	} else {
		for k, v := range base {
			out[k] = v
		}
	}
	published := MustPublishedEnv(env)
	if len(published) > 0 {
		TouchActivity(env)
		for k, v := range published {
			out[k] = v
		}
		delete(out, "WAYLAND_DISPLAY")
	}
	return out
}

// Tool boundary hook bringing the sandbox screen up, or a headless host screen when auto_start allows
func EnsureStartedForTool(env *tools.Env) {
	if MustPublishedEnv(env)["DISPLAY"] != "" {
		TouchActivity(env)
		return
	}
	where, err := Resolve(env)
	if err != nil {
		panic(err)
	}
	if where.Where == Refused {
		return
	}
	if where.Where == Terminal {
		if _, err := Start(env, defaultLauncherWaitSeconds); err != nil {
			slog.Info(fmt.Sprintf("Bot Desktop sandbox start deferred to the tool: %s", err))
		}
		return
	}
	environ := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			environ[k] = v
		}
	}
	auto, err := shouldAutoStart(env, environ)
	if err != nil {
		panic(err)
	}
	if !auto {
		return
	}
	if _, err := Start(env, defaultLauncherWaitSeconds); err != nil {
		slog.Info(fmt.Sprintf("Bot Desktop auto-start skipped: %s", err))
	}
}

// Where the browser or cua-driver about to spawn must run, starting a down sandbox screen first
func ToolPlacement(env *tools.Env) (string, error) {
	where, err := Resolve(env)
	if err != nil {
		return "", err
	}
	if where.Where == Refused {
		return "", errors.New(where.Reason)
	}
	if where.Where == Gateway {
		return Gateway, nil
	}
	running, err := SandboxScreenRunning(env)
	if err != nil {
		return "", err
	}
	published := map[string]string{}
	if running {
		if published, err = PublishedEnv(env); err != nil {
			return "", err
		}
	}
	if !running || published["DISPLAY"] == "" {
		if _, err := Start(env, defaultLauncherWaitSeconds); err != nil {
			return "", err
		}
	}
	return Terminal, nil
}

// Linux host with no display, the packages present and bot_desktop.auto_start on
func shouldAutoStart(env *tools.Env, environ map[string]string) (bool, error) {
	if !IsSupportedHost() || environ["DISPLAY"] != "" || environ["WAYLAND_DISPLAY"] != "" {
		return false, nil
	}
	if len(MissingBinaries()) > 0 {
		return false, nil
	}
	cfg, err := botDesktopSection(env)
	if err != nil {
		return false, err
	}
	return pyrepr.Truthy(cfg["auto_start"]), nil
}

// Stamps the screen activity file
func TouchActivity(env *tools.Env) {
	path := filepath.Join(StateDir(env), "activity")
	if f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o666); err == nil {
		f.Close()
		now := time.Now()
		os.Chtimes(path, now, now)
	}
}

// Seconds since the last stamped use, the env publish time standing in, nil when neither exists
func IdleSeconds(env *tools.Env) *float64 {
	for _, name := range []string{"activity", "env"} {
		st, err := os.Stat(filepath.Join(StateDir(env), name))
		if err != nil {
			continue
		}
		v := max(0.0, float64(time.Now().UnixNano()-st.ModTime().UnixNano())/1e9)
		return &v
	}
	return nil
}

// Configured idle stop window in seconds, zero when off or not a positive finite number
func IdleStopSeconds(env *tools.Env) (float64, error) {
	cfg, err := botDesktopSection(env)
	if err != nil {
		return 0, err
	}
	var raw any = DefaultIdleStopMinutes
	if v, ok := cfg["idle_stop_minutes"]; ok {
		raw = v
	}
	minutes, err := pyint.Float(raw)
	if err != nil {
		minutes = DefaultIdleStopMinutes
	}
	seconds := minutes * 60
	if !math.IsInf(seconds, 0) && !math.IsNaN(seconds) && seconds > 0 {
		return seconds, nil
	}
	return 0, nil
}

// Idle seconds when the screen, or the one launcher named, is due for the idle stop now
func idleDue(env *tools.Env, launcher *launcherID) (*float64, error) {
	limit, err := IdleStopSeconds(env)
	if err != nil {
		return nil, err
	}
	running, err := IsRunning(env)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || !running {
		return nil, nil
	}
	if launcher != nil {
		current := launcherIdentity(env)
		if current == nil || current.pid != launcher.pid || math.Abs(current.born-launcher.born) >= 0.01 {
			return nil, nil
		}
	}
	idle := IdleSeconds(env)
	if idle == nil || *idle < limit {
		return nil, nil
	}
	abandonedAfter := max(limit*HumanLeaseAbandonedWindows, HumanLeaseAbandonedMinS)
	if LeaseGet(envHome(env)).Holder == LeaseHuman && *idle < abandonedAfter {
		return nil, nil
	}
	return idle, nil
}

// Stops the screen idle past the limit with no live human hold, handing an abandoned lease back
func StopIfIdle(env *tools.Env, launcher *launcherID) (bool, error) {
	idle, err := idleDue(env, launcher)
	if err != nil || idle == nil {
		return false, err
	}
	var held *BotDesktopLease
	stillDue := func() (bool, error) {
		l := LeaseGet(envHome(env))
		held = &l
		due, err := idleDue(env, launcher)
		return due != nil, err
	}
	slog.Info(fmt.Sprintf("Bot Desktop for profile %s idle for %.0f min; stopping", ProfileName(env), *idle/60))
	stopped, err := Stop(env, stillDue)
	if err != nil || !stopped {
		return false, err
	}
	if held != nil && held.Holder == LeaseHuman {
		viewer, _ := held.ViewerID.(string)
		epoch := held.EpochInt()
		if _, err := LeaseRelease(LeaseReleaseOptions{ViewerID: &viewer, ProfileKey: envHome(env), Epoch: &epoch}); err != nil {
			var pe *os.PathError
			if !errors.As(err, &pe) {
				return true, err
			}
			slog.Warn(fmt.Sprintf("Bot Desktop for profile %s stopped, but its abandoned lease was not released: %s", ProfileName(env), err))
		}
	}
	return true, nil
}

// Variables the launcher published once the screen bus existed, empty while it is down
func PublishedEnv(env *tools.Env) (map[string]string, error) {
	if len(readMarker(env)) > 0 {
		return sandboxPublishedEnv(env)
	}
	if _, ok := launcherPid(env); !ok {
		return map[string]string{}, nil
	}
	raw, ok := readState(filepath.Join(StateDir(env), "env"))
	if !ok {
		return map[string]string{}, nil
	}
	return parseEnvLines(raw), nil
}

// Published env for hook signatures without an error, panicking where hermes raises
func MustPublishedEnv(env *tools.Env) map[string]string {
	out, err := PublishedEnv(env)
	if err != nil {
		panic(err)
	}
	return out
}

// Host RFB socket path, empty when down or sandbox hosted
func RFBSocketPath(env *tools.Env) string {
	if len(readMarker(env)) > 0 {
		return ""
	}
	sock := filepath.Join(StateDir(env), "rfb.sock")
	if _, ok := launcherPid(env); !ok {
		return ""
	}
	if _, err := os.Stat(sock); err != nil {
		return ""
	}
	return sock
}

// True when policy places the screen inside the terminal backend
func InSandbox(env *tools.Env) (bool, error) {
	where, err := Resolve(env)
	return where.Where == Terminal, err
}

// True when a marker exists and its sandbox is alive, dropping a marker whose sandbox is gone
func SandboxScreenRunning(env *tools.Env) (bool, error) {
	marker := readMarker(env)
	if len(marker) == 0 {
		return false, nil
	}
	e, err := TerminalEnvironment(env, false)
	if err != nil {
		return false, err
	}
	if e != nil {
		return true, nil
	}
	if MarkerSandboxAlive(marker) {
		return true, nil
	}
	if err := os.Remove(markerPath(env)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return false, nil
}

// Environment hosting the marked screen, re attached when its sandbox is alive, nil otherwise
func ownedSandboxEnv(env *tools.Env) (tools.Environment, error) {
	marker := readMarker(env)
	if len(marker) == 0 {
		return nil, nil
	}
	e, err := TerminalEnvironment(env, false)
	if err != nil || e != nil {
		return e, err
	}
	if MarkerSandboxAlive(marker) {
		return TerminalEnvironment(env, true)
	}
	return nil, nil
}

// True when the screen published a display
func IsRunning(env *tools.Env) (bool, error) {
	published, err := PublishedEnv(env)
	return published["DISPLAY"] != "", err
}

// Stream carrying the RFB bytes of a sandbox hosted screen
func OpenRFBStream(env *tools.Env) (*tools.StreamProcess, error) {
	e, err := TerminalEnvironment(env, false)
	if err != nil {
		return nil, err
	}
	if e == nil {
		return nil, errors.New("the sandbox hosting this screen is not running")
	}
	return SandboxOpenRFBStream(e, ProfileName(env))
}

// Configured screen geometry, 1440x900 by default
func Geometry(env *tools.Env) (string, error) {
	cfg, err := botDesktopSection(env)
	if err != nil {
		return "", err
	}
	if raw := cfg["geometry"]; pyrepr.Truthy(raw) {
		return pyrepr.Str(raw), nil
	}
	return "1440x900", nil
}

// Screen status of this profile, the sandbox status when the screen lives there
func Status(env *tools.Env, profile string) (DesktopStatus, error) {
	where, err := Resolve(env)
	if err != nil {
		return DesktopStatus{}, err
	}
	if where.Where == Terminal || len(readMarker(env)) > 0 {
		return sandboxStatus(env, profile, where)
	}
	supported := IsSupportedHost()
	missing := append([]string{}, RequiredBinaries...)
	if supported {
		missing = MissingBinaries()
	}
	pid, havePid := launcherPid(env)
	published, err := PublishedEnv(env)
	if err != nil {
		return DesktopStatus{}, err
	}
	running := havePid && published["DISPLAY"] != ""
	mem := MemoryInfo{}
	if supported {
		mem = GetMemoryInfo()
	}
	geometry, err := Geometry(env)
	if err != nil {
		return DesktopStatus{}, err
	}
	st := DesktopStatus{Profile: profile, Supported: supported, Installed: len(missing) == 0, Missing: missing,
		Running: running, Geometry: geometry, MemoryAvailableMB: mem.AvailableMB, MemoryLimitMB: mem.LimitMB, Placement: Gateway}
	if st.Profile == "" {
		st.Profile = ProfileName(env)
	}
	if havePid {
		st.PID = &pid
	}
	if d, ok := published["DISPLAY"]; ok {
		st.Display = &d
	}
	st.Socket = strPtr(RFBSocketPath(env))
	if len(missing) > 0 {
		st.InstallCommand = strPtr(InstallCommand())
	}
	if supported {
		st.Browser = strPtr(Executable(env))
	}
	if !running && len(missing) == 0 && supported {
		need, err := MinFreeMB(env)
		if err != nil {
			return DesktopStatus{}, err
		}
		st.Blocker = strPtr(MemoryBlocker(mem, need))
	}
	return st, nil
}

// Status of a sandbox placed screen, package presence known once the sandbox exists
func sandboxStatus(env *tools.Env, profile string, where Placement) (DesktopStatus, error) {
	e, err := ownedSandboxEnv(env)
	if err != nil {
		return DesktopStatus{}, err
	}
	if e == nil {
		if e, err = TerminalEnvironment(env, false); err != nil {
			return DesktopStatus{}, err
		}
	}
	if profile == "" {
		profile = ProfileName(env)
	}
	missing := []string{}
	published := map[string]string{}
	if e != nil {
		if missing, err = SandboxMissingBinaries(e); err != nil {
			return DesktopStatus{}, err
		}
		if published, err = SandboxPublishedEnv(e, profile); err != nil {
			return DesktopStatus{}, err
		}
	}
	running := published["DISPLAY"] != ""
	var blocker *string
	var imageSwitch *pyjson.Object
	if len(missing) > 0 {
		advice := PluginImageAdvice(where.Backend)
		if advice == "" {
			advice = fmt.Sprintf("use %s as terminal.%s_image (the default sandbox base plus the desktop stack)", SandboxImageHint, where.Backend)
		}
		r := []rune(advice)
		b := fmt.Sprintf("The terminal backend's sandbox image lacks %s. %s%s, or set bot_desktop.placement: gateway.",
			strings.Join(missing, ", "), strings.ToUpper(string(r[0])), string(r[1:]))
		blocker = &b
		if where.Backend == "docker" {
			sw, err := SandboxImageSwitchPending(env)
			if err != nil {
				return DesktopStatus{}, err
			}
			if sw != nil {
				imageSwitch = pyjson.NewObject()
				imageSwitch.Set("current_image", sw.CurrentImage)
				imageSwitch.Set("target_image", sw.TargetImage)
				imageSwitch.Set("containers", len(sw.Containers))
				b := fmt.Sprintf("Your sandbox container still runs %s, which has no desktop. "+
					"Switch it to %s: files in /root and /workspace stay, packages "+
					"installed inside the container are reinstalled on demand.", sw.CurrentImage, sw.TargetImage)
				blocker = &b
			}
		}
	}
	geometry, err := Geometry(env)
	if err != nil {
		return DesktopStatus{}, err
	}
	st := DesktopStatus{Profile: profile, Supported: true, Installed: true, Missing: missing, Running: running,
		Geometry: geometry, Blocker: blocker, Placement: Terminal + ":" + where.Backend, ImageSwitch: imageSwitch}
	if d, ok := published["DISPLAY"]; ok {
		st.Display = &d
	}
	return st, nil
}

// Active profile name of the env home, default when it cannot be read
func ProfileName(env *tools.Env) (name string) {
	defer func() {
		if recover() != nil {
			name = "default"
		}
	}()
	if n := profiles.GetActiveProfileName(envCtx(env)); n != "" {
		return n
	}
	return "default"
}

// Starts this profile screen, idempotent, blocking until the launcher publishes or the wait passes
func Start(env *tools.Env, waitSeconds float64) (DesktopStatus, error) {
	where, err := Resolve(env)
	if err != nil {
		return DesktopStatus{}, err
	}
	if where.Where == Refused {
		return DesktopStatus{}, errors.New(where.Reason)
	}
	if where.Where == Terminal {
		return startInSandbox(env, waitSeconds)
	}
	if !IsSupportedHost() {
		return DesktopStatus{}, errors.New("Bot Desktop runs on Linux gateway hosts only")
	}
	if missing := MissingBinaries(); len(missing) > 0 {
		need := fmt.Sprintf("Bot Desktop needs %s on the gateway host", strings.Join(missing, ", "))
		if PackageManager() == "" {
			return DesktopStatus{}, fmt.Errorf("%s, and no supported package manager (apt/dnf/pacman) is available to install them. "+
				"Install TigerVNC (Xvnc) and the Xfce core components with this distro's own tooling.", need)
		}
		if !Installable() {
			return DesktopStatus{}, fmt.Errorf("%s, and this host cannot install them: the process is unprivileged and there is no "+
				"sudo. On the published Docker image the packages have to be baked in, so this needs a "+
				"newer image rather than an install.", need)
		}
		return DesktopStatus{}, fmt.Errorf("%s. Install: %s", need, InstallCommand())
	}
	sd := StateDir(env)
	if err := os.MkdirAll(sd, 0o777); err != nil {
		return DesktopStatus{}, err
	}
	if err := os.Chmod(sd, 0o700); err != nil {
		return DesktopStatus{}, err
	}
	unlock, err := flockPath(filepath.Join(sd, "start.lock"))
	if err != nil {
		return DesktopStatus{}, err
	}
	defer unlock()
	if _, ok := launcherPid(env); ok {
		published, err := PublishedEnv(env)
		if err != nil {
			return DesktopStatus{}, err
		}
		if published["DISPLAY"] != "" {
			return Status(env, "")
		}
	}
	floor, err := MinFreeMB(env)
	if err != nil {
		return DesktopStatus{}, err
	}
	mem := GetMemoryInfo()
	if blocker := MemoryBlocker(mem, floor); blocker != "" {
		return DesktopStatus{}, errors.New(blocker)
	}
	if mem.AvailableMB != nil && *mem.AvailableMB < TightHeadroomMB(floor) {
		slog.Warn(fmt.Sprintf("Bot Desktop starting with %d MB available; a browser with a few pages open can use most of that.", *mem.AvailableMB))
	}
	if _, ok := launcherPid(env); !ok {
		reapOrphanedServer(env, sd)
	}
	if err := os.MkdirAll(filepath.Dir(allocLock), 0o700); err != nil {
		return DesktopStatus{}, err
	}
	st, err := spawnAndWait(env, sd, waitSeconds)
	if err != nil {
		var re *launchError
		if errors.As(err, &re) {
			os.Remove(filepath.Join(sd, "display"))
		}
		return DesktopStatus{}, err
	}
	return st, nil
}

// A launch failure the start cleanup reacts to, the RuntimeError of the launch path
type launchError struct{ msg string }

func (e *launchError) Error() string { return e.msg }

// Seam over the launcher spawn so tests run a fake launcher
var launcherCommand = func(sd string) (*exec.Cmd, error) {
	path := filepath.Join(sd, "launcher.sh")
	if err := os.WriteFile(path, launcherScript, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(sd, "wallpaper.png"), wallpaperPNG, 0o600); err != nil {
		return nil, err
	}
	return exec.Command("bash", path), nil
}

// Spawns the launcher under the allocation lock until Xvnc claims its number, then waits for its publish
func spawnAndWait(env *tools.Env, sd string, waitSeconds float64) (DesktopStatus, error) {
	unlockAlloc, err := flockPath(allocLock)
	if err != nil {
		return DesktopStatus{}, err
	}
	released := false
	release := func() {
		if !released {
			released = true
			unlockAlloc()
		}
	}
	defer release()
	num, err := pickDisplay(env)
	if err != nil {
		return DesktopStatus{}, &launchError{err.Error()}
	}
	if err := os.WriteFile(filepath.Join(sd, "display"), []byte(fmt.Sprint(num)), 0o666); err != nil {
		return DesktopStatus{}, err
	}
	envFile := filepath.Join(sd, "env")
	os.Remove(envFile)
	childEnv, err := tools.ServedProfileChildEnv(envCtx(env), env, nil, "", false)
	if err != nil {
		return DesktopStatus{}, err
	}
	childEnv["HOME"] = childEnv["ICEBATH_REAL_HOME"]
	for _, k := range []string{"DISPLAY", "XAUTHORITY", "WAYLAND_DISPLAY", "DBUS_SESSION_BUS_ADDRESS", "SESSION_MANAGER"} {
		delete(childEnv, k)
	}
	geometry, err := Geometry(env)
	if err != nil {
		return DesktopStatus{}, err
	}
	limit, err := IdleStopSeconds(env)
	if err != nil {
		return DesktopStatus{}, err
	}
	zero := 0.0
	quoted := []string{}
	for _, a := range idleCommand() {
		quoted = append(quoted, pyshlex.Quote(a))
	}
	for k, v := range map[string]string{
		"ICEBATH_BD_PROFILE":          ProfileName(env),
		"ICEBATH_BD_DISPLAY_NUM":      fmt.Sprint(num),
		"ICEBATH_BD_SOCKET":           filepath.Join(sd, "rfb.sock"),
		"ICEBATH_BD_XAUTH":            filepath.Join(sd, "Xauthority"),
		"ICEBATH_BD_ENV_FILE":         envFile,
		"ICEBATH_BD_CONFIG_HOME":      filepath.Join(sd, "xdg"),
		"ICEBATH_BD_GEOMETRY":         geometry,
		"ICEBATH_BD_IDLE_CMD":         strings.Join(quoted, " "),
		"ICEBATH_BD_HOME":             envHome(env),
		"ICEBATH_BD_IDLE_FIRST_DELAY": fmt.Sprint(NextCheckIn(limit, &zero)),
	} {
		childEnv[k] = v
	}
	if exe, dir, ok := DockLaunch(env); ok {
		childEnv["ICEBATH_BD_BROWSER_EXEC"] = exe
		childEnv["ICEBATH_BD_BROWSER_EXEC_LINE"] = DockExecLine(exe, dir, nil)
	}
	logPath := filepath.Join(sd, "launcher.log")
	os.Remove(logPath)
	logf, err := os.Create(logPath)
	if err != nil {
		return DesktopStatus{}, err
	}
	cmd, err := launcherCommand(sd)
	if err != nil {
		logf.Close()
		return DesktopStatus{}, err
	}
	cmd.Env = make([]string, 0, len(childEnv))
	for k, v := range childEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = newSessionAttr()
	if err := cmd.Start(); err != nil {
		logf.Close()
		return DesktopStatus{}, err
	}
	logf.Close()
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	pid := cmd.Process.Pid
	born, ok := createTime(pid)
	bornS := "0"
	if ok {
		bornS = pyrepr.Float(born)
	}
	if err := os.WriteFile(filepath.Join(sd, "launcher.pid"), []byte(fmt.Sprintf("%d %s", pid, bornS)), 0o666); err != nil {
		return DesktopStatus{}, err
	}
	deadline := time.Now().Add(time.Duration(waitSeconds * float64(time.Second)))
	for time.Now().Before(deadline) {
		if _, ok := xLockPid(num); ok {
			release()
		}
		select {
		case <-exited:
			data, _ := os.ReadFile(logPath)
			return DesktopStatus{}, &launchError{fmt.Sprintf("Bot Desktop launcher exited with %d:\n%s", cmd.ProcessState.ExitCode(), tailText(data, 2000))}
		default:
		}
		_, envErr := os.Stat(envFile)
		_, sockErr := os.Stat(filepath.Join(sd, "rfb.sock"))
		if envErr == nil && sockErr == nil {
			slog.Info(fmt.Sprintf("Bot Desktop for profile %s up on :%d", ProfileName(env), num))
			TouchActivity(env)
			return Status(env, "")
		}
		time.Sleep(100 * time.Millisecond)
	}
	killGroupThenWait(&pid, pid, 2.0)
	<-exited
	for _, name := range []string{"launcher.pid", "env", "rfb.sock"} {
		os.Remove(filepath.Join(sd, name))
	}
	return DesktopStatus{}, &launchError{fmt.Sprintf("Bot Desktop did not publish its display within %.0fs (see %s)", waitSeconds, logPath)}
}

// Brings the screen up inside the terminal sandbox and records its published env on the host
func startInSandbox(env *tools.Env, waitSeconds float64) (DesktopStatus, error) {
	e, err := TerminalEnvironment(env, true)
	if err != nil {
		return DesktopStatus{}, err
	}
	if e == nil {
		return DesktopStatus{}, errors.New("the terminal backend's sandbox could not be started, so there is nowhere to put the screen")
	}
	sd := StateDir(env)
	if err := os.MkdirAll(sd, 0o777); err != nil {
		return DesktopStatus{}, err
	}
	if err := os.Chmod(sd, 0o700); err != nil {
		return DesktopStatus{}, err
	}
	published, err := func() (map[string]string, error) {
		unlock, err := flockPath(filepath.Join(sd, "start.lock"))
		if err != nil {
			return nil, err
		}
		defer unlock()
		exe, err := SandboxChromiumExecutable(e)
		if err != nil {
			return nil, err
		}
		line := ""
		if exe != "" {
			dir, err := BrowserProfileDir(e)
			if err != nil {
				return nil, err
			}
			bypass := true
			line = DockExecLine(exe, dir, &bypass)
		}
		geometry, err := Geometry(env)
		if err != nil {
			return nil, err
		}
		return SandboxStart(env, e, ProfileName(env), geometry, max(waitSeconds, sandboxLauncherWaitSecondsF), exe, line)
	}()
	if err != nil {
		return DesktopStatus{}, err
	}
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(published)) {
		b.WriteString(k + "=" + published[k] + "\n")
	}
	if err := os.WriteFile(filepath.Join(sd, "env"), []byte(b.String()), 0o666); err != nil {
		return DesktopStatus{}, err
	}
	TouchActivity(env)
	return Status(env, "")
}

// Published env of a sandbox hosted screen
func sandboxPublishedEnv(env *tools.Env) (map[string]string, error) {
	e, err := TerminalEnvironment(env, false)
	if err != nil || e == nil {
		return map[string]string{}, err
	}
	return SandboxPublishedEnv(e, ProfileName(env))
}

// Stops this profile screen, true when a launcher or an orphaned X server was signalled
func Stop(env *tools.Env, stillDue func() (bool, error)) (bool, error) {
	sandboxed := len(readMarker(env)) > 0
	if !sandboxed {
		where, err := Resolve(env)
		if err != nil {
			return false, err
		}
		sandboxed = where.Where == Terminal
	}
	if sandboxed {
		if stillDue != nil {
			due, err := stillDue()
			if err != nil || !due {
				return false, err
			}
		}
		e, err := ownedSandboxEnv(env)
		if err != nil {
			return false, err
		}
		stopped := false
		if e != nil {
			if stopped, err = SandboxStop(env, e, ProfileName(env)); err != nil {
				return false, err
			}
		}
		if err := os.Remove(markerPath(env)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		for _, name := range []string{"env", "activity"} {
			if err := os.Remove(filepath.Join(StateDir(env), name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return false, err
			}
		}
		return stopped, nil
	}
	if !IsSupportedHost() {
		return false, nil
	}
	sd := StateDir(env)
	if err := os.MkdirAll(sd, 0o777); err != nil {
		return false, err
	}
	unlock, err := flockPath(filepath.Join(sd, "start.lock"))
	if err != nil {
		return false, err
	}
	defer unlock()
	if stillDue != nil {
		due, err := stillDue()
		if err != nil {
			return false, err
		}
		if !due {
			slog.Info(fmt.Sprintf("Bot Desktop for profile %s changed while the idle stop waited; kept", ProfileName(env)))
			return false, nil
		}
	}
	return stopLocked(env, sd), nil
}

// Stops the live launcher group, or reaps the server a dead one left, under start.lock
func stopLocked(env *tools.Env, sd string) bool {
	pid, ok := launcherPid(env)
	if !ok {
		reaped := reapOrphanedServer(env, sd)
		os.Remove(filepath.Join(sd, "env"))
		return reaped
	}
	killGroupThenWait(&pid, pid, 5.0)
	for _, name := range []string{"launcher.pid", "env", "activity", "idle-watch.out"} {
		os.Remove(filepath.Join(sd, name))
	}
	return true
}
