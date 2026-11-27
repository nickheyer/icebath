package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/botdesktop"
	"github.com/nickheyer/icebath/internal/browser"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/terminal"
)

const (
	sandboxLaunch    = 60 * time.Second
	sandboxConfirmed = 30 * time.Second
	sandboxGone      = 3
	sandboxNotOurs   = 4
	startOfP         = `s=$(cat /proc/"$p"/stat 2>/dev/null) && s=${s##*")"} && set -- $s && printf %s "${20}"`
	sandboxStop      = `p=$1; t=$(` + startOfP + `); [ -n "$t" ] && [ "$t" = "$2" ] || exit 4; ` +
		`c=; while IFS= read -r -d "" a; do c="$c $a"; done < /proc/"$p"/cmdline; case "$c " in *" $3 "*) ;; *) exit 4 ;; esac; kill -TERM "$p"`
)

// Sandbox Chromium this process launched under a shared key, its start empty when it could not be read
type sandboxed struct {
	env        terminal.Environment
	pid        int
	start, dir string
}

// Endpoint found alive through a forward, kept so the next look need not ask the sandbox
type confirmation struct {
	env      terminal.Environment
	port     int
	failures int
	at       time.Time
}

type sandboxes struct {
	mu        sync.Mutex
	launched  map[string]sandboxed
	locks     map[string]*sync.Mutex
	confirmed map[string]confirmation
}

func newSandboxes() sandboxes {
	return sandboxes{launched: map[string]sandboxed{}, locks: map[string]*sync.Mutex{}, confirmed: map[string]confirmation{}}
}

// DevTools port and browser path of DevToolsActivePort text
func parseDevtools(out []byte) (int, string, bool) {
	lines := strings.Split(strings.TrimSpace(strings.ToValidUTF8(string(out), "�")), "\n")
	if len(lines) < 2 || !strings.HasPrefix(strings.TrimSpace(lines[1]), "/devtools/browser/") {
		return 0, "", false
	}
	port, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	return port, strings.TrimSpace(lines[1]), err == nil && port > 0
}

func (r *registrant) runIn(env terminal.Environment, user string, argv []string, vars map[string]string, timeout time.Duration, stdin []byte) (*terminal.RunResult, error) {
	return terminal.RunIn(context.Background(), env, argv, vars, user, timeout, stdin)
}

type liveness int

const (
	unknown liveness = iota
	gone
	live
)

// One exec saying where the sandbox's browser listens: gone when the sandbox says none runs, unknown when it could not tell
func (r *registrant) liveDevtools(env terminal.Environment, user, dir string) (int, string, liveness) {
	active := terminal.Quote(dir + "/DevToolsActivePort")
	script := fmt.Sprintf(`[ -s %s ] || exit %d; p=$(head -n1 %s); ( exec 3<>"/dev/tcp/127.0.0.1/$p" ) 2>/dev/null || exit %d; cat %s`, active,
		sandboxGone, active, sandboxGone, active)
	out, err := r.runIn(env, user, []string{"bash", "-c", script}, nil, 10*time.Second, nil)
	if err != nil {
		slog.Debug("sandbox browser liveness probe failed", "error", err)
		return 0, "", unknown
	}
	if out.Code == sandboxGone {
		return 0, "", gone
	}
	if out.Code == 0 {
		if port, path, ok := parseDevtools(out.Stdout); ok {
			return port, path, live
		}
	}
	slog.Debug(fmt.Sprintf("sandbox browser liveness probe inconclusive (exit %d)", out.Code))
	return 0, "", unknown
}

// Key every session on the sandbox's Chromium shares, by its environment and profile folder, never by profile
func sandboxKey(env terminal.Environment, dir string) string {
	return fmt.Sprintf("sandbox:%p:%s", env, dir)
}

func (r *registrant) sandboxLock(key string) *sync.Mutex {
	r.sandbox.mu.Lock()
	defer r.sandbox.mu.Unlock()
	l, ok := r.sandbox.locks[key]
	if !ok {
		l = &sync.Mutex{}
		r.sandbox.locks[key] = l
	}
	return l
}

// Launches Chromium detached in the sandbox, owned under the key from the moment it exists
func (r *registrant) spawnSandbox(cfg *config.Config, env terminal.Environment, user, dir, key string, published map[string]string) error {
	exe, err := r.desktop.SandboxChromium(env)
	if err != nil {
		return err
	}
	if exe == "" {
		return errors.New("the sandbox image has no Chromium to run the browser")
	}
	vars := maps.Clone(published)
	vars["TMPDIR"] = env.TempDir()
	if err := r.desktop.SeedSandboxProfile(env, dir); err != nil {
		return errors.New("could not write the preferences that keep the sandbox browser from calling home: " + err.Error())
	}
	argv := chromiumArgv(cfg, exe, dir, false, true, false, append(slices.Clone(botdesktop.SharedArgs), browser.BypassArgs...))
	seen := map[string]bool{}
	var quoted []string
	for _, a := range argv {
		if !seen[a] {
			seen[a] = true
			quoted = append(quoted, terminal.Quote(a))
		}
	}
	q := terminal.Quote(dir)
	script := fmt.Sprintf(`mkdir -p %s && rm -f %s/DevToolsActivePort && (setsid %s >/dev/null 2>&1 & p=$!; echo "$p $(%s)")`, q, q,
		strings.Join(quoted, " "), startOfP)
	out, err := r.runIn(env, user, []string{"bash", "-c", script}, vars, 30*time.Second, nil)
	if err != nil {
		return err
	}
	var words []string
	if out.Code == 0 {
		if lines := strings.Split(strings.TrimSpace(strings.ToValidUTF8(string(out.Stdout), "�")), "\n"); len(lines) > 0 {
			words = strings.Fields(lines[len(lines)-1])
		}
	}
	pid := 0
	if len(words) > 0 {
		pid, _ = strconv.Atoi(words[0])
	}
	if pid <= 0 {
		stderr := []rune(strings.ToValidUTF8(string(out.Stderr), "�"))
		if len(stderr) > 500 {
			stderr = stderr[len(stderr)-500:]
		}
		return errors.New("Chromium could not be started in the sandbox: " + string(stderr))
	}
	start := ""
	if len(words) > 1 {
		if _, err := strconv.Atoi(words[1]); err == nil {
			start = words[1]
		}
	}
	r.sandbox.mu.Lock()
	r.sandbox.launched[key] = sandboxed{env: env, pid: pid, start: start, dir: dir}
	r.sandbox.mu.Unlock()
	return nil
}

func (r *registrant) waitSandbox(env terminal.Environment, user, dir string) (int, string, error) {
	for deadline := time.Now().Add(sandboxLaunch); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		out, err := r.runIn(env, user, []string{"cat", dir + "/DevToolsActivePort"}, nil, 10*time.Second, nil)
		if err != nil && !strings.HasPrefix(err.Error(), "command timed out") {
			return 0, "", err
		}
		if err == nil && out.Code == 0 {
			if port, path, ok := parseDevtools(out.Stdout); ok {
				return port, path, nil
			}
		}
	}
	return 0, "", errors.New("Chromium in the sandbox did not expose a DevTools endpoint in time")
}

// Session on the sandbox's Chromium, attached to the one that runs or launched, through a forward of this machine
func (r *registrant) createSandbox(ctx context.Context, cfg *config.Config, key string) (*session, error) {
	lease := r.terminal.Held(cfg, "", "")
	if lease == nil {
		return nil, errors.New("the terminal backend's sandbox is not running, so there is nowhere to run the browser")
	}
	defer lease.Release()
	env := lease.Env
	published, err := r.desktop.Published(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if published["DISPLAY"] == "" {
		return nil, errors.New("the screen inside the terminal backend's sandbox is gone; start it again")
	}
	user, err := r.desktop.User(env)
	if err != nil {
		return nil, err
	}
	dir, err := r.desktop.SandboxProfile(env)
	if err != nil {
		return nil, err
	}
	shared := sandboxKey(env, dir)
	lock := r.sandboxLock(shared)
	lock.Lock()
	port, path, state := r.liveDevtools(env, user, dir)
	if state == unknown {
		lock.Unlock()
		return nil, errors.New("the terminal backend's sandbox did not answer whether its browser is running; try again")
	}
	local, err := func() (int, error) {
		if state == gone {
			if err := r.spawnSandbox(cfg, env, user, dir, shared, published); err != nil {
				return 0, err
			}
			var err error
			if port, path, err = r.waitSandbox(env, user, dir); err != nil {
				r.stopLaunched(shared)
				return 0, err
			}
		}
		forwarded, err := r.terminal.ForwardPort(env, port, "", user)
		if err != nil && state == gone {
			r.stopLaunched(shared)
		}
		return forwarded, err
	}()
	lock.Unlock()
	if err != nil {
		return nil, err
	}
	s := newSession("sb", fmt.Sprintf("ws://127.0.0.1:%d%s", local, path), map[string]bool{"local": true, "sandbox": true})
	r.confirm(s.cdp, env, port)
	s.shared, s.launcher = true, shared
	slog.Info(fmt.Sprintf("Created sandbox browser session %s for %s", s.name, key))
	return s, nil
}

func (r *registrant) confirm(cdp string, env terminal.Environment, port int) {
	failures := r.terminal.ForwardFailures(env, port, "")
	r.sandbox.mu.Lock()
	r.sandbox.confirmed[cdp] = confirmation{env, port, failures, time.Now()}
	r.sandbox.mu.Unlock()
}

// Whether the sandbox's Chromium still serves the session's endpoint through the current forward; true when that can't be told
func (r *registrant) sandboxAlive(ctx context.Context, cfg *config.Config, s *session) bool {
	lease := r.terminal.Held(cfg, "", "")
	if lease == nil {
		return false
	}
	defer lease.Release()
	env := lease.Env
	r.sandbox.mu.Lock()
	confirmed, ok := r.sandbox.confirmed[s.cdp]
	r.sandbox.mu.Unlock()
	if ok && confirmed.env == env && r.terminal.ForwardFailures(env, confirmed.port, "") == confirmed.failures && time.Since(confirmed.at) < sandboxConfirmed {
		return true
	}
	user, err := r.desktop.User(env)
	var dir string
	if err == nil {
		dir, err = r.desktop.SandboxProfile(env)
	}
	if err != nil {
		slog.Debug("sandbox browser liveness probe failed", "error", err)
		return true
	}
	port, path, state := r.liveDevtools(env, user, dir)
	if state == unknown {
		return true
	}
	dead := state == gone
	if !dead {
		forwarded, err := r.terminal.ForwardPort(env, port, "", user)
		dead = err != nil || s.cdp != fmt.Sprintf("ws://127.0.0.1:%d%s", forwarded, path)
	}
	if dead {
		r.sandbox.mu.Lock()
		delete(r.sandbox.confirmed, s.cdp)
		r.sandbox.mu.Unlock()
		return false
	}
	r.confirm(s.cdp, env, port)
	return true
}

// SIGTERMs the sandbox Chromium this process launched on the session's shared key
func (r *registrant) stopSandbox(_ context.Context, _ *config.Config, s *session) {
	r.sandbox.mu.Lock()
	delete(r.sandbox.confirmed, s.cdp)
	r.sandbox.mu.Unlock()
	lock := r.sandboxLock(s.launcher)
	lock.Lock()
	defer lock.Unlock()
	r.stopLaunched(s.launcher)
}

// Signals the launched browser only while its pid still has the start time and profile of the launch
func (r *registrant) stopLaunched(key string) {
	r.sandbox.mu.Lock()
	launched, ok := r.sandbox.launched[key]
	delete(r.sandbox.launched, key)
	r.sandbox.mu.Unlock()
	if !ok {
		return
	}
	if launched.start == "" {
		slog.Warn(fmt.Sprintf("browser: leaving sandbox Chromium pid %d running: its start time could not be read at launch, so the pid cannot be "+
			"proven to still be that browser", launched.pid))
		return
	}
	user, err := r.desktop.User(launched.env)
	var out *terminal.RunResult
	if err == nil {
		out, err = r.runIn(launched.env, user, []string{"bash", "-c", sandboxStop, "icebath-sandbox-browser-stop", strconv.Itoa(launched.pid), launched.start,
			"--user-data-dir=" + launched.dir}, nil, 15*time.Second, nil)
	}
	switch {
	case err != nil:
		slog.Debug(fmt.Sprintf("sandbox browser stop for pid %d failed: %s", launched.pid, err))
	case out.Code == sandboxNotOurs:
		slog.Info(fmt.Sprintf("browser: not signalling sandbox pid %d: it is gone or no longer the Chromium launched on %s", launched.pid, launched.dir))
	case out.Code != 0:
		slog.Debug(fmt.Sprintf("sandbox browser stop for pid %d exited %d", launched.pid, out.Code))
	}
}
