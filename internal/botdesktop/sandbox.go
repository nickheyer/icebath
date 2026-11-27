package botdesktop

import (
	"bytes"
	"cmp"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/browser"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/terminal/sandbox"
)

// Account the desktop and browser run as inside a sandbox image that has it
const DesktopUser = "pn"

// Programs a sandbox needs for a screen
var sandboxRequired = []string{"Xvnc", "xfwm4", "xfce4-panel", "xfdesktop", "xfsettingsd", "dbus-run-session", "xauth", "xdpyinfo", "xprop"}

//go:embed launcher.sh
var launcherScript []byte

//go:embed wallpaper.png
var wallpaperPNG []byte

// What a sandbox is known to have, probed once for each environment
type facts struct {
	mu       sync.Mutex
	user     *string
	home     string
	chromium *string
}

func (d *Desktop) factsOf(env terminal.Environment) *facts {
	f, _ := d.facts.LoadOrStore(env, &facts{})
	return f.(*facts)
}

// Folder of the profile's screen in the sandbox
func remoteDir(env terminal.Environment, profile string) string {
	return strings.TrimRight(cmp.Or(env.TempDir(), "/tmp"), "/") + "/icebath-bot-desktop/" + profile
}

func (d *Desktop) runIn(env terminal.Environment, argv []string, vars map[string]string, timeout time.Duration, stdin []byte) (*terminal.RunResult, error) {
	user, err := d.user(env)
	if err != nil {
		return nil, err
	}
	return terminal.RunIn(context.Background(), env, argv, vars, user, timeout, stdin)
}

// Desktop user when the sandbox has it, empty for the default one of its exec
func (d *Desktop) user(env terminal.Environment) (string, error) {
	f := d.factsOf(env)
	f.mu.Lock()
	cached := f.user
	f.mu.Unlock()
	if cached != nil {
		return *cached, nil
	}
	probe, err := terminal.RunIn(context.Background(), env, []string{"id", "-u", DesktopUser}, nil, "", 15*time.Second, nil)
	if err != nil {
		return "", err
	}
	user := ""
	if probe.Code == 0 {
		user = DesktopUser
	}
	f.mu.Lock()
	f.user = &user
	f.mu.Unlock()
	return user, nil
}

// Desktop user of the sandbox, the account its browser and screen run as
func (d *Desktop) User(env terminal.Environment) (string, error) { return d.user(env) }

func text(b []byte) string { return strings.ToValidUTF8(string(b), "�") }

// Home of the desktop user inside the sandbox, where state outliving a container stop goes
func (d *Desktop) home(env terminal.Environment) (string, error) {
	f := d.factsOf(env)
	f.mu.Lock()
	cached := f.home
	f.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	user, err := d.user(env)
	if err != nil {
		return "", err
	}
	out, err := d.runIn(env, []string{"bash", "-c", `printf %s "$HOME"`}, nil, 15*time.Second, nil)
	if err != nil {
		return "", err
	}
	home := ""
	if out.Code == 0 {
		home = strings.TrimSpace(text(out.Stdout))
	}
	if !strings.HasPrefix(home, "/") {
		home = "/root"
		if user != "" {
			home = "/home/" + DesktopUser
		}
	}
	f.mu.Lock()
	f.home = home
	f.mu.Unlock()
	return home, nil
}

// The one user data dir the agent's browser and the dock icon share inside the sandbox
func (d *Desktop) SandboxProfile(env terminal.Environment) (string, error) {
	home, err := d.home(env)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(home, "/") + "/.icebath/bot-desktop/browser-profile", nil
}

const chromiumProbe = `for d in "${PLAYWRIGHT_BROWSERS_PATH:-}" /opt/playwright "$HOME/.cache/ms-playwright"; do` +
	` [ -n "$d" ] || continue;` +
	` for c in "$d"/chromium-*/chrome-linux/chrome "$d"/chromium-*/chrome-linux64/chrome; do` +
	` [ -x "$c" ] && { printf %s "$c"; exit 0; }; done; done; exit 1`

// Playwright Chromium inside the sandbox, empty when its image has none
func (d *Desktop) SandboxChromium(env terminal.Environment) (string, error) {
	f := d.factsOf(env)
	f.mu.Lock()
	cached := f.chromium
	f.mu.Unlock()
	if cached != nil {
		return *cached, nil
	}
	out, err := d.runIn(env, []string{"bash", "-c", chromiumProbe}, nil, 20*time.Second, nil)
	if err != nil {
		return "", err
	}
	exe := ""
	if out.Code == 0 {
		exe = strings.TrimSpace(text(out.Stdout))
	}
	f.mu.Lock()
	f.chromium = &exe
	f.mu.Unlock()
	return exe, nil
}

// Writes the quiet preferences into a browser profile inside the sandbox, as the desktop user
func (d *Desktop) SeedSandboxProfile(env terminal.Environment, dir string) error {
	return browser.SeedRemote(dir, true, func(argv []string, stdin []byte) ([]byte, int, error) {
		out, err := d.runIn(env, argv, nil, 30*time.Second, stdin)
		if err != nil {
			return nil, 0, err
		}
		return out.Stdout, out.Code, nil
	})
}

// Desktop programs the sandbox lacks
func (d *Desktop) sandboxMissing(env terminal.Environment) ([]string, error) {
	script := "for b in " + strings.Join(sandboxRequired, " ") + `; do command -v "$b" >/dev/null 2>&1 || echo "$b"; done`
	out, err := d.runIn(env, []string{"bash", "-c", script}, nil, 20*time.Second, nil)
	if err != nil {
		return nil, err
	}
	return strings.Fields(text(out.Stdout)), nil
}

// KEY=VALUE lines, both sides trimmed, lines without an equals sign skipped
func parseEnv(raw string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == '\r' }) {
		if k, v, ok := strings.Cut(line, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// Variables the live launcher in the folder published, none while it is down
func (d *Desktop) sandboxPublished(env terminal.Environment, dir string) (map[string]string, error) {
	q := terminal.Quote(dir)
	out, err := d.runIn(env, []string{"bash", "-c", "kill -0 $(cat " + q + "/launcher.pid 2>/dev/null) 2>/dev/null && test -S " + q + "/rfb.sock && cat " +
		q + "/env"}, nil, 15*time.Second, nil)
	if err != nil {
		return nil, err
	}
	if out.Code != 0 {
		return map[string]string{}, nil
	}
	return parseEnv(text(out.Stdout)), nil
}

func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return text(b)
}

// How a plugin's sandbox gets the desktop stack, empty for a backend icebath ships
func imageAdvice(backend string) string {
	switch backend {
	case "", "local", "docker", "singularity", "modal", "daytona", "ssh", "vercel_sandbox":
		return ""
	}
	return "give the " + backend + " plugin's sandbox the desktop stack through the plugin's own image setting (the " + sandbox.Image +
		" image icebath builds has it)"
}

// Brings the screen up inside the sandbox, a live one kept, answering the variables it published or why it could not
func (d *Desktop) sandboxStart(cfg *config.Config, env terminal.Environment, profile, geometry string, wait time.Duration, exec, line string) (map[string]string, error) {
	dir := remoteDir(env, profile)
	live, err := d.sandboxPublished(env, dir)
	if err != nil {
		return nil, err
	}
	if live["DISPLAY"] != "" {
		return live, d.mark(cfg, env, dir, profile, live)
	}
	if _, err := d.sandboxStop(cfg, env, profile); err != nil {
		return nil, err
	}
	missing, err := d.sandboxMissing(env)
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		advice := cmp.Or(imageAdvice(env.Backend()), "use "+sandbox.Image+" (the sandbox icebath builds, desktop stack included) as "+
			"terminal.docker_image / modal_image")
		return nil, fmt.Errorf("Bot Desktop needs %s inside the terminal backend's sandbox. The configured image does not have them; %s, or set "+
			"bot_desktop.placement: gateway.", strings.Join(missing, ", "), advice)
	}
	q := terminal.Quote(dir)
	seed := "set -e; rm -rf " + q + "; mkdir -p " + q + "; cd " + q + ";" +
		" echo " + terminal.Quote(base64.StdEncoding.EncodeToString(launcherScript)) + " | base64 -d > launcher.sh;" +
		" echo " + terminal.Quote(base64.StdEncoding.EncodeToString(wallpaperPNG)) + " | base64 -d > wallpaper.png;" +
		" chmod 0700 . launcher.sh"
	out, err := d.runIn(env, []string{"bash", "-c", seed}, nil, 30*time.Second, nil)
	if err != nil {
		return nil, err
	}
	if out.Code != 0 {
		return nil, errors.New("could not seed the sandbox desktop dir: " + tail(out.Stderr, 500))
	}
	vars := map[string]string{
		"ICEBATH_BD_PROFILE": profile, "ICEBATH_BD_DISPLAY_NUM": "20", "ICEBATH_BD_SOCKET": dir + "/rfb.sock", "ICEBATH_BD_XAUTH": dir + "/Xauthority",
		"ICEBATH_BD_ENV_FILE": dir + "/env", "ICEBATH_BD_CONFIG_HOME": dir + "/xdg", "ICEBATH_BD_GEOMETRY": geometry, "ICEBATH_BD_WALLPAPER": dir + "/wallpaper.png",
	}
	if exec != "" && line != "" {
		vars["ICEBATH_BD_BROWSER_EXEC"], vars["ICEBATH_BD_BROWSER_EXEC_LINE"] = exec, line
	}
	spawn := "cd " + q + " && setsid -f bash -c 'echo $$ > launcher.pid; exec bash launcher.sh' > launcher.log 2>&1 < /dev/null; " +
		"for _ in 1 2 3 4 5 6 7 8 9 10; do [ -s launcher.pid ] && break; sleep 0.1; done; [ -s launcher.pid ]"
	if out, err = d.runIn(env, []string{"bash", "-c", spawn}, vars, 30*time.Second, nil); err != nil {
		return nil, err
	}
	if out.Code != 0 {
		return nil, errors.New("sandbox desktop launcher did not start: " + tail(out.Stderr, 500))
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		got, err := d.sandboxPublished(env, dir)
		if err != nil {
			return nil, err
		}
		if got["DISPLAY"] != "" {
			slog.Info(fmt.Sprintf("Bot Desktop for profile %s up inside %s on %s", profile, env.Backend(), got["DISPLAY"]))
			return got, d.mark(cfg, env, dir, profile, got)
		}
		time.Sleep(250 * time.Millisecond)
	}
	log, err := d.runIn(env, []string{"tail", "-c", "2000", dir + "/launcher.log"}, nil, 10*time.Second, nil)
	if err != nil {
		return nil, err
	}
	if _, err := d.sandboxStop(cfg, env, profile); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("sandbox desktop did not publish its display within %.0fs:\n%s", wait.Seconds(), text(log.Stdout))
}

// Host side record of the sandbox the screen lives in: its display, folder and profile, and the container that holds it
type marker struct {
	Display   string `json:"display"`
	Dir       string `json:"dir"`
	Profile   string `json:"profile"`
	Backend   string `json:"backend"`
	Container string `json:"container,omitempty"`
	Docker    string `json:"docker,omitempty"`
}

func markerPath(cfg *config.Config) string { return filepath.Join(StateDir(cfg), "sandbox.json") }

// Marker of the profile, nil when there is none or it can't be read
func readMarker(cfg *config.Config) *marker {
	raw, err := os.ReadFile(markerPath(cfg))
	if err != nil {
		return nil
	}
	var m marker
	if json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &m) != nil {
		return nil
	}
	return &m
}

// Writes the marker of the screen just found up in the sandbox
func (d *Desktop) mark(cfg *config.Config, env terminal.Environment, dir, profile string, live map[string]string) error {
	m := marker{Display: live["DISPLAY"], Dir: dir, Profile: profile, Backend: env.Backend()}
	if boxed, ok := env.(terminal.Boxed); ok {
		m.Container, m.Docker = boxed.Box()
		if m.Container == "" {
			m.Docker = ""
		}
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(StateDir(cfg), 0o700); err != nil {
		return err
	}
	return os.WriteFile(markerPath(cfg), data, 0o600)
}

func dropMarker(cfg *config.Config) error {
	if err := os.Remove(markerPath(cfg)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// A container answer kept five seconds
type aliveEntry struct {
	at    time.Time
	alive bool
}

// Whether the sandbox the marker names still exists: a container asked of its program, five seconds at a time, any other kind taken as
// alive
func (d *Desktop) markerAlive(m *marker) bool {
	if m.Container == "" {
		return true
	}
	d.mu.Lock()
	cached, ok := d.alive[m.Container]
	d.mu.Unlock()
	if ok && time.Since(cached.at) < 5*time.Second {
		return cached.alive
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, cmp.Or(m.Docker, "docker"), "inspect", "-f", "{{.State.Running}}", m.Container).Output()
	alive := err == nil && strings.TrimSpace(string(out)) == "true"
	d.mu.Lock()
	d.alive[m.Container] = aliveEntry{time.Now(), alive}
	d.mu.Unlock()
	return alive
}

// Kills the launcher's session and every process holding the profile's folder in the sandbox, whether something was live
func (d *Desktop) sandboxStop(cfg *config.Config, env terminal.Environment, profile string) (bool, error) {
	dir := remoteDir(env, profile)
	q, pattern := terminal.Quote(dir), terminal.Quote(dir+"/")
	script := "\nlive=0\np=$(cat " + q + "/launcher.pid 2>/dev/null)\n" +
		"if [ -n \"$p\" ] && kill -0 \"$p\" 2>/dev/null; then live=1; kill -TERM -- -\"$p\" 2>/dev/null || kill -TERM \"$p\" 2>/dev/null; fi\n" +
		"for pid in $(pgrep -f -- " + pattern + " 2>/dev/null); do [ \"$pid\" != \"$$\" ] && { live=1; kill -TERM \"$pid\" 2>/dev/null; }; done\n" +
		"sleep 0.5\n" +
		"for pid in $(pgrep -f -- " + pattern + " 2>/dev/null); do [ \"$pid\" != \"$$\" ] && kill -KILL \"$pid\" 2>/dev/null; done\n" +
		"rm -f " + q + "/env " + q + "/launcher.pid " + q + "/rfb.sock\n" +
		"[ \"$live\" = 1 ]\n"
	out, err := d.runIn(env, []string{"bash", "-c", script}, nil, 30*time.Second, nil)
	if err != nil {
		return false, err
	}
	if err := dropMarker(cfg); err != nil {
		return false, err
	}
	return out.Code == 0, nil
}

// Program grabbing the sandbox's screen inside it, shrinking it into a box and writing one JPEG to stdout
const grabScript = `
import io, sys
from PIL import ImageGrab
img = ImageGrab.grab(xdisplay=sys.argv[1])
img.thumbnail((int(sys.argv[2]), int(sys.argv[3])))
buf = io.BytesIO()
img.convert("RGB").save(buf, "JPEG", quality=int(sys.argv[4]), optimize=True)
sys.stdout.buffer.write(buf.getvalue())
`

// One JPEG of the sandbox's screen grabbed inside it, where the X socket and its cookie live; nil when the grab fails
func (d *Desktop) sandboxGrab(env terminal.Environment, published map[string]string, width, height, quality int) []byte {
	display := published["DISPLAY"]
	if display == "" {
		return nil
	}
	vars := map[string]string{}
	for _, k := range []string{"DISPLAY", "XAUTHORITY"} {
		if v, ok := published[k]; ok {
			vars[k] = v
		}
	}
	out, err := d.runIn(env, []string{"python3", "-c", grabScript, display, fmt.Sprint(width), fmt.Sprint(height), fmt.Sprint(quality)}, vars,
		20*time.Second, nil)
	if err != nil {
		slog.Debug("sandbox screen grab failed", "error", err)
		return nil
	}
	if out.Code != 0 || !bytes.HasPrefix(out.Stdout, []byte{0xff, 0xd8}) {
		slog.Debug("sandbox screen grab failed: " + tail(out.Stderr, 300))
		return nil
	}
	return out.Stdout
}

// Command of this machine running argv as the desktop user on the sandbox's screen with the variables, which name what it is for in a
// refusal
func (d *Desktop) DisplayCommand(env terminal.Environment, vars map[string]string, argv []string, what string) ([]string, error) {
	user, err := d.user(env)
	if err != nil {
		return nil, err
	}
	full := terminal.RemoteCommand(env, argv, vars, user, true)
	if full == nil {
		return nil, fmt.Errorf("%s cannot %s", env.Backend(), what)
	}
	return full, nil
}
