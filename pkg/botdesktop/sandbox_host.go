package botdesktop

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydeadline"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/tools"
)

// DesktopUser is the account the desktop and browser run as inside a sandbox image that has it
const DesktopUser = "pn"

// SandboxImageHint names the published image carrying the desktop stack
const SandboxImageHint = "nousresearch/hermes-sandbox:desktop"

var sandboxRequired = []string{"Xvnc", "xfwm4", "xfce4-panel", "xfdesktop", "xfsettingsd", "dbus-run-session", "xauth", "xdpyinfo", "xprop"}

//go:embed launcher.sh
var launcherScript []byte

//go:embed wallpaper.png
var wallpaperPNG []byte

const rfbRelay = "import os,socket,sys,threading\n" +
	"s=socket.socket(socket.AF_UNIX);s.connect(sys.argv[1])\n" +
	"def up():\n" +
	"  while True:\n" +
	"    d=os.read(0,65536)\n" +
	"    if not d: break\n" +
	"    s.sendall(d)\n" +
	"  s.shutdown(socket.SHUT_WR)\n" +
	"threading.Thread(target=up,daemon=True).start()\n" +
	"while True:\n" +
	"  d=s.recv(65536)\n" +
	"  if not d: break\n" +
	"  os.write(1,d)\n"

// Python class name of a terminal environment as hermes logs and records it
func envClassName(e tools.Environment) string {
	if c, ok := e.(interface{ ClassName() string }); ok {
		return c.ClassName()
	}
	switch e.EnvType() {
	case "docker":
		return "DockerEnvironment"
	case "ssh":
		return "SSHEnvironment"
	case "local":
		return "LocalEnvironment"
	case "modal":
		return "ModalEnvironment"
	case "managed_modal":
		return "ManagedModalEnvironment"
	case "daytona":
		return "DaytonaEnvironment"
	case "vercel_sandbox":
		return "VercelSandboxEnvironment"
	}
	return fmt.Sprintf("%T", e)
}

// Backend name a plugin environment was built for, empty for a built in one
func pluginBackendName(e tools.Environment) string {
	name := e.EnvType()
	if slices.Contains(tools.BuiltinTerminalBackends(), name) {
		return ""
	}
	return name
}

// How a plugin sandbox gets the desktop stack, empty for a built in backend
func PluginImageAdvice(backend string) string {
	if backend == "" || slices.Contains(tools.BuiltinTerminalBackends(), backend) {
		return ""
	}
	return fmt.Sprintf("give the %s plugin's sandbox the desktop stack through the plugin's own image setting (%s has it)", backend, SandboxImageHint)
}

// Host side record of the sandbox owning the screen
func markerPath(env *tools.Env) string {
	return filepath.Join(envHome(env), "bot-desktop", "sandbox.json")
}

// Parsed marker, empty when absent or unreadable
func readMarker(env *tools.Env) map[string]any {
	b, err := os.ReadFile(markerPath(env))
	if err != nil {
		return map[string]any{}
	}
	v, err := pyjson.Loads(strings.TrimPrefix(string(b), "\ufeff"))
	if err != nil {
		return map[string]any{}
	}
	if m, ok := pyjson.Plain(v).(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// Marker fields naming the sandbox, so a restarted gateway can tell a live one from a removed one
func ownerIdentity(e tools.Environment) *pyjson.Object {
	ident := pyjson.NewObject()
	ident.Set("backend", envClassName(e))
	if c, ok := e.(interface{ ContainerID() string }); ok && c.ContainerID() != "" {
		ident.Set("container", c.ContainerID())
		docker := "docker"
		if d, ok := e.(interface{ DockerExe() string }); ok {
			docker = d.DockerExe()
		}
		ident.Set("docker", docker)
	}
	return ident
}

type aliveEntry struct {
	at    time.Time
	alive bool
}

var (
	aliveMu    sync.Mutex
	aliveCache = map[string]aliveEntry{}
	monoNow    = time.Now
)

// Whether the sandbox a marker names still exists, docker inspect cached five seconds, others alive
func MarkerSandboxAlive(marker map[string]any) bool {
	container, _ := marker["container"].(string)
	if container == "" {
		return true
	}
	now := monoNow()
	aliveMu.Lock()
	cached, ok := aliveCache[container]
	aliveMu.Unlock()
	if ok && now.Sub(cached.at) < 5*time.Second {
		return cached.alive
	}
	docker, _ := marker["docker"].(string)
	if docker == "" {
		docker = "docker"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, docker, "inspect", "-f", "{{.State.Running}}", container)
	cmd.Stdin = nil
	out, err := cmd.Output()
	alive := err == nil && strings.TrimSpace(string(out)) == "true"
	aliveMu.Lock()
	aliveCache[container] = aliveEntry{now, alive}
	aliveMu.Unlock()
	return alive
}

// Per environment probe results hermes caches as attributes on the env object
var envCache sync.Map

type envFacts struct {
	mu       sync.Mutex
	user     *string
	home     string
	chromium *string
}

func factsOf(e tools.Environment) *envFacts {
	v, _ := envCache.LoadOrStore(e, &envFacts{})
	return v.(*envFacts)
}

// Remote directory of the profile screen under the sandbox temp dir
func remoteDir(e tools.Environment, profile string) string {
	return strings.TrimRight(tools.SandboxTempDir(e), "/") + "/icebath-bot-desktop/" + profile
}

// Runs argv inside the sandbox as user with a timeout in seconds
func runIn(e tools.Environment, argv []string, childEnv map[string]string, user string, timeoutS float64) (*tools.RunInResult, error) {
	return tools.RunIn(context.Background(), e, argv, childEnv, user, time.Duration(timeoutS*float64(time.Second)), nil)
}

// Desktop user when the sandbox has it, empty for the exec default, cached per env
func userFor(e tools.Environment) (string, error) {
	f := factsOf(e)
	f.mu.Lock()
	cached := f.user
	f.mu.Unlock()
	if cached != nil {
		return *cached, nil
	}
	probe, err := runIn(e, []string{"id", "-u", DesktopUser}, nil, "", 15)
	if err != nil {
		return "", err
	}
	user := ""
	if probe.ReturnCode == 0 {
		user = DesktopUser
	}
	f.mu.Lock()
	f.user = &user
	f.mu.Unlock()
	return user, nil
}

// Desktop user of the sandbox for callers outside the package
func UserFor(e tools.Environment) (string, error) { return userFor(e) }

// HOME of the desktop user inside the sandbox, where state outliving a container stop goes
func DesktopHome(e tools.Environment) (string, error) {
	f := factsOf(e)
	f.mu.Lock()
	cached := f.home
	f.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	user, err := userFor(e)
	if err != nil {
		return "", err
	}
	proc, err := runIn(e, []string{"bash", "-c", `printf %s "$HOME"`}, nil, user, 15)
	if err != nil {
		return "", err
	}
	h := ""
	if proc.ReturnCode == 0 {
		h = strings.TrimSpace(strings.ToValidUTF8(string(proc.Stdout), "\ufffd"))
	}
	if h == "" || !strings.HasPrefix(h, "/") {
		h = "/root"
		if user != "" {
			h = "/home/" + DesktopUser
		}
	}
	f.mu.Lock()
	f.home = h
	f.mu.Unlock()
	return h, nil
}

// The one user data dir the agent browser and the dock icon share inside the sandbox
func BrowserProfileDir(e tools.Environment) (string, error) {
	h, err := DesktopHome(e)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(h, "/") + "/.icebath/bot-desktop/browser-profile", nil
}

const chromiumProbe = `for d in "${PLAYWRIGHT_BROWSERS_PATH:-}" /opt/playwright "$HOME/.cache/ms-playwright"; do` +
	` [ -n "$d" ] || continue;` +
	` for c in "$d"/chromium-*/chrome-linux/chrome "$d"/chromium-*/chrome-linux64/chrome; do` +
	` [ -x "$c" ] && { printf %s "$c"; exit 0; }; done; done; exit 1`

// Playwright Chromium inside the sandbox, empty when the image has none, cached per env
func SandboxChromiumExecutable(e tools.Environment) (string, error) {
	f := factsOf(e)
	f.mu.Lock()
	cached := f.chromium
	f.mu.Unlock()
	if cached != nil {
		return *cached, nil
	}
	user, err := userFor(e)
	if err != nil {
		return "", err
	}
	proc, err := runIn(e, []string{"bash", "-c", chromiumProbe}, nil, user, 20)
	if err != nil {
		return "", err
	}
	exe := ""
	if proc.ReturnCode == 0 {
		exe = strings.TrimSpace(strings.ToValidUTF8(string(proc.Stdout), "\ufffd"))
	}
	f.mu.Lock()
	f.chromium = &exe
	f.mu.Unlock()
	return exe, nil
}

const grabScript = `
import io, sys
from PIL import ImageGrab
img = ImageGrab.grab(xdisplay=sys.argv[1])
img.thumbnail((int(sys.argv[2]), int(sys.argv[3])))
buf = io.BytesIO()
img.convert("RGB").save(buf, "JPEG", quality=int(sys.argv[4]), optimize=True)
sys.stdout.buffer.write(buf.getvalue())
`

// One JPEG of the sandbox screen grabbed inside the sandbox, nil when the grab fails
func GrabJPEG(e tools.Environment, published map[string]string, maxW, maxH, quality int) ([]byte, error) {
	display := published["DISPLAY"]
	if display == "" {
		return nil, nil
	}
	childEnv := map[string]string{}
	for _, k := range []string{"DISPLAY", "XAUTHORITY"} {
		if v, ok := published[k]; ok {
			childEnv[k] = v
		}
	}
	user, err := userFor(e)
	if err != nil {
		return nil, err
	}
	proc, err := runIn(e, []string{"python3", "-c", grabScript, display, fmt.Sprint(maxW), fmt.Sprint(maxH), fmt.Sprint(quality)}, childEnv, user, 20)
	if err != nil {
		slog.Debug(fmt.Sprintf("sandbox screen grab failed: %s", err))
		return nil, nil
	}
	if proc.ReturnCode != 0 || !bytes.HasPrefix(proc.Stdout, []byte{0xff, 0xd8}) {
		tail := strings.ToValidUTF8(string(proc.Stderr), "\ufffd")
		if r := []rune(tail); len(r) > 300 {
			tail = string(r[len(r)-300:])
		}
		slog.Debug(fmt.Sprintf("sandbox screen grab failed: %s", tail))
		return nil, nil
	}
	return proc.Stdout, nil
}

// Desktop binaries the sandbox lacks
func SandboxMissingBinaries(e tools.Environment) ([]string, error) {
	script := "for b in " + strings.Join(sandboxRequired, " ") + `; do command -v "$b" >/dev/null 2>&1 || echo "$b"; done`
	user, err := userFor(e)
	if err != nil {
		return nil, err
	}
	proc, err := runIn(e, []string{"bash", "-c", script}, nil, user, 20)
	if err != nil {
		return nil, err
	}
	return strings.Fields(strings.ToValidUTF8(string(proc.Stdout), "\ufffd")), nil
}

// Parses KEY=VALUE lines, stripping both sides and skipping lines without an equals sign
func parseEnvLines(text string) map[string]string {
	out := map[string]string{}
	for _, line := range splitLines(text) {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// Python str.splitlines over the line breaks env files use
func splitLines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// Env the live launcher in rdir published, empty when it is down
func sandboxPublished(e tools.Environment, rdir string) (map[string]string, error) {
	q := pyshlex.Quote(rdir)
	user, err := userFor(e)
	if err != nil {
		return nil, err
	}
	proc, err := runIn(e, []string{"bash", "-c", "kill -0 $(cat " + q + "/launcher.pid 2>/dev/null) 2>/dev/null && test -S " + q + "/rfb.sock && cat " + q + "/env"}, nil, user, 15)
	if err != nil {
		return nil, err
	}
	if proc.ReturnCode != 0 {
		return map[string]string{}, nil
	}
	return parseEnvLines(strings.ToValidUTF8(string(proc.Stdout), "\ufffd")), nil
}

// Published env of the profile screen inside the sandbox
func SandboxPublishedEnv(e tools.Environment, profile string) (map[string]string, error) {
	return sandboxPublished(e, remoteDir(e, profile))
}

// Last n bytes of b decoded with replacement
func tailText(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return strings.ToValidUTF8(string(b), "\ufffd")
}

// Brings the screen up inside the sandbox, idempotent, returning the published env or the blocker
func SandboxStart(env *tools.Env, e tools.Environment, profile, geometry string, waitSeconds float64, browserExec, browserExecLine string) (map[string]string, error) {
	rdir := remoteDir(e, profile)
	live, err := sandboxPublished(e, rdir)
	if err != nil {
		return nil, err
	}
	if live["DISPLAY"] != "" {
		return recordMarker(env, e, rdir, profile, live)
	}
	if _, err := SandboxStop(env, e, profile); err != nil {
		return nil, err
	}
	missing, err := SandboxMissingBinaries(e)
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		advice := PluginImageAdvice(pluginBackendName(e))
		if advice == "" {
			advice = fmt.Sprintf("use %s (the default sandbox base plus the desktop stack) as terminal.docker_image / modal_image", SandboxImageHint)
		}
		return nil, fmt.Errorf("Bot Desktop needs %s inside the terminal backend's sandbox. The configured image "+
			"does not have them; %s, or set bot_desktop.placement: gateway.", strings.Join(missing, ", "), advice)
	}
	user, err := userFor(e)
	if err != nil {
		return nil, err
	}
	q := pyshlex.Quote(rdir)
	seed := "set -e; rm -rf " + q + "; mkdir -p " + q + "; cd " + q + ";" +
		" echo " + pyshlex.Quote(base64.StdEncoding.EncodeToString(launcherScript)) + " | base64 -d > launcher.sh;" +
		" echo " + pyshlex.Quote(base64.StdEncoding.EncodeToString(wallpaperPNG)) + " | base64 -d > wallpaper.png;" +
		" chmod 0700 . launcher.sh"
	proc, err := runIn(e, []string{"bash", "-c", seed}, nil, user, 30)
	if err != nil {
		return nil, err
	}
	if proc.ReturnCode != 0 {
		return nil, fmt.Errorf("could not seed the sandbox desktop dir: %s", tailText(proc.Stderr, 500))
	}
	num := 20
	childEnv := map[string]string{
		"ICEBATH_BD_PROFILE": profile, "ICEBATH_BD_DISPLAY_NUM": fmt.Sprint(num), "ICEBATH_BD_SOCKET": rdir + "/rfb.sock",
		"ICEBATH_BD_XAUTH": rdir + "/Xauthority", "ICEBATH_BD_ENV_FILE": rdir + "/env",
		"ICEBATH_BD_CONFIG_HOME": rdir + "/xdg", "ICEBATH_BD_GEOMETRY": geometry, "ICEBATH_BD_WALLPAPER": rdir + "/wallpaper.png",
	}
	if browserExec != "" && browserExecLine != "" {
		childEnv["ICEBATH_BD_BROWSER_EXEC"] = browserExec
		childEnv["ICEBATH_BD_BROWSER_EXEC_LINE"] = browserExecLine
	}
	spawn := "cd " + q + " && setsid -f bash -c " +
		"'echo $$ > launcher.pid; exec bash launcher.sh' > launcher.log 2>&1 < /dev/null; " +
		"for _ in 1 2 3 4 5 6 7 8 9 10; do [ -s launcher.pid ] && break; sleep 0.1; done; [ -s launcher.pid ]"
	proc, err = runIn(e, []string{"bash", "-c", spawn}, childEnv, user, 30)
	if err != nil {
		return nil, err
	}
	if proc.ReturnCode != 0 {
		return nil, fmt.Errorf("sandbox desktop launcher did not start: %s", tailText(proc.Stderr, 500))
	}
	var pollErr error
	live = pydeadline.PollUntil(func() (map[string]string, bool) {
		got, err := sandboxPublished(e, rdir)
		if err != nil {
			pollErr = err
			return nil, true
		}
		if got["DISPLAY"] != "" {
			return got, true
		}
		return nil, false
	}, time.Duration(waitSeconds*float64(time.Second)), 250*time.Millisecond)
	if pollErr != nil {
		return nil, pollErr
	}
	if live != nil {
		slog.Info(fmt.Sprintf("Bot Desktop for profile %s up inside %s on %s", profile, envClassName(e), live["DISPLAY"]))
		return recordMarker(env, e, rdir, profile, live)
	}
	tail, err := runIn(e, []string{"tail", "-c", "2000", rdir + "/launcher.log"}, nil, user, 10)
	if err != nil {
		return nil, err
	}
	if _, err := SandboxStop(env, e, profile); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("sandbox desktop did not publish its display within %.0fs:\n%s", waitSeconds, strings.ToValidUTF8(string(tail.Stdout), "\ufffd"))
}

// Writes the host side marker naming the display, dir, profile and owning sandbox
func recordMarker(env *tools.Env, e tools.Environment, rdir, profile string, live map[string]string) (map[string]string, error) {
	path := markerPath(env)
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return nil, err
	}
	m := pyjson.NewObject()
	m.Set("display", live["DISPLAY"])
	m.Set("dir", rdir)
	m.Set("profile", profile)
	ident := ownerIdentity(e)
	for _, k := range ident.Keys {
		v, _ := ident.Get(k)
		m.Set(k, v)
	}
	text, err := pyjson.Dumps(m)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(text), 0o666); err != nil {
		return nil, err
	}
	return live, nil
}

// Kills the launcher session and every process holding the profile dir, true when something was live
func SandboxStop(env *tools.Env, e tools.Environment, profile string) (bool, error) {
	rdir := remoteDir(e, profile)
	q := pyshlex.Quote(rdir)
	pat := pyshlex.Quote(rdir + "/")
	script := "\nlive=0\np=$(cat " + q + "/launcher.pid 2>/dev/null)\n" +
		"if [ -n \"$p\" ] && kill -0 \"$p\" 2>/dev/null; then live=1; kill -TERM -- -\"$p\" 2>/dev/null || kill -TERM \"$p\" 2>/dev/null; fi\n" +
		"for pid in $(pgrep -f -- " + pat + " 2>/dev/null); do [ \"$pid\" != \"$$\" ] && { live=1; kill -TERM \"$pid\" 2>/dev/null; }; done\n" +
		"sleep 0.5\n" +
		"for pid in $(pgrep -f -- " + pat + " 2>/dev/null); do [ \"$pid\" != \"$$\" ] && kill -KILL \"$pid\" 2>/dev/null; done\n" +
		"rm -f " + q + "/env " + q + "/launcher.pid " + q + "/rfb.sock\n" +
		"[ \"$live\" = 1 ]\n"
	user, err := userFor(e)
	if err != nil {
		return false, err
	}
	proc, err := runIn(e, []string{"bash", "-c", script}, nil, user, 30)
	if err != nil {
		return false, err
	}
	if err := os.Remove(markerPath(env)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return proc.ReturnCode == 0, nil
}

// Process whose stdin and stdout carry the RFB bytes of the sandbox Xvnc
func SandboxOpenRFBStream(e tools.Environment, profile string) (*tools.StreamProcess, error) {
	user, err := userFor(e)
	if err != nil {
		return nil, err
	}
	return tools.OpenStream(e, []string{"python3", "-c", rfbRelay, remoteDir(e, profile) + "/rfb.sock"}, nil, user, nil)
}

// Command and args running cua-driver mcp on the sandbox display through the backend exec prefix
func CuaMCPInvocation(e tools.Environment, profile string, published map[string]string) (string, []string, error) {
	user, err := userFor(e)
	if err != nil {
		return "", nil, err
	}
	argv := tools.RemoteCommand(e, []string{"cua-driver", "mcp", "--no-overlay"}, published, user, true)
	if argv == nil {
		return "", nil, fmt.Errorf("%s cannot host cua-driver", envClassName(e))
	}
	return argv[0], argv[1:], nil
}
