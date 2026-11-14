package process

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/internal/hostinfo"
)

const (
	scopeProbeTTL          = 60 * time.Second
	minWorkerMemoryMax     = 64 * 1024 * 1024
	defaultWorkerMemoryMax = 1024 * 1024 * 1024
	workerMemoryMaxCap     = 4 * 1024 * 1024 * 1024
	busAddressVar          = "DBUS_SESSION_BUS_ADDRESS"
	runtimeDirVar          = "XDG_RUNTIME_DIR"
)

const cronDegradedConsequence = "cron children are dispatched as direct external subprocesses without restart-safe " +
	"cgroup isolation (killed if the gateway restarts mid-job). Set " +
	"cron.require_restart_safe_scope=true in config.yaml to fail closed instead."

const unitDegradedConsequence = "workers are spawned unmanaged inside this systemd unit's cgroup and will be KILLED when the " +
	"unit exits (Type=oneshot dispatch timers lose every worker within a second). Give the unit's " +
	"user a session bus (`loginctl enable-linger <user>`) so workers get their own scope, or set " +
	"KillMode=process on the unit."

type DispatchMode string

const (
	InProcess DispatchMode = "in_process"
	Scoped    DispatchMode = "scoped"
	Degraded  DispatchMode = "degraded"
)

type ChildDispatch struct {
	Mode DispatchMode
	Argv []string
}

type ScopeUnavailableError struct {
	Detail string
}

func (e *ScopeUnavailableError) Error() string {
	return "cannot create restart-safe systemd scope for gateway child: " + e.Detail
}

type Scopes struct {
	role     Role
	mu       sync.Mutex
	known    bool
	verdict  bool
	probedAt time.Time
	noExpand atomic.Bool
	warned   atomic.Bool
}

func NewScopes(role Role) (*Scopes, error) {
	if role == nil {
		return nil, unfilled("process.Role")
	}
	s := &Scopes{role: role}
	s.noExpand.Store(true)
	return s, nil
}

func workerMemoryMaxBytes() int64 {
	var override int64
	if raw := strings.TrimSpace(os.Getenv("TERMINAL_LOCAL_MEMORY_MAX_MB")); raw != "" {
		parsed := int64(-1)
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			parsed = n * 1024 * 1024
		}
		if parsed >= minWorkerMemoryMax {
			override = parsed
		} else {
			slog.Warn("Ignoring invalid TERMINAL_LOCAL_MEMORY_MAX_MB; expected an integer representing at least 64 MiB",
				"value", raw)
		}
	}
	var candidates []int64
	if cgroup, err := hostinfo.OwnCgroup(); err == nil {
		if mem, err := hostinfo.ReadCgroupMemory(cgroup); err == nil && mem.Max != nil && *mem.Max >= minWorkerMemoryMax {
			candidates = append(candidates, int64(*mem.Max))
		}
	}
	if mem, err := hostinfo.ReadMemory(); err == nil {
		candidates = append(candidates, min(int64(workerMemoryMaxCap), max(int64(minWorkerMemoryMax), int64(mem.Total/2))))
	}
	safe := int64(defaultWorkerMemoryMax)
	if len(candidates) > 0 {
		safe = slices.Min(candidates)
	}
	if override != 0 {
		return min(override, safe)
	}
	return safe
}

func (s *Scopes) scopeArgv(binary, unit string, argv ...string) []string {
	out := []string{binary, "--user", "--scope", "--quiet"}
	if s.noExpand.Load() {
		out = append(out, "--expand-environment=no")
	}
	out = append(out, "--unit", unit, "--collect",
		"--property", "MemoryAccounting=yes",
		"--property", "MemoryMax="+strconv.FormatInt(workerMemoryMaxBytes(), 10),
		"--")
	return append(out, argv...)
}

func UserBusEnv(base map[string]string) map[string]string {
	var env map[string]string
	if base == nil {
		env = environMap()
	} else {
		env = make(map[string]string, len(base)+2)
		for k, v := range base {
			env[k] = v
		}
	}
	return userBusEnv(env)
}

func environMap() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			env[k] = v
		}
	}
	return env
}

func envList(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

func (s *Scopes) cachedLocked() (bool, bool) {
	if !s.known || time.Since(s.probedAt) >= scopeProbeTTL {
		return false, false
	}
	return s.verdict, true
}

func (s *Scopes) available(ctx context.Context) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.cachedLocked(); ok {
		return v
	}
	available := false
	if isLinux {
		available = s.probe(ctx)
	}
	s.known, s.verdict, s.probedAt = true, available, time.Now()
	return available
}

func runCaptured(ctx context.Context, timeout time.Duration, env map[string]string, argv ...string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = envList(env)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && ctx.Err() == nil {
		return exit.ExitCode(), stderr.Bytes(), nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return 0, stderr.Bytes(), ctx.Err()
		}
		return 0, stderr.Bytes(), err
	}
	return 0, stderr.Bytes(), nil
}

func (s *Scopes) probe(ctx context.Context) bool {
	binary, err := exec.LookPath("systemd-run")
	if err != nil {
		return false
	}
	var tag [4]byte
	_, _ = rand.Read(tag[:])
	unit := "icebath-probe-scope-" + strconv.Itoa(os.Getpid()) + "-" + hex.EncodeToString(tag[:])
	var rc int
	var stderr []byte
	for range 2 {
		rc, stderr, err = runCaptured(ctx, 3*time.Second, UserBusEnv(nil), s.scopeArgv(binary, unit, "/bin/sh", "-c", "exit 0")...)
		if err != nil {
			slog.Debug("systemd-run --user --scope probe error", "err", err)
			return false
		}
		if !(rc != 0 && s.noExpand.Load() && bytes.Contains(stderr, []byte("expand-environment"))) {
			break
		}
		s.noExpand.Store(false)
	}
	if rc != 0 {
		slog.Debug("systemd-run --user --scope probe failed", "rc", rc, "stderr", strings.TrimSpace(strings.ToValidUTF8(string(stderr), "�")))
		return false
	}
	return true
}

func (s *Scopes) buildArgv(argv []string, unitSuffix string) []string {
	binary, err := exec.LookPath("systemd-run")
	if err != nil {
		return argv
	}
	return s.scopeArgv(binary, workerUnitTag+unitSuffix, argv...)
}

func (s *Scopes) warnDegraded(detail, consequence string) {
	if s.warned.Swap(true) {
		return
	}
	slog.Warn(detail + "; " + consequence)
}

func (s *Scopes) LostUserBus(spawnEnv map[string]string) bool {
	base := make(map[string]string, len(spawnEnv))
	for k, v := range spawnEnv {
		if k != busAddressVar {
			base[k] = v
		}
	}
	if _, ok := userBusEnv(base)[busAddressVar]; ok {
		return false
	}
	s.mu.Lock()
	s.known, s.verdict, s.probedAt = true, false, time.Now()
	s.mu.Unlock()
	return true
}

func (s *Scopes) GatewayChild(ctx context.Context, command []string, unitSuffix string, requireScope, outlivesParent bool) (ChildDispatch, error) {
	if !isLinux || os.Getenv("INVOCATION_ID") == "" {
		return ChildDispatch{Mode: InProcess, Argv: command}, nil
	}
	supervised := s.role.SupervisedGateway()
	if !supervised && !outlivesParent {
		return ChildDispatch{Mode: InProcess, Argv: command}, nil
	}
	degrade := func(detail string) (ChildDispatch, error) {
		if supervised {
			if requireScope {
				return ChildDispatch{}, &ScopeUnavailableError{Detail: detail}
			}
			s.warnDegraded("managed gateway: "+detail, cronDegradedConsequence)
		} else {
			s.warnDegraded("systemd unit dispatch: "+detail, unitDegradedConsequence)
		}
		return ChildDispatch{Mode: Degraded, Argv: command}, nil
	}
	if !s.available(ctx) {
		return degrade("systemd-run --user --scope is unavailable (usually no reachable user D-Bus session at " +
			"/run/user/" + strconv.Itoa(os.Getuid()) + "/bus). On a system-level service install, run " +
			"`sudo loginctl enable-linger <gateway-user>` and restart the gateway.")
	}
	scoped := s.buildArgv(command, unitSuffix)
	if slices.Equal(scoped, command) {
		return degrade("systemd-run disappeared after the availability probe")
	}
	return ChildDispatch{Mode: Scoped, Argv: scoped}, nil
}

func stopUnit(ctx context.Context, unit string) bool {
	binary, err := exec.LookPath("systemctl")
	if err != nil {
		return false
	}
	rc, stderr, err := runCaptured(ctx, 15*time.Second, UserBusEnv(nil), binary, "--user", "stop", unit)
	if err != nil {
		slog.Debug("systemctl --user stop failed", "unit", unit, "err", err)
		return false
	}
	if rc != 0 {
		text := strings.TrimSpace(strings.ToValidUTF8(string(stderr), "�"))
		lower := strings.ToLower(text)
		for _, marker := range []string{"not loaded", "not found", "does not exist"} {
			if strings.Contains(lower, marker) {
				return true
			}
		}
		slog.Debug("systemctl --user stop exited nonzero", "unit", unit, "rc", rc, "stderr", text)
		return false
	}
	return true
}
