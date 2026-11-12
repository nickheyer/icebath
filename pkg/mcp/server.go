package mcp

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Reconnect budget of a session that never proved healthy and of the very first connection, swapped by tests
var (
	maxReconnectRetries      = 5
	maxInitialConnectRetries = 3
)

// Run loop timings, swapped by tests
var (
	maxBackoffSeconds   = 60.0
	parkedRetryInterval = 300 * time.Second
	keepaliveRPCTimeout = 30 * time.Second
	shutdownWait        = 10 * time.Second
	keepaliveDefault    = time.Duration(defaultKeepaliveInterval * float64(time.Second))
	keepaliveFloor      = time.Duration(minKeepaliveInterval * float64(time.Second))
	childPollInterval   = 250 * time.Millisecond
	serverNow           = time.Now
	serverSleep         = func(ctx context.Context, d time.Duration) bool {
		if d <= 0 {
			return ctx.Err() == nil
		}
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
			return true
		case <-ctx.Done():
			return false
		}
	}
)

// Config keys whose change on disk makes a remote rebuild adopt the fresh definition
var remoteRebindKeys = []string{"url", "auth", "oauth", "headers", "transport"}

// Per run retry counters, the reconnect count lives on the task
type retryBudget struct {
	initialRetries int
	backoff        float64
}

// OAuth credentials an auth failure park failed with
type authParkState struct {
	storage *TokenStorage
	stamp   CredentialStamp
}

// One running RPC a deliberate teardown can fail fast
type inflightCall struct {
	cancel context.CancelFunc
}

// One MCP server connection served by a long lived goroutine that connects, serves, reconnects, parks or recycles
type serverTask struct {
	name string
	env  *tools.Env

	ready      *event
	shutdownEv *event
	reconnect  *event
	rpc        rpcMutex
	refreshMu  sync.Mutex

	mu                 sync.Mutex
	session            *Session
	toolTimeout        float64
	tools              []*Tool
	toolNames          []string
	toolNamesSet       bool
	rendered           *Rendered
	config             map[string]any
	err                error
	sampler            Sampler
	elicitor           Elicitor
	reconnectRetries   int
	sessionProven      bool
	everConnected      bool
	sseFallback        bool
	httpRejection      HTTPRejection
	wasParked          bool
	parkReason         string
	lastParkLine       string
	authPark           *authParkState
	resolvedIdentity   string
	inflight           map[*inflightCall]struct{}
	reconnecting       bool
	suspectReason      string
	teardownRace       bool
	permanentGraceUsed bool
	stdioChildPIDs     []int
	authType           string
	pendingCallContext context.Context
	lifecycleStartedAt time.Time
	lastToolCallAt     time.Time
	idleTimeout        *float64
	maxLifetime        *float64
	recycledReason     string
	initializeResult   *InitializeResult
	listCacheMeta      CacheHints
	pingUnsupported    bool
	unattended         bool
	transportRunner    func(ctx context.Context, r *Rendered) (string, error)

	running       bool
	done          chan struct{}
	cancel        context.CancelFunc
	refreshWG     sync.WaitGroup
	refreshCancel context.CancelFunc
	refreshCtx    context.Context
}

// Fresh task for one server, idle until started
func newServerTask(name string, env *tools.Env) *serverTask {
	now := serverNow()
	rctx, rcancel := context.WithCancel(context.Background())
	return &serverTask{
		name: name, env: env, ready: newEvent(), shutdownEv: newEvent(), reconnect: newEvent(),
		toolTimeout: defaultToolTimeout, inflight: map[*inflightCall]struct{}{}, config: map[string]any{},
		lifecycleStartedAt: now, lastToolCallAt: now, done: make(chan struct{}),
		refreshCtx: rctx, refreshCancel: rcancel,
	}
}

// Whether the server holds a session and finished registering its tools, an empty list included
func (s *serverTask) registrationFinished() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session != nil && s.toolNamesSet
}

func (s *serverTask) registeredToolNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.toolNames...)
}

func (s *serverTask) reconnectEvent() *event { return s.reconnect }

func (s *serverTask) readyEvent() *event { return s.ready }

func (s *serverTask) currentSession() *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session
}

// Replaces the live session under the task lock
func (s *serverTask) setSession(sess *Session) {
	s.mu.Lock()
	s.session = sess
	s.mu.Unlock()
}

// Config tree tool timeouts resolve against
func (s *serverTask) tree() config.Tree {
	if s.env == nil {
		return config.Tree{}
	}
	return s.env.Config
}

// Snapshot of the config the task currently serves
func (s *serverTask) currentConfig() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config
}

// Whether the server is reached over HTTP or SSE rather than stdio
func (s *serverTask) isHTTP() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isHTTPLocked()
}

func (s *serverTask) isHTTPLocked() bool {
	_, ok := s.config["url"]
	return ok
}

// True when a stdio server was intentionally recycled
func (s *serverTask) isRecycledStdio() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.isHTTPLocked() && s.recycledReason != ""
}

// Records that a user visible MCP operation is starting
func (s *serverTask) markToolCall() {
	s.mu.Lock()
	s.lastToolCallAt = serverNow()
	s.mu.Unlock()
}

// Restarts the idle and lifetime clocks for a fresh stdio session
func (s *serverTask) markLifecycleStarted() {
	s.mu.Lock()
	now := serverNow()
	s.lifecycleStartedAt, s.lastToolCallAt = now, now
	s.recycledReason = ""
	s.mu.Unlock()
}

type recycleDeadline struct {
	at     time.Time
	reason string
}

// Lifetime and idle deadlines of a stdio session, none for HTTP or while an RPC holds the lock
func (s *serverTask) stdioRecycleDeadlines() []recycleDeadline {
	if s.isHTTP() || s.rpc.Locked() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []recycleDeadline
	if s.maxLifetime != nil {
		out = append(out, recycleDeadline{s.lifecycleStartedAt.Add(secondsDuration(*s.maxLifetime)), "max_lifetime_seconds"})
	}
	if s.idleTimeout != nil {
		out = append(out, recycleDeadline{s.lastToolCallAt.Add(secondsDuration(*s.idleTimeout)), "idle_timeout_seconds"})
	}
	return out
}

// Recycle reason whose deadline has passed, lifetime first, empty when none
func (s *serverTask) stdioRecycleReason(now time.Time) string {
	for _, d := range s.stdioRecycleDeadlines() {
		if !now.Before(d.at) {
			return d.reason
		}
	}
	return ""
}

// Earliest stdio recycle deadline, false when no limit applies right now
func (s *serverTask) nextStdioRecycleDeadline() (time.Time, bool) {
	var best time.Time
	found := false
	for _, d := range s.stdioRecycleDeadlines() {
		if !found || d.at.Before(best) {
			best, found = d.at, true
		}
	}
	return best, found
}

// Marks a stdio session dormant before its transport finishes closing
func (s *serverTask) markStdioRecycled(reason string) {
	s.mu.Lock()
	s.recycledReason = reason
	s.session = nil
	s.mu.Unlock()
}

// Latches a due stdio idle or lifetime recycle
func (s *serverTask) recycleIfDue() bool {
	reason := s.stdioRecycleReason(serverNow())
	if reason == "" {
		return false
	}
	s.markStdioRecycled(reason)
	return true
}

// Seconds as a duration
func secondsDuration(sec float64) time.Duration { return time.Duration(sec * float64(time.Second)) }

// Python class name style label of an error for log lines
func errTypeName(err error) string {
	if name := pyOSErrorName(err); name != "" {
		return name
	}
	name := fmt.Sprintf("%T", err)
	name = strings.TrimLeft(name, "*")
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// Python exception class an OS level Go error surfaces as, empty for any other error
func pyOSErrorName(err error) string {
	switch err.(type) {
	case *fileNotFoundError:
		return "FileNotFoundError"
	case *mcpConnectionError:
		return "ConnectionError"
	case *probeTimeoutError:
		return "TimeoutError"
	case *fs.PathError, *os.SyscallError, *os.LinkError, *exec.Error, syscall.Errno:
	default:
		switch {
		case err == context.Canceled:
			return "CancelledError"
		case err == context.DeadlineExceeded:
			return "TimeoutError"
		}
		return ""
	}
	var errno syscall.Errno
	switch {
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		return "FileNotFoundError"
	case errors.Is(err, fs.ErrExist):
		return "FileExistsError"
	case errors.Is(err, fs.ErrPermission):
		return "PermissionError"
	case !errors.As(err, &errno):
		return "OSError"
	}
	switch errno {
	case syscall.EAGAIN, syscall.EALREADY, syscall.EINPROGRESS:
		return "BlockingIOError"
	case syscall.ECHILD:
		return "ChildProcessError"
	case syscall.EPIPE, syscall.ESHUTDOWN:
		return "BrokenPipeError"
	case syscall.ECONNABORTED:
		return "ConnectionAbortedError"
	case syscall.ECONNREFUSED:
		return "ConnectionRefusedError"
	case syscall.ECONNRESET:
		return "ConnectionResetError"
	case syscall.EINTR:
		return "InterruptedError"
	case syscall.EISDIR:
		return "IsADirectoryError"
	case syscall.ENOTDIR:
		return "NotADirectoryError"
	case syscall.ESRCH:
		return "ProcessLookupError"
	case syscall.ETIMEDOUT:
		return "TimeoutError"
	}
	return "OSError"
}

// False only when captured capabilities omit tools, true without capability info
func (s *serverTask) advertisesTools() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.initializeResult == nil || s.initializeResult.Capabilities == nil {
		return true
	}
	return s.initializeResult.Capabilities.Tools != nil
}

// Exercises the session with ping, falling back to tools list when ping is unsupported or silently dropped
func (s *serverTask) keepaliveProbe(ctx context.Context) error {
	sess := s.currentSession()
	if sess == nil {
		return errors.New("MCP session is not connected")
	}
	listTools := func() error {
		lctx, cancel := context.WithTimeout(ctx, keepaliveRPCTimeout)
		defer cancel()
		_, err := sess.ListTools(lctx, nil)
		return err
	}
	s.mu.Lock()
	unsupported := s.pingUnsupported
	s.mu.Unlock()
	if !unsupported {
		pctx, cancel := context.WithTimeout(ctx, keepaliveRPCTimeout)
		err := sess.Ping(pctx)
		cancel()
		if err == nil {
			return nil
		}
		switch {
		case isMethodNotFoundError(err):
			if !s.advertisesTools() {
				return err
			}
			s.mu.Lock()
			s.pingUnsupported = true
			s.mu.Unlock()
			mcpLogger().Info(fmt.Sprintf("MCP server '%s': does not implement the optional 'ping' utility (-32601); using 'list_tools' for keepalive on this connection.", s.name))
		case isTimeout(err) && s.advertisesTools():
			if listTools() != nil {
				return err
			}
			s.mu.Lock()
			s.pingUnsupported = true
			s.mu.Unlock()
			mcpLogger().Info(fmt.Sprintf("MCP server '%s': ping timed out but list_tools succeeded — server silently drops ping; using 'list_tools' for keepalive on this connection.", s.name))
			return nil
		default:
			return err
		}
	}
	return listTools()
}

// Records demonstrated health, clearing the rapid drop budget and logging a revival once
func (s *serverTask) markSessionProven() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionProven {
		return
	}
	s.sessionProven = true
	s.reconnectRetries = 0
	s.parkReason = ""
	if s.wasParked {
		s.wasParked = false
		mcpLogger().Warn(fmt.Sprintf("MCP server '%s': revived — session healthy again after parking (state: parked → connected)", s.name))
	}
	s.permanentGraceUsed, s.teardownRace = false, false
}

// Latches a suspicion the next call verifies through ensureHealthy
func (s *serverTask) markSuspect(reason string) {
	s.mu.Lock()
	s.markSuspectLocked(reason)
	s.mu.Unlock()
}

func (s *serverTask) markSuspectLocked(reason string) {
	if s.suspectReason == "" && reason != "" {
		mcpLogger().Warn(fmt.Sprintf("MCP server '%s': connection marked suspect (%s); next call will health-check it", s.name, reason))
	}
	s.suspectReason = reason
}

// Verifies a suspect connection before reuse, requesting a reconnect and returning false when it is dead
func (s *serverTask) ensureHealthy(ctx context.Context, timeout time.Duration) bool {
	s.mu.Lock()
	reason := s.suspectReason
	if reason == "" {
		s.mu.Unlock()
		return true
	}
	if s.session == nil {
		s.suspectReason = ""
		s.mu.Unlock()
		s.reconnect.Set()
		return false
	}
	s.mu.Unlock()
	pctx, cancel := context.WithTimeout(ctx, timeout)
	err := s.keepaliveProbe(pctx)
	if err == nil && pctx.Err() != nil {
		err = pctx.Err()
	}
	cancel()
	if err != nil {
		mcpLogger().Warn(fmt.Sprintf("MCP server '%s': suspect connection (%s) failed health check (%s: %s) — requesting reconnect (state: suspect → degraded)", s.name, reason, errTypeName(err), excStr(err)))
		s.mu.Lock()
		s.suspectReason = ""
		s.markSuspectLocked("health check failed after " + reason)
		s.session = nil
		s.mu.Unlock()
		s.ready.Clear()
		s.reconnect.Set()
		return false
	}
	mcpLogger().Info(fmt.Sprintf("MCP server '%s': suspect connection passed health check (%s) — clearing suspicion", s.name, reason))
	s.mu.Lock()
	s.suspectReason = ""
	s.mu.Unlock()
	s.markSessionProven()
	return true
}

// Registers a running RPC so a teardown can fail it fast, the returned func unregisters it
func (s *serverTask) trackInflight(cancel context.CancelFunc) func() {
	c := &inflightCall{cancel: cancel}
	s.mu.Lock()
	s.inflight[c] = struct{}{}
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.inflight, c)
		s.mu.Unlock()
	}
}

// Whether a teardown is failing in flight calls on purpose
func (s *serverTask) isReconnecting() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reconnecting
}

// Number of RPCs still running on the session
func (s *serverTask) inflightCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inflight)
}

// Cancels every in flight RPC before the transport unwinds and flags the teardown race
func (s *serverTask) failInflightCalls(reason string) {
	s.mu.Lock()
	if len(s.inflight) == 0 {
		s.mu.Unlock()
		return
	}
	victims := make([]*inflightCall, 0, len(s.inflight))
	for c := range s.inflight {
		victims = append(victims, c)
	}
	s.reconnecting, s.teardownRace = true, true
	s.markSuspectLocked(fmt.Sprintf("%s tore down %d in-flight call(s)", reason, len(victims)))
	s.mu.Unlock()
	for _, c := range victims {
		c.cancel()
	}
}

// Liveness probe of one tracked child pid, nil when this host has no probe support
var childPidProbe = func(pid int) (bool, error) { return pidExists(pid), nil }

// True when every stdio child spawned for the current transport has exited, false when liveness is unknown
func (s *serverTask) stdioChildrenDead() bool {
	s.mu.Lock()
	pids := append([]int(nil), s.stdioChildPIDs...)
	http := s.isHTTPLocked()
	s.mu.Unlock()
	if len(pids) == 0 || http || childPidProbe == nil {
		return false
	}
	for _, pid := range pids {
		alive, err := childPidProbe(pid)
		if err != nil || alive {
			return false
		}
	}
	return true
}

// Polls stdio child liveness and returns once a tracked child set has died or ctx ends
func (s *serverTask) watchStdioChildren(ctx context.Context) bool {
	for !s.stdioChildrenDead() {
		if !serverSleep(ctx, childPollInterval) {
			return false
		}
	}
	return true
}

// Runs fn as a tracked tool refresh that shutdown cancels and waits for
func (s *serverTask) spawnRefresh(fn func(ctx context.Context)) {
	s.mu.Lock()
	ctx := s.refreshCtx
	s.refreshWG.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.refreshWG.Done()
		fn(ctx)
	}()
}

// Schedules a background tool refresh whose failure is logged
func (s *serverTask) scheduleToolsRefresh() {
	s.spawnRefresh(func(ctx context.Context) {
		if err := s.refreshTools(ctx); err != nil {
			mcpLogger().Error(fmt.Sprintf("MCP server '%s': dynamic tool refresh failed: %s", s.name, excStr(err)))
		}
	})
}

// Timer the lifecycle wait arms for one keepalive or recycle interval, nil for an open ended wait
var lifecycleTimer = func(d time.Duration) <-chan time.Time {
	if d < 0 {
		return nil
	}
	return time.After(d)
}

// Serves until shutdown, reconnect or a stdio recycle, keeping remote sessions alive with periodic probes
func (s *serverTask) waitForLifecycleEvent(ctx context.Context, sessionDone <-chan struct{}) (string, error) {
	isHTTP := s.isHTTP()
	cfg := s.currentConfig()
	configured, hasConfigured := cfg["keepalive_interval"]
	hasConfigured = hasConfigured && configured != nil
	var keepalive time.Duration
	if isHTTP || hasConfigured {
		keepalive = keepaliveDefault
		if hasConfigured {
			f, _ := config.AsFloat(configured)
			keepalive = secondsDuration(f)
		}
		keepalive = max(keepaliveFloor, keepalive)
	}
	var proofAt time.Time
	s.mu.Lock()
	proven := s.sessionProven
	s.mu.Unlock()
	if keepalive == 0 && !proven {
		proofAt = serverNow().Add(keepaliveDefault)
	}
	var rpcIdle chan struct{}
	for {
		if s.recycleIfDue() {
			return "recycle", nil
		}
		s.mu.Lock()
		proven = s.sessionProven
		s.mu.Unlock()
		timeout := time.Duration(-1)
		if keepalive > 0 {
			timeout = keepalive
		}
		if timeout < 0 && !proven && !proofAt.IsZero() {
			timeout = max(0, proofAt.Sub(serverNow()))
		}
		if deadline, ok := s.nextStdioRecycleDeadline(); ok {
			recycle := max(0, deadline.Sub(serverNow()))
			if timeout < 0 || recycle < timeout {
				timeout = recycle
			}
		} else if !isHTTP && s.rpc.Locked() && rpcIdle == nil {
			rpcIdle = make(chan struct{})
			go func(ch chan struct{}) {
				if s.rpc.Lock(ctx) == nil {
					s.rpc.Unlock()
				}
				close(ch)
			}(rpcIdle)
		}
		timer := lifecycleTimer(timeout)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-s.shutdownEv.Wait():
			return s.finishLifecycleWait(), nil
		case <-s.reconnect.Wait():
			return s.finishLifecycleWait(), nil
		case <-sessionDone:
			if s.shutdownEv.IsSet() || s.reconnect.IsSet() {
				return s.finishLifecycleWait(), nil
			}
			return "", &sessionDropError{err: s.droppedSessionErr()}
		case <-rpcIdle:
			rpcIdle = nil
			continue
		case <-timer:
		}
		if s.recycleIfDue() {
			return "recycle", nil
		}
		if keepalive == 0 {
			s.mu.Lock()
			proven = s.sessionProven
			s.mu.Unlock()
			if !proven && !proofAt.IsZero() && !serverNow().Before(proofAt) {
				if s.stdioChildrenDead() {
					mcpLogger().Warn(fmt.Sprintf("MCP server '%s' stdio child exited before the session proved healthy; triggering reconnect (state: connected → degraded)", s.name))
					return s.finishLifecycleWait(), nil
				}
				s.markSessionProven()
			}
			continue
		}
		if s.currentSession() == nil {
			continue
		}
		if s.rpc.Locked() || s.inflightCount() > 0 {
			continue
		}
		if err := s.rpc.Lock(ctx); err != nil {
			return "", err
		}
		err := s.keepaliveProbe(ctx)
		s.rpc.Unlock()
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			root := unwrapExceptionGroup(err)
			mcpLogger().Warn(fmt.Sprintf("MCP server '%s' keepalive failed, triggering reconnect (state: connected → degraded): %s: %s", s.name, errTypeName(root), root.Error()))
			s.markSuspect(fmt.Sprintf("keepalive failed: %s: %s", errTypeName(root), root.Error()))
			s.reconnect.Set()
			return s.finishLifecycleWait(), nil
		}
		s.markSessionProven()
	}
}

// Shutdown or reconnect outcome of a lifecycle wait, failing in flight calls first
func (s *serverTask) finishLifecycleWait() string {
	if s.shutdownEv.IsSet() {
		s.failInflightCalls("shutdown")
		return "shutdown"
	}
	s.failInflightCalls("reconnect")
	s.reconnect.Clear()
	return "reconnect"
}

// Session drop that ended a live serve, mapped to a reconnect on remote transports
type sessionDropError struct{ err error }

func (e *sessionDropError) Error() string { return excStr(e.err) }

func (e *sessionDropError) Unwrap() error { return e.err }

// Terminal error of the session that just dropped
func (s *serverTask) droppedSessionErr() error {
	if sess := s.currentSession(); sess != nil {
		if err := sess.Err(); err != nil {
			return err
		}
	}
	return errors.New("MCP session closed")
}

// Parked wait returning shutdown, reconnect or self-probe when timeout elapses first
func (s *serverTask) waitForReconnectOrShutdown(ctx context.Context, timeout time.Duration) string {
	var timer <-chan time.Time
	if timeout >= 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}
	select {
	case <-ctx.Done():
	case <-s.shutdownEv.Wait():
	case <-s.reconnect.Wait():
	case <-timer:
	}
	if s.shutdownEv.IsSet() || ctx.Err() != nil {
		return "shutdown"
	}
	if !s.reconnect.IsSet() {
		return "self-probe"
	}
	s.reconnect.Clear()
	return "reconnect"
}

// Warns on a new park line and demotes an identical repeat while still parked to debug
func (s *serverTask) logPark(line string) {
	s.mu.Lock()
	repeat := s.wasParked && line == s.lastParkLine
	if !repeat {
		s.lastParkLine = line
	}
	s.mu.Unlock()
	if repeat {
		mcpLogger().Debug(line)
	} else {
		mcpLogger().Warn(line)
	}
}

// Drops the tools and waits for a revival, self probing on a timer, true when shutdown came instead
func (s *serverTask) park(ctx context.Context, revivalReason string) bool {
	s.deregisterTools(ctx)
	s.mu.Lock()
	s.wasParked = true
	s.parkReason = revivalReason
	s.mu.Unlock()
	s.reconnect.Clear()
	paused := false
	for {
		outcome := s.waitForReconnectOrShutdown(ctx, parkedRetryInterval)
		if outcome == "shutdown" {
			return true
		}
		if outcome == "self-probe" && !s.stillConfiguredEnabled(ctx) {
			line := fmt.Sprintf("MCP server '%s': parked entry is disabled or gone from mcp_servers; pausing the self-probe until it is re-enabled", s.name)
			if paused {
				mcpLogger().Debug(line)
			} else {
				mcpLogger().Info(line)
			}
			paused = true
			continue
		}
		if outcome == "self-probe" {
			s.mu.Lock()
			s.unattended = true
			s.mu.Unlock()
		}
		mcpLogger().Debug(fmt.Sprintf("MCP server '%s': attempting revival %s (%s); rebuilding transport.", s.name, revivalReason, outcome))
		return false
	}
}

// Whether mcp_servers on disk still wants this server connected, true when the config cannot be read
func (s *serverTask) stillConfiguredEnabled(ctx context.Context) (enabled bool) {
	defer func() {
		if r := recover(); r != nil {
			enabled = true
		}
	}()
	entry, ok := LoadServerConfigs(ctx, s.env)[s.name]
	if !ok || entry == nil {
		return false
	}
	return ServerEnabled(entry.Config)
}

// Binds config and handlers and validates a remote endpoint, false when the server must not start
func (s *serverTask) prepareRun(ctx context.Context, r *Rendered) bool {
	cfg := r.Config
	s.mu.Lock()
	s.rendered, s.config = r, cfg
	s.toolTimeout = resolveToolTimeout(cfg, s.tree())
	s.authType = strings.ToLower(strings.TrimSpace(pyStr(cfg["auth"])))
	s.idleTimeout, s.maxLifetime = nil, nil
	if v, ok := lifecycleSeconds(cfg, "idle_timeout_seconds"); ok {
		s.idleTimeout = &v
	}
	if v, ok := lifecycleSeconds(cfg, "max_lifetime_seconds"); ok {
		s.maxLifetime = &v
	}
	authType := s.authType
	s.mu.Unlock()
	samplingCfg, _ := cfg["sampling"].(map[string]any)
	if samplingCfg == nil {
		samplingCfg = map[string]any{}
	}
	var sampler Sampler
	if v, ok := samplingCfg["enabled"]; !ok || pyTruthy(v) {
		sampler = newSamplingHandler(s.name, s.env, samplingCfg)
	}
	elicitCfg, _ := cfg["elicitation"].(map[string]any)
	if elicitCfg == nil {
		elicitCfg = map[string]any{}
	}
	var elicitor Elicitor
	if v, ok := elicitCfg["enabled"]; !ok || pyTruthy(v) {
		elicitor = newElicitationHandler(s.name, s.env, elicitCfg, func() context.Context {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.pendingCallContext
		})
	}
	s.mu.Lock()
	s.sampler, s.elicitor = sampler, elicitor
	s.mu.Unlock()
	_, hasURL := cfg["url"]
	_, hasCommand := cfg["command"]
	if hasURL && hasCommand {
		mcpLogger().Warn(fmt.Sprintf("MCP server '%s' has both 'url' and 'command' in config. Using HTTP transport ('url'). Remove 'command' to silence this warning.", s.name))
	}
	if !hasURL {
		return true
	}
	err := validateRemoteMCPURL(s.name, cfg["url"])
	if err == nil && pyStr(cfg["transport"]) != "sse" && !pyTruthy(cfg["skip_preflight"]) &&
		livenessKind(ctx, s.name) != "server_json" && !s.ready.IsSet() && authType != "oauth" {
		headers := map[string]string{}
		if h, ok := cfg["headers"].(map[string]any); ok {
			for k, v := range h {
				headers[k] = pyStr(v)
			}
		}
		sslVerify, ok := cfg["ssl_verify"]
		if !ok {
			sslVerify = true
		}
		err = s.preflightContentType(ctx, pyStr(cfg["url"]), headers, sslVerify, cfg["client_cert"], cfg["client_key"],
			5*time.Second, pyTruthy(cfg["strict_redirect_headers"]))
	}
	if err != nil {
		mcpLogger().Warn(excStr(err))
		s.publishError(err)
		return false
	}
	return true
}

// Hands err to the waiting start
func (s *serverTask) publishError(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
	s.ready.Set()
}

// Adopts the on disk definition of a remote server when its endpoint or auth changed since start
func (s *serverTask) refreshRemoteConfig(ctx context.Context, r *Rendered) *Rendered {
	if !s.isHTTP() {
		return r
	}
	fresh, ok := LoadServerConfigs(ctx, s.env)[s.name]
	if !ok || fresh == nil {
		return r
	}
	if _, hasURL := fresh.Config["url"]; !hasURL {
		return r
	}
	same := true
	for _, k := range remoteRebindKeys {
		if !reflect.DeepEqual(fresh.Config[k], r.Config[k]) {
			same = false
			break
		}
	}
	if same {
		return r
	}
	mcpLogger().Info(fmt.Sprintf("MCP server '%s': definition changed in config.yaml (%s -> %s); rebuilding with the new one",
		s.name, pyRepr(r.Config["url"]), pyRepr(fresh.Config["url"])))
	s.mu.Lock()
	s.rendered, s.config = fresh, fresh.Config
	s.authType = strings.ToLower(strings.TrimSpace(pyStr(fresh.Config["auth"])))
	s.sseFallback = false
	s.mu.Unlock()
	return fresh
}

// Connecting, connected, degraded, parked and revived until shutdown, parking rather than exiting on exhaustion
func (s *serverTask) run(ctx context.Context, r *Rendered) {
	if !s.prepareRun(ctx, r) {
		return
	}
	s.mu.Lock()
	s.reconnectRetries = 0
	s.mu.Unlock()
	budget := &retryBudget{backoff: 1.0}
	rebuild := false
	for {
		var reason string
		var err error
		if rebuild {
			scoped := installOwnerSecretScope(ctx)
			r = s.refreshRemoteConfig(scoped, r)
			err = requireRenderedRemote(s.name, r.Config)
		}
		rebuild = true
		if err == nil {
			s.mu.Lock()
			s.authPark = nil
			s.resolvedIdentity = ""
			unattended := s.unattended
			s.mu.Unlock()
			tctx := ctx
			if unattended {
				tctx = SuppressInteractiveOAuth(ctx)
			}
			if runner := s.transportRunner; runner != nil {
				reason, err = runner(tctx, r)
			} else if s.isHTTP() {
				reason, err = s.runHTTP(tctx, r)
			} else {
				reason, err = s.runStdio(tctx, r)
			}
		}
		s.mu.Lock()
		s.session = nil
		s.stdioChildPIDs = nil
		s.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		var keepGoing bool
		if err != nil {
			keepGoing = s.onTransportError(ctx, err, budget)
		} else {
			keepGoing = s.onCleanReturn(ctx, reason, budget)
		}
		if !keepGoing {
			return
		}
	}
}

// Handles a clean transport return of shutdown, stdio recycle or a requested rebuild
func (s *serverTask) onCleanReturn(ctx context.Context, reason string, budget *retryBudget) bool {
	if s.shutdownEv.IsSet() {
		return false
	}
	if reason == "recycle" {
		s.mu.Lock()
		recycled := s.recycledReason
		s.session = nil
		s.mu.Unlock()
		mcpLogger().Info(fmt.Sprintf("MCP server '%s': stdio session recycled after %s; waiting for lazy reconnect", s.name, recycled))
		return s.waitForReconnectOrShutdown(ctx, -1) != "shutdown"
	}
	mcpLogger().Debug(fmt.Sprintf("MCP server '%s': reconnecting (OAuth recovery or manual refresh)", s.name))
	s.mu.Lock()
	switch {
	case s.teardownRace && !s.sessionProven:
		mcpLogger().Info(fmt.Sprintf("MCP server '%s': reconnect after teardown race (in-flight calls were failed); not charging the rapid-drop budget", s.name))
		s.teardownRace, budget.backoff = false, 1.0
	case s.sessionProven:
		s.reconnectRetries, budget.backoff = 0, 1.0
	default:
		s.reconnectRetries++
		if s.reconnectRetries > maxReconnectRetries {
			s.mu.Unlock()
			s.logPark(fmt.Sprintf("MCP server '%s': %d consecutive reconnects without a healthy session (rapid-drop budget exhausted), parking; will self-probe every %ds until it recovers (state: degraded → parked)",
				s.name, maxReconnectRetries, int(parkedRetryInterval.Seconds())))
			if !s.parkAndRearm(ctx, "from parked state", budget) {
				return false
			}
			s.mu.Lock()
		}
	}
	s.session = nil
	s.mu.Unlock()
	s.ready.Clear()
	return true
}

// Parks and on revival leaves one probe per wake, false on shutdown
func (s *serverTask) parkAndRearm(ctx context.Context, revivalReason string, budget *retryBudget) bool {
	if s.park(ctx, revivalReason) {
		return false
	}
	s.mu.Lock()
	s.reconnectRetries, budget.backoff = maxReconnectRetries, 1.0
	s.mu.Unlock()
	return true
}

// Publishes err to start, parks, and on revival resets every counter, false on shutdown
func (s *serverTask) parkInitialFailure(ctx context.Context, err error, revivalReason string, budget *retryBudget) bool {
	s.publishError(err)
	if s.park(ctx, revivalReason) {
		return false
	}
	s.mu.Lock()
	budget.initialRetries, s.reconnectRetries = 0, 0
	budget.backoff = 1.0
	s.err = nil
	s.mu.Unlock()
	s.ready.Clear()
	return true
}

// Sleeps the jittered backoff and doubles it up to the cap
func (s *serverTask) backoffSleep(ctx context.Context, budget *retryBudget) {
	serverSleep(ctx, secondsDuration(jittered(budget.backoff)))
	budget.backoff = min(budget.backoff*2, maxBackoffSeconds)
}

// Classifies a transport failure and runs the initial connect or reconnect ladder, false to exit
func (s *serverTask) onTransportError(ctx context.Context, err error, budget *retryBudget) bool {
	failureClass := classifyMCPFailure(err)
	if s.isRecycledStdio() {
		mcpLogger().Warn(fmt.Sprintf("MCP server '%s': lazy reconnect after stdio recycle failed, marking unavailable while retrying: %s: %s", s.name, errTypeName(err), excStr(err)))
		s.mu.Lock()
		s.recycledReason = ""
		s.mu.Unlock()
	}
	s.mu.Lock()
	ever := s.everConnected
	s.mu.Unlock()
	if !ever {
		return s.onInitialConnectError(ctx, err, failureClass, budget)
	}
	if s.shutdownEv.IsSet() {
		mcpLogger().Debug(fmt.Sprintf("MCP server '%s' disconnected during shutdown: %s: %s", s.name, errTypeName(err), excStr(err)))
		return false
	}
	if failureClass == "permanent" {
		return s.onPermanentError(ctx, err, budget)
	}
	s.mu.Lock()
	s.reconnectRetries++
	retries := s.reconnectRetries
	s.mu.Unlock()
	if retries > maxReconnectRetries {
		s.logPark(fmt.Sprintf("MCP server '%s' failed after %d reconnection attempts, parking; will self-probe every %ds until it recovers (state: degraded → parked): %s: %s",
			s.name, maxReconnectRetries, int(parkedRetryInterval.Seconds()), errTypeName(err), excStr(err)))
		return s.parkAndRearm(ctx, "from parked state", budget)
	}
	mcpLogger().Debug(fmt.Sprintf("MCP server '%s' connection lost (attempt %d/%d), reconnecting in %.0fs: %s: %s",
		s.name, retries, maxReconnectRetries, budget.backoff, errTypeName(err), excStr(err)))
	s.backoffSleep(ctx, budget)
	return !s.shutdownEv.IsSet() && ctx.Err() == nil
}

// Initial connect ladder that parks at once on a permanent failure or after the initial retries
func (s *serverTask) onInitialConnectError(ctx context.Context, err error, failureClass string, budget *retryBudget) bool {
	if failureClass == "permanent" {
		detail := "connection with a permanent error, parking without retries"
		if isAuthError(err) {
			detail = fmt.Sprintf("authentication, parking until credentials change; re-authenticate with `icebath mcp login %s`", s.name)
		}
		s.logPark(fmt.Sprintf("MCP server '%s' failed initial %s (state: connecting → parked): %s: %s", s.name, detail, errTypeName(err), excStr(err)))
		s.armAuthPark(ctx, err)
		return s.parkInitialFailure(ctx, err, "after permanent initial failure", budget)
	}
	budget.initialRetries++
	if budget.initialRetries > maxInitialConnectRetries {
		s.logPark(fmt.Sprintf("MCP server '%s' failed initial connection after %d attempts, parking until a reconnect is requested (state: connecting → parked): %s: %s",
			s.name, maxInitialConnectRetries, errTypeName(err), excStr(err)))
		return s.parkInitialFailure(ctx, err, "after initial connection failures", budget)
	}
	mcpLogger().Debug(fmt.Sprintf("MCP server '%s' initial connection failed (attempt %d/%d), retrying in %.0fs: %s: %s",
		s.name, budget.initialRetries, maxInitialConnectRetries, budget.backoff, errTypeName(err), excStr(err)))
	s.backoffSleep(ctx, budget)
	if s.shutdownEv.IsSet() {
		s.publishError(err)
	}
	return !s.shutdownEv.IsSet() && ctx.Err() == nil
}

// Grants a proven session one suspect reconnect on an auth error, otherwise parks without retries
func (s *serverTask) onPermanentError(ctx context.Context, err error, budget *retryBudget) bool {
	s.mu.Lock()
	grace := isAuthError(err) && s.sessionProven && !s.permanentGraceUsed
	if grace {
		s.permanentGraceUsed = true
	}
	s.mu.Unlock()
	if grace {
		s.markSuspect("auth error on proven session: " + excStr(err))
		mcpLogger().Warn(fmt.Sprintf("MCP server '%s': auth error on a previously healthy session — marking suspect and forcing one reconnect instead of parking (state: connected → suspect): %s: %s",
			s.name, errTypeName(err), excStr(err)))
		s.mu.Lock()
		s.reconnectRetries, budget.backoff = 0, 1.0
		s.mu.Unlock()
		serverSleep(ctx, secondsDuration(jittered(1.0)))
		return !s.shutdownEv.IsSet() && ctx.Err() == nil
	}
	s.logPark(fmt.Sprintf("MCP server '%s' hit a permanent error, parking without retries; will self-probe every %ds (state: connected → parked): %s: %s",
		s.name, int(parkedRetryInterval.Seconds()), errTypeName(err), excStr(err)))
	s.armAuthPark(ctx, err)
	return s.parkAndRearm(ctx, "from parked state (permanent error)", budget)
}

// Records the OAuth credentials an auth failure park failed with so discovery waits for a re-authorization
func (s *serverTask) armAuthPark(ctx context.Context, err error) {
	s.mu.Lock()
	oauth := s.authType == "oauth"
	s.mu.Unlock()
	if !oauth || !isAuthError(err) {
		return
	}
	storage := NewTokenStorage(ctx, s.name, "")
	park := &authParkState{storage: storage, stamp: storage.CredentialStamp()}
	s.mu.Lock()
	s.authPark = park
	s.mu.Unlock()
}

// Starts the run goroutine on the MCP loop and waits until it is ready or failed
func (s *serverTask) start(ctx context.Context, r *Rendered) error {
	s.mu.Lock()
	s.running = true
	s.mu.Unlock()
	err := spawnOnLoop(ctx, func(loopCtx context.Context) {
		runCtx, cancel := context.WithCancel(loopCtx)
		s.mu.Lock()
		s.cancel = cancel
		s.mu.Unlock()
		defer close(s.done)
		defer cancel()
		s.run(runCtx, r)
	})
	if err != nil {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
		close(s.done)
		return err
	}
	select {
	case <-s.ready.Wait():
	case <-s.done:
		if !s.ready.IsSet() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.err != nil {
				return s.err
			}
			return errors.New("MCP server task exited before becoming ready")
		}
	case <-ctx.Done():
		s.cancelTask()
		return ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Cancels the run goroutine
func (s *serverTask) cancelTask() {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Signals the run goroutine to exit and waits for clean teardown, cancelling it after the grace period
func (s *serverTask) shutdown(ctx context.Context) {
	s.shutdownEv.Set()
	s.reconnect.Set()
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if running {
		t := time.NewTimer(shutdownWait)
		select {
		case <-s.done:
		case <-t.C:
			mcpLogger().Warn(fmt.Sprintf("MCP server '%s' shutdown timed out, cancelling task", s.name))
			s.cancelTask()
			<-s.done
		}
		t.Stop()
	}
	s.mu.Lock()
	cancelRefresh := s.refreshCancel
	s.mu.Unlock()
	cancelRefresh()
	s.refreshWG.Wait()
	s.mu.Lock()
	s.refreshCtx, s.refreshCancel = context.WithCancel(context.Background())
	s.mu.Unlock()
	s.deregisterTools(ctx)
	s.setSession(nil)
}

// Drops every tool this server registered from the registry
func (s *serverTask) deregisterTools(ctx context.Context) {
	for _, name := range s.registeredToolNames() {
		deregisterMCPToolAllScopes(ctx, s, name)
	}
	s.mu.Lock()
	s.toolNames = nil
	s.mu.Unlock()
}
