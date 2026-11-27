package terminal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/nickheyer/icebath/internal/config"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// How long the answer of whether scopes can be made holds before it is asked again
	scopeProbeTTL = 60 * time.Second
	// Memory a scope may use: the least taken, the most the machine's half may give, and the default when nothing is known
	scopeMemoryMin     = 64 << 20
	scopeMemoryCap     = 4 << 30
	scopeMemoryDefault = 1 << 30
)

// Whether background commands go in transient scopes of the user's systemd, and how systemd-run is called for them
type scopes struct {
	mu        sync.Mutex
	available *bool
	probed    time.Time
	// systemd-run of this machine knows --expand-environment, which keeps it from expanding the command's $ words itself
	expand bool
}

func newScopes() *scopes {
	return &scopes{expand: true}
}

// Whether the background commands of this process go in scopes of their own: it serves others and systemd runs it, so a restart of its
// unit would kill every command left in the unit's cgroup, and scopes can be made
func (r *Jobs) scoped(label string) bool {
	if !r.outlive || os.Getenv("INVOCATION_ID") == "" {
		return false
	}
	if r.scopes.ready() {
		return true
	}
	slog.Debug(label + " background executor not isolated in a systemd scope (systemd-run --user unavailable); worker shares the server's cgroup.")
	return false
}

// Whether systemd-run --user --scope can make a cgroup here, probed at most once a minute
func (s *scopes) ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.available != nil && time.Since(s.probed) < scopeProbeTTL {
		return *s.available
	}
	available := false
	if binary, err := exec.LookPath("systemd-run"); err == nil {
		unit := fmt.Sprintf("icebath-probe-scope-%d-%s", os.Getpid(), strs.Random(4))
		for attempt := 0; attempt < 2; attempt++ {
			code, stderr := probeScope(s.argv(binary, unit, scopeMemoryMin, "/bin/sh", "-c", "exit 0"))
			if code != 0 && s.expand && strings.Contains(stderr, "expand-environment") {
				s.expand = false
				continue
			}
			available = code == 0
			if !available {
				slog.Debug(fmt.Sprintf("systemd-run --user --scope probe failed (rc=%d): %s", code, strings.TrimSpace(stderr)))
			}
			break
		}
	}
	s.available, s.probed = &available, time.Now()
	return available
}

// Runs the probe within three seconds, answering its exit code and what it wrote on stderr
func probeScope(argv []string) (int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = userBus(os.Environ())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			return exit.ExitCode(), stderr.String()
		}
		return -1, err.Error()
	}
	return 0, stderr.String()
}

// Argv of systemd-run putting the rest in a transient scope of the unit name, with memory accounting and the cap
func (s *scopes) argv(binary, unit string, memory int64, argv ...string) []string {
	out := []string{binary, "--user", "--scope", "--quiet"}
	if s.expand {
		out = append(out, "--expand-environment=no")
	}
	out = append(out, "--unit", unit, "--collect", "--property", "MemoryAccounting=yes", "--property", fmt.Sprintf("MemoryMax=%d", memory), "--")
	return append(out, argv...)
}

// Command wrapped in a scope of its own, its unit named after the suffix and recorded on the job, with the environment that reaches the
// user's systemd; unchanged when this process puts no command in a scope
func (r *Jobs) scope(cfg *config.Config, j *Job, command Command, suffix, label string) Command {
	if !r.scoped(label) {
		return command
	}
	binary, err := exec.LookPath("systemd-run")
	if err != nil {
		return command
	}
	r.scopes.mu.Lock()
	argv := r.scopes.argv(binary, "icebath-worker-"+suffix, scopeMemory(cfg), command.Argv...)
	r.scopes.mu.Unlock()
	j.unit = "icebath-worker-" + suffix + ".scope"
	command.Argv, command.Env = argv, userBus(command.Env)
	return command
}

// Memory cap of a scope: the least of this process's own cgroup limit and half the machine's memory within bounds, the profile's cap
// below that when it sets one
func scopeMemory(cfg *config.Config) int64 {
	var override int64
	if mb := cfg.GetInt(terminalconfig.LocalMemoryMax); mb > 0 {
		if bytes := int64(mb) << 20; bytes >= scopeMemoryMin {
			override = bytes
		} else {
			slog.Warn(fmt.Sprintf("Ignoring %s=%d; it must be at least %d MiB", terminalconfig.LocalMemoryMax, mb, scopeMemoryMin>>20))
		}
	}
	var candidates []int64
	if limit, ok := cgroupMemory(); ok && limit >= scopeMemoryMin {
		candidates = append(candidates, limit)
	}
	var info unix.Sysinfo_t
	if unix.Sysinfo(&info) == nil {
		candidates = append(candidates, min(scopeMemoryCap, max(scopeMemoryMin, int64(info.Totalram)*int64(info.Unit)/2)))
	}
	safe := int64(scopeMemoryDefault)
	if len(candidates) > 0 {
		safe = candidates[0]
		for _, candidate := range candidates[1:] {
			safe = min(safe, candidate)
		}
	}
	if override > 0 {
		return min(override, safe)
	}
	return safe
}

// memory.max of this process's cgroup v2, false when it can't be read or is unlimited
func cgroupMemory() (int64, bool) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		relative, ok := strings.CutPrefix(line, "0::")
		if !ok {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", strings.TrimLeft(relative, "/"), "memory.max"))
		if err != nil {
			return 0, false
		}
		n, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(string(raw), "\ufeff")), 10, 64)
		return n, err == nil && n >= 0
	}
	return 0, false
}

// Environment with XDG_RUNTIME_DIR and the session bus that reach this user's systemd, even one that lingers with no login of its own
func userBus(env []string) []string {
	values := map[string]string{}
	for _, entry := range env {
		if name, value, ok := strings.Cut(entry, "="); ok {
			values[name] = value
		}
	}
	runtime := values["XDG_RUNTIME_DIR"]
	if runtime == "" || !privateDir(runtime) {
		if runtime = fmt.Sprintf("/run/user/%d", os.Getuid()); !privateDir(runtime) {
			return env
		}
	}
	bus := filepath.Join(runtime, "bus")
	if !ownSocket(bus) {
		return env
	}
	values["XDG_RUNTIME_DIR"], values["DBUS_SESSION_BUS_ADDRESS"] = runtime, "unix:path="+bus
	return Environ(values)
}

// Whether the folder is this user's and no one else may write in it
func privateDir(path string) bool {
	st, err := os.Lstat(path)
	if err != nil || !filepath.IsAbs(path) || !st.IsDir() {
		return false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(sys.Uid) == os.Getuid() && st.Mode().Perm()&0o022 == 0
}

// Whether the path is a socket of this user
func ownSocket(path string) bool {
	st, err := os.Lstat(path)
	if err != nil {
		return false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && st.Mode()&os.ModeSocket != 0 && int(sys.Uid) == os.Getuid()
}

// Stops a transient scope by its unit name, true once it stopped or was already gone
func stopUnit(unit string) bool {
	binary, err := exec.LookPath("systemctl")
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--user", "stop", unit)
	cmd.Env = userBus(os.Environ())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err == nil {
		return true
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		text := strings.TrimSpace(stderr.String())
		lower := strings.ToLower(text)
		for _, marker := range []string{"not loaded", "not found", "does not exist"} {
			if strings.Contains(lower, marker) {
				return true
			}
		}
		slog.Debug(fmt.Sprintf("systemctl --user stop %s exited %d: %s", unit, exit.ExitCode(), text))
		return false
	}
	slog.Debug(fmt.Sprintf("systemctl --user stop %s failed: %v", unit, err))
	return false
}
