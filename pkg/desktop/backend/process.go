package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Child is a spawned backend process the desktop owns.
type Child interface {
	Pid() int
	Killed() bool
	Exited() bool
	Kill(signal string) error
	WaitExit(timeout time.Duration) bool
}

// StopDeps are the process tree primitives a stop uses.
type StopDeps struct {
	IsWindows            bool
	ForceKillProcessTree func(pid int)
	KillGroup            func(pgid int, signal string) error
}

// WaitForBackendExit waits for child, escalates to SIGKILL of its group or tree, and errors when it still lives
func WaitForBackendExit(child Child, deps StopDeps, timeout time.Duration) error {
	if child == nil || child.Exited() {
		return nil
	}
	if child.WaitExit(timeout) {
		return nil
	}
	pid := child.Pid()
	switch {
	case deps.IsWindows && pid > 0:
		deps.ForceKillProcessTree(pid)
	case pid > 0:
		if err := deps.KillGroup(-pid, "SIGKILL"); err != nil {
			child.Kill("SIGKILL")
		}
	default:
		child.Kill("SIGKILL")
	}
	if child.WaitExit(time.Second) {
		return nil
	}
	label := ""
	if pid > 0 {
		label = fmt.Sprintf(" (PID %d)", pid)
	}
	return fmt.Errorf("Backend child%s did not exit after SIGKILL; retaining ownership.", label)
}

// StopBackendChild asks child to stop, the whole tree on Windows and its process group elsewhere
func StopBackendChild(child Child, deps StopDeps) {
	if child == nil || child.Killed() {
		return
	}
	pid := child.Pid()
	switch {
	case deps.IsWindows && pid > 0:
		deps.ForceKillProcessTree(pid)
	case pid > 0:
		if err := deps.KillGroup(-pid, "SIGTERM"); err != nil {
			child.Kill("SIGTERM")
		}
	default:
		child.Kill("SIGTERM")
	}
}

// ExecText runs command with a timeout and returns its trimmed standard output
func ExecText(command string, args []string, timeout time.Duration) (string, error) {
	ctx := context.Background()
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, command, args...)
	hideWindow(cmd)
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("%s timed out after %dms", command, timeout.Milliseconds())
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

const ReapProbeTimeout = 5 * time.Second

// ErrNoProcess classifies a missing PID so orphan reaping can drop its record.
var ErrNoProcess = errors.New("ESRCH")

// NoProcessError reports a PID that no longer exists.
type NoProcessError struct{ Pid int }

func (e *NoProcessError) Error() string { return fmt.Sprintf("PID %d no longer exists", e.Pid) }

// Is matches ErrNoProcess
func (e *NoProcessError) Is(target error) bool { return target == ErrNoProcess }

const pidOnlyMarkerPrefix = "pid-only:"

// IsPidOnlyStartMarker recognizes degraded PID only ownership records
func IsPidOnlyStartMarker(marker any) bool {
	s, ok := marker.(string)
	return ok && strings.HasPrefix(s, pidOnlyMarkerPrefix)
}

// ConnectionAttempt is one generation of a backend connection.
type ConnectionAttempt[C any] struct {
	Generation int
	Future     *Future[C]
}

// ProcessOwner pins a process to the generation that attached it.
type ProcessOwner[P comparable] struct {
	Generation int
	Process    P
}

type pendingStop[P comparable] struct {
	process P
	done    chan struct{}
	err     error
	failed  bool
}

// ConnectionState serializes backend connection attempts, process ownership and stops across generations.
type ConnectionState[P comparable, C any] struct {
	mu         sync.Mutex
	generation int
	process    P
	hasProcess bool
	promise    *Future[C]
	pending    *Future[C]
	stopping   *pendingStop[P]
}

// ErrSuperseded is returned for an attempt a newer connection replaced.
var ErrSuperseded = errors.New("Icebath backend start was superseded by a newer connection attempt.")

// StartAttempt begins a new attempt, refusing while a failed stop still holds the previous child
func (s *ConnectionState[P, C]) StartAttempt() (*ConnectionAttempt[C], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping != nil {
		return nil, errors.New("The previous backend has not stopped. Retry its shutdown before starting a replacement.")
	}
	return &ConnectionAttempt[C]{Generation: s.generation}, nil
}

// SetPromise publishes the attempt future when the attempt is still current
func (s *ConnectionState[P, C]) SetPromise(a *ConnectionAttempt[C], f *Future[C]) bool {
	s.mu.Lock()
	if a.Generation != s.generation {
		s.mu.Unlock()
		return false
	}
	a.Future = f
	s.promise = f
	s.pending = f
	s.mu.Unlock()
	go func() {
		f.Wait()
		s.mu.Lock()
		if a.Generation == s.generation && s.promise == f {
			s.pending = nil
		}
		s.mu.Unlock()
	}()
	return true
}

// IsCurrentAttempt reports whether a is the latest generation
func (s *ConnectionState[P, C]) IsCurrentAttempt(a *ConnectionAttempt[C]) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return a.Generation == s.generation
}

// AssertCurrentAttempt errors when a newer attempt replaced a
func (s *ConnectionState[P, C]) AssertCurrentAttempt(a *ConnectionAttempt[C]) error {
	if !s.IsCurrentAttempt(a) {
		return ErrSuperseded
	}
	return nil
}

// AttachProcess records p for a current attempt and returns its owner
func (s *ConnectionState[P, C]) AttachProcess(a *ConnectionAttempt[C], p P) *ProcessOwner[P] {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.Generation != s.generation {
		return nil
	}
	s.process, s.hasProcess = p, true
	return &ProcessOwner[P]{Generation: s.generation, Process: p}
}

// ClaimProcess attaches p, runs its persistent claim, and keeps it only while still current
func (s *ConnectionState[P, C]) ClaimProcess(a *ConnectionAttempt[C], p P, claim func(P) error) (*ProcessOwner[P], error) {
	owner := s.AttachProcess(a, p)
	if owner == nil {
		return nil, nil
	}
	if err := claim(p); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if owner.Generation == s.generation && s.hasProcess && s.process == p {
		return owner, nil
	}
	return nil, nil
}

// ClearForCurrentProcess clears the process and promise when owner is still the live one
func (s *ConnectionState[P, C]) ClearForCurrentProcess(owner *ProcessOwner[P]) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if owner.Generation != s.generation || !s.hasProcess || owner.Process != s.process {
		return false
	}
	var zero P
	s.process, s.hasProcess, s.promise, s.pending = zero, false, nil, nil
	return true
}

// ClearPromiseForAttempt drops the promise of a failed attempt unless a newer one replaced it
func (s *ConnectionState[P, C]) ClearPromiseForAttempt(a *ConnectionAttempt[C]) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.Generation != s.generation || (s.promise != nil && a.Future != s.promise) {
		return false
	}
	s.promise, s.pending = nil, nil
	return true
}

// Process returns the attached process
func (s *ConnectionState[P, C]) Process() (P, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.process, s.hasProcess
}

// Promise returns the published connection future
func (s *ConnectionState[P, C]) Promise() *Future[C] {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.promise
}

// PendingPromise returns the future while it has not settled
func (s *ConnectionState[P, C]) PendingPromise() *Future[C] {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending
}

func (s *ConnectionState[P, C]) invalidateLocked() (P, bool) {
	p, had := s.process, s.hasProcess
	var zero P
	s.generation++
	s.process, s.hasProcess, s.promise, s.pending = zero, false, nil, nil
	return p, had
}

// Invalidate starts a new generation and returns the process it detached
func (s *ConnectionState[P, C]) Invalidate() (P, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.invalidateLocked()
}

// StopProcess stops the current or still stopping process once, retaining it after a failure until a retry succeeds
func (s *ConnectionState[P, C]) StopProcess(stop func(P) error) error {
	s.mu.Lock()
	if st := s.stopping; st != nil && !st.failed {
		s.mu.Unlock()
		<-st.done
		return st.err
	}
	var current P
	if s.stopping != nil {
		current = s.stopping.process
	} else {
		p, had := s.invalidateLocked()
		if !had {
			s.mu.Unlock()
			return nil
		}
		current = p
	}
	st := &pendingStop[P]{process: current, done: make(chan struct{})}
	s.stopping = st
	s.mu.Unlock()
	err := stop(current)
	s.mu.Lock()
	if err != nil {
		st.failed, st.err = true, err
	} else if s.stopping == st {
		s.stopping = nil
	}
	s.mu.Unlock()
	close(st.done)
	return err
}

// Future is a one shot result shared by every waiter.
type Future[T any] struct {
	done  chan struct{}
	value T
	err   error
}

// NewFuture runs f in a goroutine and settles with its result
func NewFuture[T any](f func() (T, error)) *Future[T] {
	fu := &Future[T]{done: make(chan struct{})}
	go func() {
		defer close(fu.done)
		fu.value, fu.err = f()
	}()
	return fu
}

// Wait blocks until the future settles
func (f *Future[T]) Wait() (T, error) {
	<-f.done
	return f.value, f.err
}

// Settled reports whether the future has a result
func (f *Future[T]) Settled() bool {
	select {
	case <-f.done:
		return true
	default:
		return false
	}
}

// DescriptorEntry is a cached backend descriptor with its connection future.
type DescriptorEntry interface{ comparable }

// ForgetFailedDescriptor removes entry only when it is still the cached one for key
func ForgetFailedDescriptor[E DescriptorEntry](mu *sync.Mutex, cache map[string]E, key string, entry E) {
	mu.Lock()
	defer mu.Unlock()
	if cur, ok := cache[key]; ok && cur == entry {
		delete(cache, key)
	}
}

// AssertDescriptorStillOwned errors when entry was forgotten or replaced before its dial finished
func AssertDescriptorStillOwned[E DescriptorEntry](mu *sync.Mutex, cache map[string]E, key string, entry E) error {
	mu.Lock()
	defer mu.Unlock()
	if cur, ok := cache[key]; !ok || cur != entry {
		return fmt.Errorf("Profile backend start for %q was cancelled before spawn.", key)
	}
	return nil
}

// DialClaims coalesces concurrent dials of one backend scope onto a single spawn.
type DialClaims struct {
	mu       sync.Mutex
	inflight map[string]any
}

// InFlight reports whether a dial for key is running
func (d *DialClaims) InFlight(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.inflight[key]
	return ok
}

// RunDial joins the running dial for key or starts dial, releasing the claim once it settles
func RunDial[T any](d *DialClaims, key string, dial func() (T, error)) (T, error) {
	d.mu.Lock()
	if d.inflight == nil {
		d.inflight = map[string]any{}
	}
	if existing, ok := d.inflight[key].(*Future[T]); ok {
		d.mu.Unlock()
		return existing.Wait()
	}
	f := NewFuture(func() (v T, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		return dial()
	})
	d.inflight[key] = f
	d.mu.Unlock()
	v, err := f.Wait()
	d.mu.Lock()
	if cur, ok := d.inflight[key].(*Future[T]); ok && cur == f {
		delete(d.inflight, key)
	}
	d.mu.Unlock()
	return v, err
}

const LocalConnectionID = "local"

// BackendScopeKey names the pool slot of a connection and profile, the bare profile for the local scope
func BackendScopeKey(connectionID, profile string) string {
	p := strings.TrimSpace(profile)
	if p == "" {
		p = "default"
	}
	c := strings.TrimSpace(connectionID)
	if c == "" || c == LocalConnectionID {
		return p
	}
	return "conn:" + c + "::" + p
}

var scopeKeyRe = regexp.MustCompile(`^conn:(.+?)::(.+)$`)

// ScopeRoute is a parsed backend scope key, ConnectionID empty for the local scope.
type ScopeRoute struct {
	ConnectionID *string `json:"connectionId"`
	Profile      string  `json:"profile"`
}

// ParseBackendScopeKey splits a composite pool key back into its connection and profile
func ParseBackendScopeKey(key string) ScopeRoute {
	v := strings.TrimSpace(key)
	m := scopeKeyRe.FindStringSubmatch(v)
	if m == nil {
		if v == "" {
			v = "default"
		}
		return ScopeRoute{Profile: v}
	}
	id := m[1]
	return ScopeRoute{ConnectionID: &id, Profile: m[2]}
}

// BackendScopePrefix is the key prefix of every pool slot of a connection
func BackendScopePrefix(connectionID string) string {
	return "conn:" + strings.TrimSpace(connectionID) + "::"
}

// PrimaryDialClaimKey separates a dial that inherits the primary from a same keyed explicit dial
func PrimaryDialClaimKey(scopeKey string, inheritsPrimary bool) string {
	if inheritsPrimary {
		return scopeKey + "\x00inherited"
	}
	return scopeKey
}

// HostBackendRecord is a dialable dashboard or serve backend from spawn-ledger.json.
type HostBackendRecord struct {
	CreateTime   *float64 `json:"createTime"`
	Host         string   `json:"host"`
	Pid          int      `json:"pid"`
	Port         int      `json:"port"`
	Profile      string   `json:"profile"`
	Purpose      string   `json:"purpose"`
	RegisteredAt float64  `json:"registeredAt"`
}

const SpawnLedgerFilename = "spawn-ledger.json"

var attachablePurposes = map[string]bool{"dashboard": true, "serve": true}
var loopbackDialable = map[string]bool{"": true, "0.0.0.0": true, "127.0.0.1": true, "::": true, "::1": true, "localhost": true}

func jsonInteger(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok || f != float64(int64(f)) {
		return 0, false
	}
	return int(f), true
}

func jsString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		b, _ := json.Marshal(t)
		return string(b)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// ParseSpawnLedger returns the attachable loopback backends of a spawn ledger, nothing for unreadable contents
func ParseSpawnLedger(contents string) []HostBackendRecord {
	var parsed []any
	if json.Unmarshal([]byte(contents), &parsed) != nil {
		return []HostBackendRecord{}
	}
	out := []HostBackendRecord{}
	for _, v := range parsed {
		e, ok := v.(map[string]any)
		if !ok {
			continue
		}
		pid, okPid := jsonInteger(e["pid"])
		port, okPort := jsonInteger(e["port"])
		purpose, host := jsString(e["purpose"]), jsString(e["host"])
		if !okPid || pid <= 0 || !okPort || port <= 0 || port > 65535 || e["isolated"] == true || !attachablePurposes[purpose] || !loopbackDialable[strings.ToLower(host)] {
			continue
		}
		rec := HostBackendRecord{Host: host, Pid: pid, Port: port, Profile: jsString(e["profile"]), Purpose: purpose}
		if ct, ok := e["create_time"].(float64); ok {
			rec.CreateTime = &ct
		}
		if ra, ok := e["registered_at"].(float64); ok {
			rec.RegisteredAt = ra
		}
		out = append(out, rec)
	}
	return out
}

// RecycleTarget says whether a recycle hits the primary or a pooled backend.
type RecycleTarget string

const (
	RecyclePool    RecycleTarget = "pool"
	RecyclePrimary RecycleTarget = "primary"
)

// RecycleDeps are the teardown steps of a backend recycle.
type RecycleDeps struct {
	NotifyApplied       func()
	PrimaryProfile      string
	Profile             string
	RestartLocalGateway func(profile string) error
	TeardownPool        func(profile string) error
	TeardownPrimary     func() error
	TeardownSSH         func(profile string) error
}

// RecycleOwnedBackendTarget treats an empty or primary profile as the primary backend
func RecycleOwnedBackendTarget(profile, primaryProfile string) RecycleTarget {
	key := strings.TrimSpace(profile)
	if key == "" || key == primaryProfile {
		return RecyclePrimary
	}
	return RecyclePool
}

// RecycleOwnedBackend tears down the SSH serve first, restarts the gateway, then drops the primary or pooled child
func RecycleOwnedBackend(d RecycleDeps) (RecycleTarget, error) {
	target := RecycleOwnedBackendTarget(d.Profile, d.PrimaryProfile)
	profile := strings.TrimSpace(d.Profile)
	if target == RecyclePrimary {
		if err := d.TeardownSSH(""); err != nil {
			return "", err
		}
		if err := d.RestartLocalGateway(d.PrimaryProfile); err != nil {
			return "", err
		}
		if err := d.TeardownPrimary(); err != nil {
			return "", err
		}
		d.NotifyApplied()
		return target, nil
	}
	if err := d.TeardownSSH(profile); err != nil {
		return "", err
	}
	if err := d.RestartLocalGateway(profile); err != nil {
		return "", err
	}
	if err := d.TeardownPool(profile); err != nil {
		return "", err
	}
	return target, nil
}

const (
	ReleaseGateDeadline = 15 * time.Second
	ReleaseGatePoll     = 300 * time.Millisecond
)

// ReleaseGateDeps observe and kill the processes holding the runtime before an update.
type ReleaseGateDeps struct {
	IsShimLocked         func() bool
	IsPidAlive           func(pid int) bool
	CollectStragglerPids func() []int
	KillProcessTree      func(pid int)
	Sleep                func(time.Duration)
	Now                  func() time.Time
	Log                  func(string)
}

// ReleaseGateResult reports whether the runtime unlocked and which signalled PIDs remain.
type ReleaseGateResult struct {
	Unlocked      bool  `json:"unlocked"`
	LingeringPids []int `json:"lingeringPids"`
}

// WaitForBackendRelease waits for the shim to unlock and signalled PIDs to exit, killing stragglers until the deadline
func WaitForBackendRelease(initial []int, d ReleaseGateDeps, tag string, deadline time.Duration) ReleaseGateResult {
	var killed []int
	seen := map[int]bool{}
	add := func(pid int) bool {
		if pid > 0 && !seen[pid] {
			seen[pid] = true
			killed = append(killed, pid)
			return true
		}
		return pid > 0
	}
	for _, p := range initial {
		add(p)
	}
	lingering := func() []int {
		out := []int{}
		for _, p := range killed {
			if d.IsPidAlive(p) {
				out = append(out, p)
			}
		}
		return out
	}
	end := d.Now().Add(deadline)
	for d.Now().Before(end) {
		if !d.IsShimLocked() && len(lingering()) == 0 {
			d.Log(fmt.Sprintf("[%s] venv shim unlocked and %d signalled backend PID(s) exited; safe to proceed", tag, len(killed)))
			return ReleaseGateResult{Unlocked: true, LingeringPids: []int{}}
		}
		for _, p := range d.CollectStragglerPids() {
			if add(p) {
				d.KillProcessTree(p)
			}
		}
		d.Sleep(ReleaseGatePoll)
	}
	left := lingering()
	if !d.IsShimLocked() {
		d.Log(fmt.Sprintf("[%s] proceeding after deadline: venv shim unlocked, but %d signalled PID(s) still enumerable", tag, len(left)))
		return ReleaseGateResult{Unlocked: true, LingeringPids: left}
	}
	return ReleaseGateResult{Unlocked: false, LingeringPids: left}
}

// InstalledRuntimeGate skips the installed runtime under ICEBATH_DESKTOP_IGNORE_EXISTING until this launch installs one.
type InstalledRuntimeGate[T any] struct {
	env       func(string) string
	log       func(string)
	mu        sync.Mutex
	installed bool
}

// NewInstalledRuntimeGate reads the flag through env at every resolve
func NewInstalledRuntimeGate[T any](env func(string) string, log func(string)) *InstalledRuntimeGate[T] {
	return &InstalledRuntimeGate[T]{env: env, log: log}
}

// Resolve probes the installed runtime at root unless the flag tells the desktop to ignore it
func (g *InstalledRuntimeGate[T]) Resolve(root string, probe func() (*T, error)) (*T, error) {
	g.mu.Lock()
	installed := g.installed
	g.mu.Unlock()
	if g.env("ICEBATH_DESKTOP_IGNORE_EXISTING") == "1" && !installed {
		g.log("[bootstrap] ICEBATH_DESKTOP_IGNORE_EXISTING=1; skipping the installed runtime at " + root)
		return nil, nil
	}
	return probe()
}

// AfterInstall marks the runtime as installed by this launch and resolves the backend again
func (g *InstalledRuntimeGate[T]) AfterInstall(resolve func() (*T, error)) (*T, error) {
	g.mu.Lock()
	g.installed = true
	g.mu.Unlock()
	return resolve()
}

// DesktopBackendSpawnEnv keeps base and stamps stay_alive last so nothing overrides it
func DesktopBackendSpawnEnv(base map[string]string) map[string]string {
	out := make(map[string]string, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	out["GATEWAY_ON_ALL_ADAPTERS_DOWN"] = "stay_alive"
	return out
}

// RunBackendStartStep runs step unless ctx is cancelled, returning the cancellation cause if it fires first
func RunBackendStartStep[T any](ctx context.Context, step func() (T, error)) (T, error) {
	var zero T
	if ctx == nil {
		return step()
	}
	if err := context.Cause(ctx); err != nil {
		return zero, err
	}
	f := NewFuture(step)
	select {
	case <-f.done:
		if err := context.Cause(ctx); err != nil {
			return zero, err
		}
		return f.value, f.err
	case <-ctx.Done():
		return zero, context.Cause(ctx)
	}
}
