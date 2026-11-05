package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Uniform jitter applied to reconnect backoff so servers that lost one backend do not retry in lockstep
const backoffJitter = 0.2

// Seconds with plus or minus twenty percent uniform jitter, floored at zero
func jittered(seconds float64) float64 {
	return math.Max(0, seconds*(1-backoffJitter+rand.Float64()*2*backoffJitter))
}

// Credential shapes stripped from error text before it reaches the model
var credentialPattern = regexp.MustCompile(`(?i)(?:ghp_[A-Za-z0-9_]{1,255}|sk-[A-Za-z0-9_-](?:\.?[A-Za-z0-9_-]){0,254}|Bearer\s+\S+` +
	`|(?:token|key|API_KEY|password|secret)=[^\s&,;"']{1,255})`)

// Text with credential like patterns replaced by [REDACTED]
func sanitizeError(text string) string {
	return credentialPattern.ReplaceAllString(text, "[REDACTED]")
}

// Non empty text of an error, its Go syntax form when the message is blank
func excStr(err error) string {
	if err == nil {
		return ""
	}
	if text := strings.TrimSpace(err.Error()); text != "" {
		return text
	}
	return fmt.Sprintf("%#v", err)
}

// ServerKey is a connection key, the bare name outside a multiplexer and the owning scope with the name under one
type ServerKey struct {
	Scope string
	Name  string
}

// Reports whether the key carries an owning registry scope
func (k ServerKey) Scoped() bool { return k.Scope != "" }

// Connection key of a server owned by scope, the current registry scope when current and scope is empty
func serverKey(ctx context.Context, name, scope string, current bool) ServerKey {
	if scope == "" && current {
		scope = mcpRegistryScope(ctx)
	}
	return ServerKey{Scope: scope, Name: name}
}

// Reports whether the connection under key serves scope, owned by it or adopted into it, caller holds mcpMu
func keyVisibleInScope(k ServerKey, scope string) bool {
	if scope == "" {
		return true
	}
	return k.Scope == scope || serverToolScopes[k][scope]
}

// Key a call from scope must use, its own, else an adopted shared one, else its own, caller holds mcpMu
func resolveServerKey(ctx context.Context, name, scope string, current bool) ServerKey {
	if scope == "" && current {
		scope = mcpRegistryScope(ctx)
	}
	own := ServerKey{Scope: scope, Name: name}
	if scope == "" {
		return own
	}
	if _, ok := mcpServers[own]; ok {
		return own
	}
	if _, ok := lazyServerConfigs[own]; ok {
		return own
	}
	for k, scopes := range serverToolScopes {
		if scopes[scope] && k.Name == name {
			if _, live := mcpServers[k]; live {
				return k
			}
		}
	}
	return own
}

// keyedLedger is one connection keyed map or set whose bare keys move under a scope when the process multiplexes
type keyedLedger interface {
	rekeyUnscoped(scope string)
}

// keyedMap is a ledger of values by connection key
type keyedMap[V any] map[ServerKey]V

// Moves every bare key under the scope
func (m keyedMap[V]) rekeyUnscoped(scope string) {
	for k, v := range m {
		if k.Scoped() {
			continue
		}
		delete(m, k)
		m[ServerKey{Scope: scope, Name: k.Name}] = v
	}
}

// keyedSet is a ledger of connection keys
type keyedSet map[ServerKey]struct{}

// Moves every bare key under the scope
func (s keyedSet) rekeyUnscoped(scope string) {
	for k := range s {
		if k.Scoped() {
			continue
		}
		delete(s, k)
		s[ServerKey{Scope: scope, Name: k.Name}] = struct{}{}
	}
}

// Every ledger keyed by connection key, in the order the bare keys move
func keyedLedgers() []keyedLedger {
	return []keyedLedger{
		mcpServers, serverScopeKeys, serverToolScopes, serverConnectErrors, lazyServerConfigs, lazyServerFingerprints,
		lazyServerToolNames, serverConnectRetryAfter, serverConnectFailures, serverErrorCounts, serverBreakerOpenedAt,
		serverErrorsAllApplication, serverTrustLevels, toolReadOnlyHints, serverCostPolicies,
		serverConnecting, parallelSafeServers,
	}
}

// Moves every bare name connection, its owner and its tools under scope once a single profile process starts multiplexing
func RekeyUnscopedConnections(scope string) {
	var toolNames []string
	mcpMu.Lock()
	for _, l := range keyedLedgers() {
		l.rekeyUnscoped(scope)
	}
	for k, owner := range serverScopeKeys {
		if owner == "" && k.Scope == scope {
			serverScopeKeys[k] = scope
			if srv := mcpServers[k]; srv != nil {
				toolNames = append(toolNames, srv.registeredToolNames()...)
			}
		}
	}
	for k, names := range lazyServerToolNames {
		if k.Scope == scope {
			toolNames = append(toolNames, names...)
		}
	}
	touch := func(k ServerKey) {
		if k.Scope != scope {
			return
		}
		if serverToolScopes[k] == nil {
			serverToolScopes[k] = map[string]bool{}
		}
		serverToolScopes[k][scope] = true
	}
	for k := range mcpServers {
		touch(k)
	}
	for k := range lazyServerToolNames {
		touch(k)
	}
	mcpMu.Unlock()
	rekeyUnscopedCosts(scope)
	tools.Default.MoveToScope(toolNames, scope)
}

// event is a settable flag goroutines wait on, the asyncio Event of the server tasks
type event struct {
	mu  sync.Mutex
	ch  chan struct{}
	set bool
}

// Fresh unset event
func newEvent() *event { return &event{ch: make(chan struct{})} }

// Sets the flag and wakes every waiter
func (e *event) Set() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.set {
		e.set = true
		close(e.ch)
	}
}

// Clears the flag so later waiters block again
func (e *event) Clear() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.set {
		e.set = false
		e.ch = make(chan struct{})
	}
}

// Reports whether the flag is set
func (e *event) IsSet() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.set
}

// Channel closed once the flag is set
func (e *event) Wait() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ch
}

// rpcMutex serializes RPCs on one server, acquirable under a context and observable while held
type rpcMutex struct {
	once sync.Once
	ch   chan struct{}
}

// Lazily built one slot channel
func (m *rpcMutex) slot() chan struct{} {
	m.once.Do(func() { m.ch = make(chan struct{}, 1) })
	return m.ch
}

// Acquires the mutex or returns the context error when it ends first
func (m *rpcMutex) Lock(ctx context.Context) error {
	select {
	case m.slot() <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Releases the mutex
func (m *rpcMutex) Unlock() { <-m.slot() }

// Reports whether an RPC holds the mutex
func (m *rpcMutex) Locked() bool { return len(m.slot()) == 1 }

// Seconds a final shutdown spends cancelling loop tasks before giving up on resistant ones, swapped by tests
var mcpLoopDrainTimeout = 3 * time.Second

// Seconds a stop waits for the loop goroutine to exit after the drain
const mcpLoopJoinTimeout = 5 * time.Second

// Error a task scheduled onto a stopped loop fails with, suppressed by the loop exception handler
var errLoopClosed = errors.New("MCP event loop is closed")

// mcpLoop is the task group every MCP server goroutine runs in plus the serial callback executor owning it
type mcpLoop struct {
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	wg       sync.WaitGroup
	pending  int
	stopped  bool
	ready    []func()
	wake     *sync.Cond
	stopping bool
	running  bool
	closed   bool
	exited   chan struct{}
}

// Builds a loop and starts its executor goroutine
func newMCPLoop() *mcpLoop {
	ctx, cancel := context.WithCancel(context.Background())
	l := &mcpLoop{ctx: ctx, cancel: cancel}
	l.wake = sync.NewCond(&l.mu)
	exited := l.markRunning()
	go l.runForever(exited)
	return l
}

// Flags the executor running and returns the channel its run closes on exit
func (l *mcpLoop) markRunning() chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.running = true
	l.exited = make(chan struct{})
	return l.exited
}

// Runs queued callbacks one batch at a time until a stop lands, finishing the batch that holds it
func (l *mcpLoop) runForever(exited chan struct{}) {
	defer close(exited)
	for {
		l.mu.Lock()
		for len(l.ready) == 0 && !l.stopping {
			l.wake.Wait()
		}
		batch := l.ready
		l.ready = nil
		l.mu.Unlock()
		for _, cb := range batch {
			runLoopCallback(cb)
		}
		l.mu.Lock()
		if l.stopping {
			l.stopping, l.running = false, false
			l.mu.Unlock()
			return
		}
		l.mu.Unlock()
	}
}

// Runs one callback, routing a panic through the loop exception handler
func runLoopCallback(cb func()) {
	defer func() {
		if r := recover(); r != nil {
			loopExceptionHandler(r)
		}
	}()
	cb()
}

// Queues cb behind every earlier callback and task start, an error once the loop is closed
func (l *mcpLoop) callSoon(cb func()) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return errLoopClosed
	}
	l.ready = append(l.ready, cb)
	l.wake.Signal()
	return nil
}

// Makes the executor exit after the batch it is running
func (l *mcpLoop) stop() {
	l.mu.Lock()
	l.stopping = true
	l.wake.Signal()
	l.mu.Unlock()
}

// True while the executor goroutine runs callbacks
func (l *mcpLoop) isRunning() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.running
}

// True once the loop was closed
func (l *mcpLoop) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// Channel the latest executor run closes when it exits
func (l *mcpLoop) exitedChan() chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.exited
}

// Closes a stopped loop and drops its queued callbacks, an error while the executor still runs
func (l *mcpLoop) close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running {
		return errors.New("Cannot close a running event loop")
	}
	if l.closed {
		return nil
	}
	l.closed = true
	l.ready = nil
	return nil
}

// Outcome of a scheduled drain, the error carrying a panic the drain raised
type drainResult struct{ err error }

// Queues the drain then the stop as one loop owned sequence, the channel delivering the drain outcome
func (l *mcpLoop) scheduleDrainAndStop() (<-chan drainResult, error) {
	done := make(chan drainResult, 1)
	err := l.callSoon(func() {
		go func() {
			var res drainResult
			defer func() {
				if r := recover(); r != nil {
					res.err = fmt.Errorf("%v", r)
				}
				_ = l.callSoon(l.stop)
				done <- res
			}()
			drainMCPLoopTasks(l, mcpLoopDrainTimeout)
		}()
	})
	if err != nil {
		return nil, err
	}
	return done, nil
}

// Loop the MCP tasks run in, guarded by mcpMu
var mcpLoopState *mcpLoop

// Logs an unhandled task panic, the benign closed loop error excepted
func loopExceptionHandler(r any) {
	if err, ok := r.(error); ok && errors.Is(err, errLoopClosed) {
		return
	}
	mcpLogger().Error(fmt.Sprintf("Unhandled exception in MCP loop task: %v", r))
}

// Queues the start of fn as a loop task behind earlier callbacks, false when the loop stopped accepting work
func (l *mcpLoop) spawn(ctx context.Context, fn func(ctx context.Context)) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopped || l.closed {
		return false
	}
	l.pending++
	l.wg.Add(1)
	l.ready = append(l.ready, func() { go l.runTask(ctx, fn) })
	l.wake.Signal()
	return true
}

// Runs one task body and settles the pending count however it ends
func (l *mcpLoop) runTask(ctx context.Context, fn func(ctx context.Context)) {
	defer func() {
		if r := recover(); r != nil {
			loopExceptionHandler(r)
		}
		l.mu.Lock()
		l.pending--
		l.mu.Unlock()
		l.wg.Done()
	}()
	fn(ctx)
}

// Starts a fresh MCP loop unless one is already running
func ensureMCPLoop() {
	mcpMu.Lock()
	defer mcpMu.Unlock()
	if mcpLoopState != nil && mcpLoopState.isRunning() {
		return
	}
	mcpLoopState = newMCPLoop()
}

// Loop when it is running, nil otherwise
func runningLoop() *mcpLoop {
	mcpMu.Lock()
	l := mcpLoopState
	mcpMu.Unlock()
	if l == nil || !l.isRunning() {
		return nil
	}
	return l
}

// Starts a long lived task on the running loop, an error when the loop is down
func spawnOnLoop(ctx context.Context, fn func(ctx context.Context)) error {
	l := runningLoop()
	if l == nil {
		return errors.New("MCP event loop is not running")
	}
	taskCtx := wrapWithDashboardOAuthFlow(ctx, wrapWithHomeOverride(ctx, newLoopTaskContext(ctx, l.ctx)))
	if !l.spawn(taskCtx, fn) {
		return errors.New("MCP event loop unavailable (failed to schedule)")
	}
	return nil
}

// Lives as long as the loop and reads every caller value except the worker identity
type loopTaskContext struct {
	context.Context
	values context.Context
}

// Loop task context holding a copy of the caller's values like a Python task scheduled from another thread
func newLoopTaskContext(caller, loop context.Context) context.Context {
	values := tools.WithActingFor(tools.WithWorker(context.WithoutCancel(caller), 0), 0)
	return loopTaskContext{Context: loop, values: values}
}

// Caller's value under key, the worker identity reading as the loop's own
func (c loopTaskContext) Value(key any) any { return c.values.Value(key) }

// Runs f once the loop context ends so derived contexts need no watcher goroutine
func (c loopTaskContext) AfterFunc(f func()) func() bool { return context.AfterFunc(c.Context, f) }

// Task context carrying the caller's home override, the task context itself when there is none
func wrapWithHomeOverride(caller, task context.Context) context.Context {
	override := home.Override(caller)
	if override == "" {
		return task
	}
	return home.WithOverride(task, override)
}

// Task context carrying the caller's dashboard OAuth flow, the task context itself when there is none
func wrapWithDashboardOAuthFlow(caller, task context.Context) context.Context {
	flow := dashboardOAuthFlowFrom(caller)
	if flow == nil {
		return task
	}
	return withDashboardOAuthFlow(task, flow)
}

// ErrInterrupted ends a loop call when the user sent a new message
var ErrInterrupted = errors.New("User sent a new message")

// LoopTimeoutError is a loop call that outlived its configured timeout
type LoopTimeoutError struct {
	Elapsed    time.Duration
	Configured time.Duration
}

func (e *LoopTimeoutError) Error() string {
	return fmt.Sprintf("MCP call timed out after %.1fs (configured timeout: %.1fs)", e.Elapsed.Seconds(), e.Configured.Seconds())
}

// Marks the error as a timeout for callers that test for one
func (e *LoopTimeoutError) Timeout() bool { return true }

// NoTimeout lets a loop call run until it finishes or is interrupted
const NoTimeout time.Duration = -1

// Interval the loop call wait polls the interrupt flag at
const loopPollInterval = 100 * time.Millisecond

// Runs fn on the loop with the caller home override and OAuth flow, polling interrupts until done or timed out
func runOnLoop[T any](ctx context.Context, timeout time.Duration, fn func(ctx context.Context) (T, error)) (T, error) {
	var zero T
	l := runningLoop()
	if l == nil {
		return zero, errors.New("MCP event loop is not running")
	}
	taskCtx, cancel := context.WithCancel(wrapWithDashboardOAuthFlow(ctx, wrapWithHomeOverride(ctx, newLoopTaskContext(ctx, l.ctx))))
	type outcome struct {
		v   T
		err error
	}
	done := make(chan outcome, 1)
	if !l.spawn(taskCtx, func(c context.Context) {
		v, err := fn(c)
		done <- outcome{v, err}
	}) {
		cancel()
		mcpLogger().Debug(fmt.Sprintf("MCP scheduling failed: %s", errLoopClosed))
		return zero, errors.New("MCP event loop unavailable (failed to schedule)")
	}
	start := time.Now()
	ticker := time.NewTicker(loopPollInterval)
	defer ticker.Stop()
	var deadline <-chan time.Time
	if timeout >= 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		deadline = timer.C
	}
	for {
		select {
		case o := <-done:
			cancel()
			return o.v, o.err
		case <-deadline:
			select {
			case o := <-done:
				cancel()
				return o.v, o.err
			default:
			}
			cancel()
			return zero, &LoopTimeoutError{Elapsed: time.Since(start), Configured: timeout}
		case <-ctx.Done():
			cancel()
			return zero, ctx.Err()
		case <-ticker.C:
			if tools.IsInterrupted(ctx) {
				cancel()
				return zero, ErrInterrupted
			}
		}
	}
}

// Cancels every task still pending on the loop and waits for them, bounded so a resistant task cannot hang exit
func drainMCPLoopTasks(l *mcpLoop, timeout time.Duration) {
	l.mu.Lock()
	l.stopped = true
	pending := l.pending
	l.mu.Unlock()
	if pending == 0 {
		l.cancel()
		return
	}
	mcpLogger().Debug(fmt.Sprintf("Draining %d pending task(s) from the MCP loop", pending))
	l.cancel()
	finished := make(chan struct{})
	go func() {
		l.wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(timeout):
		l.mu.Lock()
		still := l.pending
		l.mu.Unlock()
		mcpLogger().Warn(fmt.Sprintf("%d MCP loop task(s) still pending after %.1fs drain", still, timeout.Seconds()))
	}
}

// Stops the loop after draining its tasks and reaps every MCP child, false when onlyIfIdle and servers remain
func stopMCPLoop(onlyIfIdle bool) bool {
	mcpMu.Lock()
	if onlyIfIdle && (len(mcpServers) > 0 || len(serverConnecting) > 0) {
		mcpMu.Unlock()
		mcpLogger().Debug("Leaving MCP event loop running; active servers are registered or connecting")
		return false
	}
	l := mcpLoopState
	mcpLoopState = nil
	mcpMu.Unlock()
	if l == nil {
		return true
	}
	var future <-chan drainResult
	if l.isRunning() {
		done, err := l.scheduleDrainAndStop()
		if err != nil {
			mcpLogger().Warn(fmt.Sprintf("MCP loop drain: failed to schedule: %s", excStr(err)))
		} else {
			future = done
			wait := mcpLoopDrainTimeout + time.Second
			select {
			case res := <-done:
				if res.err != nil {
					mcpLogger().Warn(fmt.Sprintf("Error draining MCP loop tasks: %s", excStr(res.err)))
				}
			case <-time.After(wait):
				mcpLogger().Warn(fmt.Sprintf("Timed out waiting for MCP loop drain after %.1fs", wait.Seconds()))
			}
		}
	} else if !l.isClosed() {
		if done, err := l.scheduleDrainAndStop(); err != nil {
			mcpLogger().Warn(fmt.Sprintf("Error draining stopped MCP loop tasks: %s", excStr(err)))
		} else {
			l.runForever(l.markRunning())
			if res := <-done; res.err != nil {
				mcpLogger().Warn(fmt.Sprintf("Error draining stopped MCP loop tasks: %s", excStr(res.err)))
			}
		}
	}
	if future == nil && l.isRunning() {
		_ = l.callSoon(l.stop)
	}
	select {
	case <-l.exitedChan():
	case <-time.After(mcpLoopJoinTimeout):
		mcpLogger().Warn(fmt.Sprintf("MCP event loop thread did not stop within %.1fs", mcpLoopJoinTimeout.Seconds()))
	}
	if err := l.close(); err != nil {
		mcpLogger().Warn(fmt.Sprintf("Unable to close MCP event loop cleanly: %s", excStr(err)))
	}
	killOrphanedMCPChildren(true, "")
	return true
}

// LockCookie holds the cross process discovery lock until released
type LockCookie struct {
	fh *os.File
}

// Drops the lock, an unlock or close failure never propagating
func (c *LockCookie) Release() {
	if c == nil || c.fh == nil {
		return
	}
	_ = unlockFile(c.fh)
	_ = c.fh.Close()
	c.fh = nil
}

// Outcome of a discovery lock attempt
type lockOutcome int

const (
	lockAcquired lockOutcome = iota
	lockHeld
	lockUnavailable
)

var (
	discoveryLockMu   sync.Mutex
	discoveryLockPath string
)

// Non blocking exclusive lock on an open file, false when another process holds it
func acquireLockOnFH(fh *os.File) (bool, error) { return lockFile(fh) }

// Cross process discovery lock of the active home, held elsewhere, or unavailable so discovery runs unguarded
func tryAcquireDiscoveryLock(ctx context.Context) (*LockCookie, lockOutcome) {
	var lockPath string
	if home.Override(ctx) != "" {
		lockPath = filepath.Join(home.Dir(ctx), ".mcp-discovery.lock")
	} else {
		discoveryLockMu.Lock()
		if discoveryLockPath == "" {
			discoveryLockPath = filepath.Join(home.Dir(ctx), ".mcp-discovery.lock")
		}
		lockPath = discoveryLockPath
		discoveryLockMu.Unlock()
	}
	fh, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, lockUnavailable
	}
	acquired, err := acquireLockOnFH(fh)
	if err != nil {
		fh.Close()
		return nil, lockUnavailable
	}
	if acquired {
		return &LockCookie{fh: fh}, lockAcquired
	}
	fh.Close()
	return nil, lockHeld
}

// reconnectable is the server task surface reconnect signalling drives
type reconnectable interface {
	reconnectEvent() *event
	readyEvent() *event
	currentSession() *Session
}

// Asks a server task to rebuild its transport through the loop when it runs, false without reconnect machinery
func signalReconnect(srv reconnectable) bool {
	ev := srv.reconnectEvent()
	if ev == nil {
		return false
	}
	if l := runningLoop(); l == nil || l.callSoon(ev.Set) != nil {
		ev.Set()
	}
	return true
}

// Asks a live server to rebuild after external re-auth, false when no such server is connected
func ReconnectServer(ctx context.Context, name string) bool {
	mcpMu.Lock()
	srv := mcpServers[resolveServerKey(ctx, name, "", true)]
	mcpMu.Unlock()
	return srv != nil && signalReconnect(srv)
}

// Pause between session readiness polls, swapped by tests
var readinessPoll = func() { time.Sleep(250 * time.Millisecond) }

// Polls until the server exposes a ready session different from old, bounded by iterations rather than a deadline
func waitForServerSessionReady(srv reconnectable, old *Session, timeout time.Duration) bool {
	iterations := max(1, int(math.Max(timeout.Seconds(), 0)/0.25))
	for i := 0; i < iterations; i++ {
		session := srv.currentSession()
		ready := srv.readyEvent()
		isReady := ready == nil || ready.IsSet()
		if session != nil && session != old && isReady {
			return true
		}
		if i < iterations-1 {
			readinessPoll()
		}
	}
	return false
}

// Requests a transport rebuild with readiness cleared first and waits for the fresh session
func signalReconnectAndWait(serverName string, srv reconnectable, opDescription string, timeout time.Duration) bool {
	l := runningLoop()
	if l == nil {
		return false
	}
	old := srv.currentSession()
	mcpLogger().Info(fmt.Sprintf("MCP server '%s': %s requesting transport reconnect", serverName, opDescription))
	if l.callSoon(func() {
		if ready := srv.readyEvent(); ready != nil {
			ready.Clear()
		}
		if ev := srv.reconnectEvent(); ev != nil {
			ev.Set()
		}
	}) != nil {
		return false
	}
	return waitForServerSessionReady(srv, old, timeout)
}

// Sampler answers a server's sampling/createMessage requests
type Sampler interface {
	Capabilities() map[string]any
	CreateMessage(ctx context.Context, params *CreateMessageParams) (any, *RPCError)
}

// Elicitor answers a server's elicitation/create requests
type Elicitor interface {
	Capabilities() map[string]any
	Elicit(ctx context.Context, params *ElicitParams) (*ElicitResult, *RPCError)
}

// SessionOptions configure one client session
type SessionOptions struct {
	Name               string
	Env                *tools.Env
	Sampler            Sampler
	Elicitor           Elicitor
	OnToolsListChanged func()
}

// Session is one JSON-RPC client session over a transport, the ClientSession of the server task
type Session struct {
	t        Transport
	opts     SessionOptions
	mu       sync.Mutex
	pending  map[string]chan *Message
	inflight map[string]context.CancelFunc
	progress map[string]func(ProgressParams)
	result   *InitializeResult
	version  string
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	err      error
}

// Code the client reports when the transport closed under a pending request
const codeConnectionClosed = -32000

// Starts a session reading the transport's inbound messages
func NewSession(t Transport, opts SessionOptions) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{t: t, opts: opts, pending: map[string]chan *Message{}, inflight: map[string]context.CancelFunc{},
		progress: map[string]func(ProgressParams){}, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	go s.read()
	return s
}

// Name of the server the session talks to
func (s *Session) Name() string { return s.opts.Name }

// Handshake or discovery result the session adopted, nil before negotiation
func (s *Session) InitializeResult() *InitializeResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result
}

// Protocol revision later requests are shaped for
func (s *Session) ProtocolVersion() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

// Channel closed once the transport ended and every pending request failed
func (s *Session) Done() <-chan struct{} { return s.done }

// Why the session ended, nil while it runs
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Pumps inbound messages until the transport closes, then fails every pending request
func (s *Session) read() {
	for msg := range s.t.Receive() {
		switch {
		case msg.IsResponse():
			s.mu.Lock()
			ch := s.pending[IDKey(msg.ID)]
			delete(s.pending, IDKey(msg.ID))
			s.mu.Unlock()
			if ch != nil {
				ch <- msg
			} else {
				mcpLogger().Debug(fmt.Sprintf("MCP server '%s': response for unknown request id %s", s.opts.Name, string(msg.ID)))
			}
		case msg.IsRequest():
			reqCtx, cancel := context.WithCancel(s.ctx)
			s.mu.Lock()
			s.inflight[IDKey(msg.ID)] = cancel
			s.mu.Unlock()
			go s.handleRequest(reqCtx, msg)
		case msg.IsNotification():
			s.handleNotification(msg)
		}
	}
	err := s.t.Err()
	closed := &RPCError{Code: codeConnectionClosed, Message: "Connection closed"}
	s.mu.Lock()
	if err != nil {
		s.err = fmt.Errorf("%w: %v", closed, err)
	} else {
		s.err = closed
	}
	pending := s.pending
	s.pending = map[string]chan *Message{}
	s.mu.Unlock()
	for _, ch := range pending {
		close(ch)
	}
	s.cancel()
	close(s.done)
}

// Answers one server to client request on its own goroutine
func (s *Session) handleRequest(ctx context.Context, msg *Message) {
	defer func() {
		s.mu.Lock()
		if cancel := s.inflight[IDKey(msg.ID)]; cancel != nil {
			cancel()
		}
		delete(s.inflight, IDKey(msg.ID))
		s.mu.Unlock()
	}()
	var reply *Message
	switch msg.Method {
	case methodPing:
		reply, _ = NewResponse(msg.ID, map[string]any{})
	case methodSamplingCreateMessage:
		reply = s.answerSampling(ctx, msg)
	case methodElicitationCreate:
		reply = s.answerElicitation(ctx, msg)
	default:
		reply = NewErrorResponse(msg.ID, CodeMethodNotFound, "Method not found")
	}
	if ctx.Err() != nil {
		return
	}
	if err := s.t.Send(s.ctx, reply); err != nil {
		mcpLogger().Debug(fmt.Sprintf("MCP server '%s': failed to answer %s: %v", s.opts.Name, msg.Method, err))
	}
}

// Sampling answer from the configured sampler, invalid request when the client offers none
func (s *Session) answerSampling(ctx context.Context, msg *Message) *Message {
	if s.opts.Sampler == nil {
		return NewErrorResponse(msg.ID, CodeInvalidRequest, "Sampling not supported")
	}
	var params CreateMessageParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return NewErrorResponse(msg.ID, CodeInvalidParams, "Invalid request parameters")
	}
	result, rpcErr := s.opts.Sampler.CreateMessage(ctx, &params)
	if rpcErr != nil {
		return &Message{JSONRPC: "2.0", ID: msg.ID, Error: rpcErr}
	}
	reply, err := NewResponse(msg.ID, result)
	if err != nil {
		return NewErrorResponse(msg.ID, CodeInternalError, err.Error())
	}
	return reply
}

// Elicitation answer from the configured elicitor, invalid request when the client offers none
func (s *Session) answerElicitation(ctx context.Context, msg *Message) *Message {
	if s.opts.Elicitor == nil {
		return NewErrorResponse(msg.ID, CodeInvalidRequest, "Elicitation not supported")
	}
	var params ElicitParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return NewErrorResponse(msg.ID, CodeInvalidParams, "Invalid request parameters")
	}
	result, rpcErr := s.opts.Elicitor.Elicit(ctx, &params)
	if rpcErr != nil {
		return &Message{JSONRPC: "2.0", ID: msg.ID, Error: rpcErr}
	}
	reply, err := NewResponse(msg.ID, result)
	if err != nil {
		return NewErrorResponse(msg.ID, CodeInternalError, err.Error())
	}
	return reply
}

// Routes list changes, server log messages, progress and cancellations of server requests
func (s *Session) handleNotification(msg *Message) {
	switch msg.Method {
	case methodToolsListChanged:
		mcpLogger().Info(fmt.Sprintf("MCP server '%s': received tools/list_changed notification", s.opts.Name))
		if s.opts.OnToolsListChanged != nil {
			s.opts.OnToolsListChanged()
		}
	case methodPromptsListChanged:
		mcpLogger().Debug(fmt.Sprintf("MCP server '%s': prompts/list_changed (ignored)", s.opts.Name))
	case methodResourcesListChanged:
		mcpLogger().Debug(fmt.Sprintf("MCP server '%s': resources/list_changed (ignored)", s.opts.Name))
	case methodLoggingMessage:
		s.onServerLog(msg.Params)
	case methodProgress:
		var p ProgressParams
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return
		}
		s.mu.Lock()
		sink := s.progress[IDKey(p.ProgressToken)]
		s.mu.Unlock()
		if sink != nil {
			sink(p)
		}
	case methodCancelled:
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(msg.Params, &p) == nil && len(p.RequestID) > 0 {
			s.mu.Lock()
			if cancel := s.inflight[IDKey(p.RequestID)]; cancel != nil {
				cancel()
			}
			s.mu.Unlock()
		}
	}
}

// Characters of one server log payload kept before the truncation marker
const serverLogCap = 2000

// Forwards a server notifications/message into the log tagged with the server and its logger name
func (s *Session) onServerLog(raw json.RawMessage) {
	var p struct {
		Level  any             `json:"level"`
		Logger any             `json:"logger"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		mcpLogger().Debug(fmt.Sprintf("Failed to handle MCP log notification from '%s'", s.opts.Name), "error", err)
		return
	}
	levelName := "info"
	if p.Level != nil {
		levelName = strings.ToLower(pyStr(p.Level))
	}
	level := slog.LevelInfo
	switch logLevelMap[levelName] {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	var data string
	if len(p.Data) == 0 || json.Unmarshal(p.Data, &data) != nil {
		data = pyDumpsRaw(p.Data)
	}
	if r := []rune(data); len(r) > serverLogCap {
		data = string(r[:serverLogCap]) + "... [truncated]"
	}
	origin := s.opts.Name
	if p.Logger != nil && pyTruthy(p.Logger) {
		origin = s.opts.Name + "/" + pyStr(p.Logger)
	}
	mcpLogger().Log(context.Background(), level, fmt.Sprintf("MCP server log [%s]: %s", origin, data))
}

// Python json.dumps rendering of raw JSON with key order kept, spaced separators and no ASCII escaping
func pyDumpsRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "null"
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var b strings.Builder
	var value func() error
	value = func() error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				b.WriteByte('{')
				for i := 0; dec.More(); i++ {
					if i > 0 {
						b.WriteString(", ")
					}
					k, err := dec.Token()
					if err != nil {
						return err
					}
					b.WriteString(pyJSONString(fmt.Sprint(k)))
					b.WriteString(": ")
					if err := value(); err != nil {
						return err
					}
				}
				if _, err := dec.Token(); err != nil {
					return err
				}
				b.WriteByte('}')
			case '[':
				b.WriteByte('[')
				for i := 0; dec.More(); i++ {
					if i > 0 {
						b.WriteString(", ")
					}
					if err := value(); err != nil {
						return err
					}
				}
				if _, err := dec.Token(); err != nil {
					return err
				}
				b.WriteByte(']')
			}
		case string:
			b.WriteString(pyJSONString(t))
		case json.Number:
			text := t.String()
			if strings.ContainsAny(text, ".eE") {
				f, _ := t.Float64()
				text = pyFloatRepr(f)
			}
			b.WriteString(text)
		case bool:
			b.WriteString(strconv.FormatBool(t))
		case nil:
			b.WriteString("null")
		}
		return nil
	}
	if err := value(); err != nil {
		return string(raw)
	}
	return b.String()
}

// JSON string literal the way json.dumps writes it without ASCII escaping
func pyJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Sends one request and waits for its answer, telling the server when the caller gives up
func (s *Session) request(ctx context.Context, msg *Message, out any) error {
	ch := make(chan *Message, 1)
	key := IDKey(msg.ID)
	s.mu.Lock()
	if s.err != nil {
		err := s.err
		s.mu.Unlock()
		return err
	}
	s.pending[key] = ch
	s.mu.Unlock()
	if err := s.t.Send(ctx, msg); err != nil {
		s.mu.Lock()
		delete(s.pending, key)
		s.mu.Unlock()
		return err
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			return s.Err()
		}
		if resp.Error != nil {
			return resp.Error
		}
		if out == nil || len(resp.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("decode %s result: %w", msg.Method, err)
		}
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, key)
		s.mu.Unlock()
		if note, err := NewNotification(methodCancelled, map[string]any{"requestId": msg.ID, "reason": ctx.Err().Error()}); err == nil {
			sendCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.t.Send(sendCtx, note)
			cancel()
		}
		return ctx.Err()
	}
}

// Sends a request built from a method and params
func (s *Session) call(ctx context.Context, method string, params, out any) error {
	msg, err := NewRequest(method, params)
	if err != nil {
		return err
	}
	return s.request(ctx, msg, out)
}

// Sends a notification
func (s *Session) notify(ctx context.Context, method string, params any) error {
	msg, err := NewNotification(method, params)
	if err != nil {
		return err
	}
	return s.t.Send(ctx, msg)
}

// Capabilities this client advertises, sampling and elicitation only when a handler is configured
func (s *Session) capabilities() ClientCapabilities {
	var caps ClientCapabilities
	if s.opts.Sampler != nil {
		caps.Sampling = s.opts.Sampler.Capabilities()
		if caps.Sampling == nil {
			caps.Sampling = map[string]any{}
		}
	}
	if s.opts.Elicitor != nil {
		caps.Elicitation = s.opts.Elicitor.Capabilities()
		if caps.Elicitation == nil {
			caps.Elicitation = map[string]any{}
		}
	}
	return caps
}

// Initialize params offering the newest handshake revision
func (s *Session) initializeParams() InitializeParams {
	return InitializeParams{ProtocolVersion: LatestHandshakeVersion, Capabilities: s.capabilities(),
		ClientInfo: Implementation{Name: ClientName, Version: ClientVersion}}
}

// UnsupportedServerVersionError is an initialize answered with a revision outside the handshake set
type UnsupportedServerVersionError struct {
	Version string
}

func (e *UnsupportedServerVersionError) Error() string {
	return "Unsupported protocol version from the server: " + e.Version
}

// Adopts a negotiated result and pins later requests to version
func (s *Session) adopt(result *InitializeResult, version string) {
	s.mu.Lock()
	s.result = result
	s.version = version
	s.mu.Unlock()
	if v, ok := s.t.(interface{ SetProtocolVersion(string) }); ok {
		v.SetProtocolVersion(version)
	}
}

// Legacy handshake, refusing a server revision outside the supported set, then notifications/initialized
func (s *Session) Initialize(ctx context.Context) (*InitializeResult, error) {
	var result InitializeResult
	if err := s.call(ctx, methodInitialize, s.initializeParams(), &result); err != nil {
		return nil, err
	}
	if !supportedVersion(result.ProtocolVersion) {
		return nil, &UnsupportedServerVersionError{Version: result.ProtocolVersion}
	}
	s.adopt(&result, result.ProtocolVersion)
	if err := s.notify(ctx, methodInitialized, nil); err != nil {
		return nil, err
	}
	return &result, nil
}

// Stateless server/discover probe adopting the capabilities and revision it names
func (s *Session) Discover(ctx context.Context) (*InitializeResult, error) {
	var result InitializeResult
	if err := s.call(ctx, methodDiscover, nil, &result); err != nil {
		return nil, err
	}
	version := result.ProtocolVersion
	if version == "" {
		version = LatestProtocolVersion
	}
	s.adopt(&result, version)
	return &result, nil
}

// Reruns initialize and adopts its result pinned to the offered revision, then sends notifications/initialized
func (s *Session) CompleteHandshakeAtOfferedVersion(ctx context.Context) (*InitializeResult, error) {
	var result InitializeResult
	if err := s.call(ctx, methodInitialize, s.initializeParams(), &result); err != nil {
		return nil, err
	}
	pinned := result
	pinned.ProtocolVersion = LatestHandshakeVersion
	s.adopt(&pinned, LatestHandshakeVersion)
	if err := s.notify(ctx, methodInitialized, nil); err != nil {
		return nil, err
	}
	return &result, nil
}

// handshaker is the session surface protocol negotiation drives
type handshaker interface {
	Initialize(ctx context.Context) (*InitializeResult, error)
	CompleteHandshakeAtOfferedVersion(ctx context.Context) (*InitializeResult, error)
}

// discoverer is a session that can probe server/discover
type discoverer interface {
	Discover(ctx context.Context) (*InitializeResult, error)
}

// Reports whether an error chain carries one of the codes or its text one of the markers
func jsonrpcMatches(err error, codes []int, markers []string) bool {
	code := RPCCode(err)
	var coded interface{ ErrorCode() int }
	if code == 0 && errors.As(err, &coded) {
		code = coded.ErrorCode()
	}
	for _, c := range codes {
		if code != 0 && code == c {
			return true
		}
	}
	text := strings.ToLower(err.Error())
	for _, m := range markers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// Reports whether a JSON-RPC method not found came back, structurally or in any common phrasing
func isMethodNotFoundError(err error) bool {
	return jsonrpcMatches(err, []int{CodeMethodNotFound},
		[]string{strconv.Itoa(CodeMethodNotFound), "method not found", "unknown method", "not found: ping"})
}

// Reports whether a failed initialize signals a stateless only server
func handshakeRejectedAsModern(err error) bool {
	return jsonrpcMatches(err, []int{CodeUnsupportedProtocolVersion, CodeMethodNotFound},
		[]string{"unsupported protocol version", strconv.Itoa(CodeUnsupportedProtocolVersion)}) || isMethodNotFoundError(err)
}

// Reports whether initialize succeeded on the wire but named a revision the handshake refuses
func handshakeAnsweredWithUnsupportedVersion(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "unsupported protocol version from the server")
}

// Reports whether an error is a deadline the negotiation must never fall back from
func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &t) && t.Timeout())
}

// Negotiates the protocol era, auto trying the handshake first and discover only on a modern only signal
func negotiateSession(ctx context.Context, serverName, mode string, s handshaker, connectTimeout time.Duration) (*InitializeResult, error) {
	step := func(name string) func(context.Context) (*InitializeResult, error) {
		if name == "initialize" {
			return s.Initialize
		}
		if d, ok := s.(discoverer); ok {
			return d.Discover
		}
		return func(context.Context) (*InitializeResult, error) {
			return nil, errors.New("session has no server/discover client")
		}
	}
	call := func(name string) (*InitializeResult, error) {
		cctx, cancel := context.WithTimeout(ctx, connectTimeout)
		defer cancel()
		return step(name)(cctx)
	}
	attempt := func(primary, fallback string, shouldFallback func(error) bool, logf func(error)) (*InitializeResult, error) {
		res, err := call(primary)
		if err == nil {
			return res, nil
		}
		if isTimeout(err) || !shouldFallback(err) {
			return nil, err
		}
		logf(err)
		res, fallbackErr := call(fallback)
		if fallbackErr == nil {
			return res, nil
		}
		if isTimeout(fallbackErr) || primary != "initialize" || !handshakeAnsweredWithUnsupportedVersion(err) {
			return nil, fallbackErr
		}
		mcpLogger().Info(fmt.Sprintf("MCP server '%s': server/discover also failed (%v) \u2014 completing the handshake at %s, the version this client offered",
			serverName, fallbackErr, LatestHandshakeVersion))
		cctx, cancel := context.WithTimeout(ctx, connectTimeout)
		defer cancel()
		return s.CompleteHandshakeAtOfferedVersion(cctx)
	}
	switch mode {
	case "stateless", "modern", "2026-07-28":
		return attempt("discover", "initialize", func(error) bool { return true }, func(err error) {
			mcpLogger().Info(fmt.Sprintf("MCP server '%s': server/discover rejected (%v) despite protocol=%s \u2014 falling back to the legacy handshake", serverName, err, mode))
		})
	case "legacy", "handshake":
		return call("initialize")
	case "auto":
	default:
		mcpLogger().Warn(fmt.Sprintf("MCP server '%s': unknown protocol=%s \u2014 treating as 'auto' (valid: auto, stateless, legacy)", serverName, pyReprStr(mode)))
	}
	_, canDiscover := s.(discoverer)
	return attempt("initialize", "discover", func(err error) bool { return handshakeRejectedAsModern(err) && canDiscover }, func(err error) {
		mcpLogger().Info(fmt.Sprintf("MCP server '%s': legacy handshake rejected (%v) \u2014 retrying via server/discover (2026-07-28 stateless server)", serverName, err))
	})
}

// Negotiates this session per the configured protocol mode under the connect timeout
func (s *Session) Negotiate(ctx context.Context, mode string, connectTimeout time.Duration) (*InitializeResult, error) {
	return negotiateSession(ctx, s.opts.Name, mode, s, connectTimeout)
}

// Ping round trip, the optional utility a keepalive probes
func (s *Session) Ping(ctx context.Context) error {
	return s.call(ctx, methodPing, nil, nil)
}

// Pages a paginated list may follow so a forever cursor cannot spin discovery
const listMaxPages = 50

// CacheHints are the SEP-2549 hints the first page of a list carries
type CacheHints struct {
	TTLMs      *float64
	CacheScope string
}

// Drains a paginated list by following nextCursor, recording the first page's cache hints, caller holds the rpc mutex
func paginate[T any](ctx context.Context, s *Session, method, itemsAttr string, items func(*listPage) []T, hints *CacheHints) ([]T, error) {
	var out []T
	cursor := ""
	for page := 0; ; page++ {
		if page == listMaxPages {
			mcpLogger().Warn(fmt.Sprintf("MCP server '%s': %s pagination exceeded %d pages; truncating at %d items", s.opts.Name, itemsAttr, listMaxPages, len(out)))
			break
		}
		var params any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}
		var result listPage
		if err := s.call(ctx, method, params, &result); err != nil {
			return nil, err
		}
		if hints != nil && len(out) == 0 {
			if result.TTLMs != nil {
				hints.TTLMs = result.TTLMs
			}
			if result.CacheScope != "" {
				hints.CacheScope = result.CacheScope
			}
		}
		out = append(out, items(&result)...)
		cursor = result.NextCursor
		if cursor == "" {
			break
		}
	}
	return out, nil
}

// Every tool the server lists across pages, the first page's cache hints in hints when given
func (s *Session) ListTools(ctx context.Context, hints *CacheHints) ([]*Tool, error) {
	return paginate(ctx, s, methodToolsList, "tools", func(p *listPage) []*Tool { return p.Tools }, hints)
}

// Every resource the server lists across pages
func (s *Session) ListResources(ctx context.Context) ([]*Resource, error) {
	return paginate(ctx, s, methodResourcesList, "resources", func(p *listPage) []*Resource { return p.Resources }, nil)
}

// Every prompt the server lists across pages
func (s *Session) ListPrompts(ctx context.Context) ([]*Prompt, error) {
	return paginate(ctx, s, methodPromptsList, "prompts", func(p *listPage) []*Prompt { return p.Prompts }, nil)
}

// Contents of one resource
func (s *Session) ReadResource(ctx context.Context, uri string) (*ReadResourceResult, error) {
	var out ReadResourceResult
	if err := s.call(ctx, methodResourcesRead, map[string]any{"uri": uri}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Messages of one prompt rendered with its arguments
func (s *Session) GetPrompt(ctx context.Context, name string, args map[string]string) (*GetPromptResult, error) {
	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	}
	var out GetPromptResult
	if err := s.call(ctx, methodPromptsGet, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ProgressFunc receives one progress update of a running call
type ProgressFunc func(progress float64, total *float64, message string)

// Calls a tool, opting into notifications/progress with the request id as token when a progress sink is given
func (s *Session) CallTool(ctx context.Context, name string, args map[string]any, progress ProgressFunc) (*CallToolResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	id := nextID()
	params := map[string]any{"name": name, "arguments": args}
	if progress != nil {
		params["_meta"] = map[string]any{"progressToken": json.RawMessage(id)}
		key := IDKey(id)
		s.mu.Lock()
		s.progress[key] = func(p ProgressParams) { progress(p.Progress, p.Total, p.Message) }
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			delete(s.progress, key)
			s.mu.Unlock()
		}()
	}
	raw, err := marshalParams(params)
	if err != nil {
		return nil, err
	}
	var out CallToolResult
	if err := s.request(ctx, &Message{JSONRPC: "2.0", ID: id, Method: methodToolsCall, Params: raw}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Closes the transport and waits for the reader to fail pending requests
func (s *Session) Close() error {
	err := s.t.Close()
	<-s.done
	return err
}

// Event kind a running tool's progress reaches the host as
const toolProgressEvent = "tool.progress"

// Minimum spacing between relayed progress updates of one call
const progressMinInterval = 0.5

// Monotonic clock in seconds the progress throttle reads, swapped by tests
var progressNow = func() float64 { return float64(time.Since(processStart)) / float64(time.Second) }

var processStart = time.Now()

// Progress sink relaying a call's updates to the host as throttled tool.progress events, the first and final always passing
func newProgressReporter(env *tools.Env, toolName, toolCallID string) ProgressFunc {
	var mu sync.Mutex
	last := 0.0
	return func(progress float64, total *float64, message string) {
		done := total != nil && *total > 0 && progress >= *total
		now := progressNow()
		mu.Lock()
		if last != 0 && !done && now-last < progressMinInterval {
			mu.Unlock()
			return
		}
		last = now
		mu.Unlock()
		var percent any
		var totalVal any
		if total != nil {
			totalVal = *total
			if *total != 0 {
				percent = int(math.RoundToEven(math.Min(math.Max(progress / *total, 0), 1) * 100))
			}
		}
		defer func() {
			if r := recover(); r != nil {
				mcpLogger().Debug(fmt.Sprintf("tool.progress callback failed for %s", toolName), "panic", r)
			}
		}()
		env.Emit(tools.Event{Kind: toolProgressEvent, Tool: toolName, Text: message, Data: map[string]any{
			"tool_call_id": toolCallID, "progress": progress, "total": totalVal, "percent": percent}})
	}
}
