package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/proc"
)

const (
	lightpandaHint      = "Install Lightpanda from https://lightpanda.io/docs/run-locally/installation/one-liner and make sure `lightpanda` is on PATH"
	lightpandaCacheFlag = "--http-cache-dir"
	// Variable that stops Lightpanda sending its usage telemetry
	lightpandaTelemetry = "LIGHTPANDA_DISABLE_TELEMETRY"
	lightpandaReady     = 10 * time.Second
)

// Standard folders a PATH is completed with when they exist
var sanePath = []string{"/opt/homebrew/bin", "/opt/homebrew/sbin", "/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"}

// One lightpanda serve of a browser session, its CDP discovery on a loopback port
type lightpandaServer struct {
	session string
	port    int
	proc    *child
	log     string
	start   *int64
}

func (s *lightpandaServer) cdp() string { return "http://127.0.0.1:" + strconv.Itoa(s.port) }

func (s *lightpandaServer) alive() bool {
	_, done := s.proc.poll()
	return !done
}

// Servers this process started by session name, and whether a binary takes the HTTP cache flag
type lightpandas struct {
	mu      sync.Mutex
	order   []string
	servers map[string]*lightpandaServer
	probed  map[string]bool
}

// PATH with the standard folders that exist added after it
func completePath(existing string) string {
	parts := slices.DeleteFunc(strings.Split(existing, string(os.PathListSeparator)), func(p string) bool { return p == "" })
	out := slices.Clone(parts)
	for _, d := range sanePath {
		if st, err := os.Stat(d); err == nil && st.IsDir() && !slices.Contains(parts, d) {
			out = append(out, d)
		}
	}
	return strings.Join(out, string(os.PathListSeparator))
}

// Lightpanda on PATH and the standard folders, then where its installer puts it; empty on Windows, where it has no build
func findLightpanda(cfg *config.Config) string {
	if runtime.GOOS == "windows" {
		return ""
	}
	for _, dir := range filepath.SplitList(completePath(os.Getenv("PATH"))) {
		if candidate := filepath.Join(dir, "lightpanda"); fsx.Executable(candidate) {
			return candidate
		}
	}
	home, _ := os.UserHomeDir()
	for _, candidate := range []string{filepath.Join(home, ".lightpanda", "lightpanda"), filepath.Join(home, ".local", "bin", "lightpanda"),
		filepath.Join(profileDir(cfg), "bin", "lightpanda")} {
		if fsx.Executable(candidate) {
			return candidate
		}
	}
	return ""
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// Folder of the session records and logs, made when missing
func lightpandaState(cfg *config.Config) (string, error) {
	// A process that never took a profile up, such as one whose saved profile is gone, keeps nothing: its folder would be the profiles'
	if cfg.Profile() == "" {
		return "", errors.New("No profile is in use, so no lightpanda records are kept.")
	}
	path := filepath.Join(profileDir(cfg), "cache", "browser-use", "lightpanda")
	return path, os.MkdirAll(path, 0o700)
}

// Whether lightpanda serve takes the HTTP cache flag, read once per binary from its help
func (l *lightpandas) cache(binary string) bool {
	l.mu.Lock()
	supported, known := l.probed[binary]
	l.mu.Unlock()
	if known {
		return supported
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "help").CombinedOutput()
	if _, exited := errors.AsType[*exec.ExitError](err); err != nil && !exited || ctx.Err() != nil {
		slog.Debug(fmt.Sprintf("lightpanda http-cache probe failed (%v); assuming no", err))
	}
	supported = ctx.Err() == nil && strings.Contains(string(out), lightpandaCacheFlag)
	l.mu.Lock()
	l.probed[binary] = supported
	l.mu.Unlock()
	return supported
}

// Last line of the child's log that has something on it
func logLast(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(data) > 2000 {
		data = data[len(data)-2000:]
	}
	lines := strings.Split(strings.TrimSpace(strings.ToValidUTF8(string(data), "�")), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return lines[i]
		}
	}
	return ""
}

// Ends a live child, killing it when it lingers five seconds
func end(p *child) {
	if _, done := p.poll(); done {
		return
	}
	if err := ask(p.cmd.Process); err != nil {
		slog.Debug("lightpanda terminate failed", "error", err)
		return
	}
	if !p.wait(5 * time.Second) {
		p.cmd.Process.Kill()
	}
}

// Record of a lightpanda serve, by which a later process reaps one whose owner is gone
type lightpandaRecord struct {
	PID     int     `json:"pid"`
	Port    int     `json:"port"`
	Owner   int     `json:"owner_pid"`
	Start   *int64  `json:"start_time"`
	Started float64 `json:"started_at"`
}

// Starts lightpanda serve on a free loopback port, answering it once json/version does; private networks blocked when asked
func (r *registrant) launchLightpanda(ctx context.Context, cfg *config.Config, session string, blockPrivate bool) (*lightpandaServer, string) {
	binary := findLightpanda(cfg)
	if binary == "" {
		if runtime.GOOS == "windows" {
			return nil, "browser.engine is 'lightpanda' but Lightpanda has no Windows build. Set browser.engine to auto (or run icebath under WSL2)."
		}
		return nil, "browser.engine is 'lightpanda' but no lightpanda binary was found on PATH, ~/.lightpanda or ~/.local/bin. " + lightpandaHint +
			", or set browser.engine to auto."
	}
	port, err := freePort()
	if err != nil {
		return nil, err.Error()
	}
	state, err := lightpandaState(cfg)
	if err != nil {
		return nil, err.Error()
	}
	argv := []string{binary, "serve", "--host", "127.0.0.1", "--port", strconv.Itoa(port)}
	if r.lightpanda.cache(binary) {
		cache := filepath.Join(state, "http-cache")
		if err := os.MkdirAll(cache, 0o700); err != nil {
			return nil, err.Error()
		}
		argv = append(argv, lightpandaCacheFlag, cache)
	}
	if blockPrivate {
		argv = append(argv, "--block-private-networks")
	}
	logPath := filepath.Join(state, session+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Sprintf("Failed to launch lightpanda serve (%s): %s", binary, err)
	}
	env := r.childEnv(ctx, cfg)
	env[lightpandaTelemetry] = "true"
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = terminal.Environ(env)
	cmd.Stderr = logFile
	cmd.SysProcAttr = detached()
	err = cmd.Start()
	logFile.Close()
	if err != nil {
		return nil, fmt.Sprintf("Failed to launch lightpanda serve (%s): %s", binary, err)
	}
	p := &child{cmd: cmd, done: make(chan struct{})}
	go func() {
		cmd.Wait()
		p.code = proc.ExitCode(cmd.ProcessState)
		close(p.done)
	}()
	address := "http://127.0.0.1:" + strconv.Itoa(port)
	for deadline := time.Now().Add(lightpandaReady); ; time.Sleep(100 * time.Millisecond) {
		if rc, done := p.poll(); done {
			msg := fmt.Sprintf("lightpanda serve exited with code %d before %s/json/version answered", rc, address)
			if tail := logLast(logPath); tail != "" {
				msg += ": " + tail
			}
			return nil, msg
		}
		if debugReady(ctx, address, 200*time.Millisecond) {
			break
		}
		if !time.Now().Before(deadline) {
			end(p)
			detail := ""
			if tail := logLast(logPath); tail != "" {
				detail = " (last stderr line: " + tail + ")"
			}
			return nil, fmt.Sprintf("lightpanda serve did not expose %s/json/version within %ds%s", address, int(lightpandaReady.Seconds()), detail)
		}
	}
	server := &lightpandaServer{session: session, port: port, proc: p, log: logPath, start: startOf(cmd.Process.Pid)}
	rec := lightpandaRecord{PID: cmd.Process.Pid, Port: port, Owner: os.Getpid(), Start: server.start, Started: float64(time.Now().UnixNano()) / 1e9}
	if err := writeState(filepath.Join(state, session+".json"), rec); err != nil {
		slog.Debug("could not write lightpanda record for "+session, "error", err)
	}
	r.lightpanda.mu.Lock()
	if _, ok := r.lightpanda.servers[session]; !ok {
		r.lightpanda.order = append(r.lightpanda.order, session)
	}
	r.lightpanda.servers[session] = server
	r.lightpanda.mu.Unlock()
	slog.Info(fmt.Sprintf("Started lightpanda serve (pid %d, port %d) for session %s", cmd.Process.Pid, port, session))
	return server, ""
}

func (l *lightpandas) get(session string) *lightpandaServer {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.servers[session]
}

func (l *lightpandas) pop(session string) *lightpandaServer {
	l.mu.Lock()
	defer l.mu.Unlock()
	server := l.servers[session]
	if server != nil {
		delete(l.servers, session)
		l.order = slices.DeleteFunc(l.order, func(n string) bool { return n == session })
	}
	return server
}

// Stops the session's server by its tree, plainly when that fails, and drops its record
func (r *registrant) stopLightpanda(cfg *config.Config, session string) {
	server := r.lightpanda.pop(session)
	if server != nil && server.alive() {
		var start int64
		if server.start != nil {
			start = *server.start
		}
		r.terminal.Terminate(cfg, server.proc.cmd.Process.Pid, start, false)
		if !server.proc.wait(5 * time.Second) {
			end(server.proc)
		}
	}
	if state, err := lightpandaState(cfg); err == nil {
		if err := os.Remove(filepath.Join(state, session+".json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("could not remove lightpanda record for "+session, "error", err)
		}
	}
	if server != nil {
		slog.Debug("Stopped lightpanda serve for session " + session)
	}
}

// Stops every server this process started
func (r *registrant) stopAllLightpanda() {
	r.lightpanda.mu.Lock()
	sessions := slices.Clone(r.lightpanda.order)
	r.lightpanda.mu.Unlock()
	for _, session := range sessions {
		r.stopLightpanda(r.config, session)
	}
}

// Whether the pid is verifiably the lightpanda serve recorded on that port with that start time
func isLightpanda(pid, port int, start *int64) bool {
	argv := terminal.Cmdline(pid)
	if len(argv) == 0 || !strings.Contains(strings.ToLower(filepath.Base(argv[0])), "lightpanda") || !slices.Contains(argv, "serve") ||
		!slices.Contains(argv, strconv.Itoa(port)) {
		return false
	}
	return start == nil || sameStart(startOf(pid), start)
}

// Kills the lightpanda serve processes of the profile whose owner is gone, never one of a live owner, and answers how many
func (r *registrant) reapLightpanda(cfg *config.Config) int {
	state, err := lightpandaState(cfg)
	if err != nil {
		return 0
	}
	entries, err := os.ReadDir(state)
	if err != nil {
		return 0
	}
	reaped := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path, session := filepath.Join(state, e.Name()), strings.TrimSuffix(e.Name(), ".json")
		var rec lightpandaRecord
		if !loadState(path, &rec) {
			continue
		}
		if rec.Owner == os.Getpid() {
			if r.lightpanda.get(session) != nil {
				continue
			}
		} else if rec.Owner > 0 && terminal.Running(rec.Owner) {
			continue
		}
		if rec.PID > 0 && isLightpanda(rec.PID, rec.Port, rec.Start) {
			var start int64
			if rec.Start != nil {
				start = *rec.Start
			}
			r.terminal.Terminate(cfg, rec.PID, start, false)
			reaped++
			slog.Info(fmt.Sprintf("Reaped orphaned lightpanda serve pid %d (session %s)", rec.PID, session))
		}
		os.Remove(path)
	}
	return reaped
}

// Whether Lightpanda is the engine in effect, and the setting that shadows it when it is not
func (r *registrant) lightpandaStatus(cfg *config.Config) (bool, string) {
	if !lightpanda(cfg) {
		return false, ""
	}
	if r.overrideRaw(cfg) != "" {
		return false, "a CDP override is active (/browser connect or browser.cdp_url)"
	}
	if realProfile(cfg) {
		return false, "browser.use_real_profile is on (Lightpanda cannot load a Chromium profile)"
	}
	if cloud, err := r.browser.Cloud(cfg); err == nil && cloud != nil {
		return false, "cloud provider " + cloud.Title + " is selected (browser.cloud_provider, or auto-detected from credentials)"
	}
	if r.browserUseByKey(cfg) {
		return false, "Browser Use cloud (BROWSER_USE_API_KEY) is selected"
	}
	return true, "icebath spawns `lightpanda serve` per browser session"
}
