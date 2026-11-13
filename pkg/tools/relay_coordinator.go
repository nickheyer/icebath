package tools

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
)

// Profile scoped Relay host, the real runtime or the reduced capability no op host
type RelayHost interface {
	ApplyToolRequestIntercepts(ctx context.Context, sessionID, toolName string, args map[string]any) (map[string]any, error)
	RetainManagedExecution(consumer string) error
	ReleaseManagedExecution(consumer string)
	ManagedExecutionEnabled() bool
	Shutdown()
}

// Explicit reduced capability host for platforms without the Relay binding
type NoopRelayRuntime struct {
	ProfileKey string
	Reason     string
}

func (NoopRelayRuntime) ApplyToolRequestIntercepts(_ context.Context, _, _ string, args map[string]any) (map[string]any, error) {
	return args, nil
}
func (NoopRelayRuntime) RetainManagedExecution(string) error { return nil }
func (NoopRelayRuntime) ReleaseManagedExecution(string)      {}
func (NoopRelayRuntime) ManagedExecutionEnabled() bool       { return false }
func (NoopRelayRuntime) Shutdown()                           {}

// Source of profile hosts the coordinator resolves through
type RelayHostSource interface {
	ForProfile(ctx context.Context, profileKey string, create bool) RelayHost
}

// Exactly one Relay host for each canonical icebath profile
type RelayHostRegistry struct {
	mu    sync.Mutex
	hosts *pydict.Dict[string, RelayHost]
}

// Empty host registry
func NewRelayHostRegistry() *RelayHostRegistry {
	return &RelayHostRegistry{hosts: pydict.New[string, RelayHost]()}
}

// Host of the profile, created on first use unless create is false, the no op host when Relay cannot start
func (g *RelayHostRegistry) ForProfile(ctx context.Context, profileKey string, create bool) RelayHost {
	key := profileKey
	if key == "" {
		key = CurrentRelayProfileKey(ctx)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	host, _ := g.hosts.Get(key)
	if host != nil || !create {
		return host
	}
	rt, err := NewRelayRuntime(ctx, nil, key)
	if err != nil {
		relayLogger().WarnContext(ctx, "Icebath Relay runtime initialization failed", "err", err)
		host = NoopRelayRuntime{ProfileKey: key, Reason: err.Error()}
	} else {
		host = rt
	}
	g.hosts.Set(key, host)
	return host
}

// Shuts down every host and empties the registry
func (g *RelayHostRegistry) ShutdownAll() {
	g.mu.Lock()
	hosts := g.hosts.Values()
	g.hosts = pydict.New[string, RelayHost]()
	g.mu.Unlock()
	for _, h := range hosts {
		h.Shutdown()
	}
}

// Process wide host registry
var RelayHosts = NewRelayHostRegistry()

// Resumable reference to one profile scoped conversation scope
type ConversationLease struct {
	ProfileKey      string
	SessionID       string
	Platform        string
	Host            RelayHost
	Session         *RelaySession
	ParentSessionID string
	TurnCwd         string
	released        atomic.Bool
}

// Whether the caller released the lease
func (l *ConversationLease) Released() bool { return l.released.Load() }

// Real Relay host when the lease owns an open session
func (l *ConversationLease) LiveRuntime() *RelayRuntime {
	rt, ok := l.Host.(*RelayRuntime)
	if !ok || l.Session == nil {
		return nil
	}
	return rt
}

// Runtime only context of one icebath turn or top level task
type RelayTurnContext struct {
	Lease            *ConversationLease
	TurnID           string
	TaskID           string
	Handle           any
	LogicalLLMCalls  *pydict.Dict[string, any]
	LogicalLLMMu     sync.Mutex
	finalizeMu       sync.Mutex
	previousTurn     *RelayTurnContext
	activeRegistered bool
	relayEnabled     atomic.Bool
	closed           atomic.Bool
}

// Whether this turn may create Relay instrumentation
func (t *RelayTurnContext) RelayEnabled() bool { return t.relayEnabled.Load() }

// Whether the turn finished
func (t *RelayTurnContext) Closed() bool { return t.closed.Load() }

var (
	relayCurrentTurnVar          = pyctx.NewVar("hermes_relay_turn", (*RelayTurnContext)(nil))
	relayManagedCallbackDepthVar = pyctx.NewVar("hermes_relay_managed_callback_depth", 0)
)

// Context of a managed Relay callback whose transitive work, forwarded copies included, runs unmanaged
func RelayManagedCallbackGuard(ctx context.Context) context.Context {
	inner := pyctx.With(ctx, pyctx.CopyContext(ctx))
	relayManagedCallbackDepthVar.Set(inner, relayManagedCallbackDepthVar.Get(ctx).(int)+1)
	return inner
}

// Runs fail open telemetry work, logging the failure and returning the zero value on error
func relayWarnOnError[T any](ctx context.Context, what string, fn func() (T, error)) T {
	v, err := fn()
	if err != nil {
		relayLogger().WarnContext(ctx, "Icebath Relay "+what+" failed", "err", err)
		var zero T
		return zero
	}
	return v
}

// Deferred guard of a telemetry hook that logs a failure inside the hook as a warning and keeps it from the caller
func relayFailOpen(ctx context.Context, what string) func() {
	return func() {
		if failure := recover(); failure != nil {
			relayLogger().WarnContext(ctx, "Icebath Relay "+what+" failed", "err", failure)
		}
	}
}

// Sets a pending rotation or close flag unless the session is already closing
func relayFlagOpenSession(session *RelaySession, flag *bool) {
	session.Mu.Lock()
	defer session.Mu.Unlock()
	if !session.Closing {
		*flag = true
	}
}

type relayTurnKey struct{ profileKey, sessionID string }

// Session preparation run before scope creation
type RelaySessionInitializer func(ctx context.Context, host *RelayRuntime, context map[string]any) error

// Semantic conversation and turn lifetimes of icebath core
type RelaySessionCoordinator struct {
	Registry      RelayHostSource
	initializerMu sync.Mutex
	initializers  *pydict.Dict[string, RelaySessionInitializer]
	activeTurnsMu sync.Mutex
	activeTurns   map[relayTurnKey]map[*RelayTurnContext]bool
}

// Coordinator over registry
func NewRelaySessionCoordinator(registry RelayHostSource) *RelaySessionCoordinator {
	return &RelaySessionCoordinator{Registry: registry, initializers: pydict.New[string, RelaySessionInitializer](),
		activeTurns: map[relayTurnKey]map[*RelayTurnContext]bool{}}
}

// Registers idempotent profile and session preparation before scope creation
func (c *RelaySessionCoordinator) RegisterSessionInitializer(name string, callback RelaySessionInitializer) {
	c.initializerMu.Lock()
	c.initializers.Set(name, callback)
	c.initializerMu.Unlock()
}

// Runs every registered initializer in order, logging a failed one and going on
func (c *RelaySessionCoordinator) prepareSession(ctx context.Context, host *RelayRuntime, context map[string]any) {
	c.initializerMu.Lock()
	names, callbacks := c.initializers.Keys(), c.initializers.Values()
	c.initializerMu.Unlock()
	for i, callback := range callbacks {
		if err := relayRunInitializer(ctx, callback, host, context); err != nil {
			relayLogger().WarnContext(ctx, "Icebath Relay session initializer failed: "+names[i], "err", err)
		}
	}
}

// Error or recovered panic of one initializer call
func relayRunInitializer(ctx context.Context, callback RelaySessionInitializer, host *RelayRuntime, context map[string]any) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("%v", rec)
		}
	}()
	return callback(ctx, host, context)
}

// Conversation identity and working directories of AcquireConversation
type RelayConversation struct {
	ProfileKey      string
	SessionID       string
	Platform        string
	ParentSessionID string
	Model           string
	SessionCwd      *string
	TurnCwd         *string
}

// Opens or resumes the conversation scope of a session and returns the caller lease
func (c *RelaySessionCoordinator) AcquireConversation(ctx context.Context, o RelayConversation) *ConversationLease {
	host := c.Registry.ForProfile(ctx, o.ProfileKey, true)
	if host == nil || (reflect.ValueOf(host).Kind() == reflect.Pointer && reflect.ValueOf(host).IsNil()) {
		host = NoopRelayRuntime{ProfileKey: o.ProfileKey, Reason: "Relay host creation was disabled"}
	}
	var session *RelaySession
	if rt, ok := host.(*RelayRuntime); ok {
		var cwd any
		if o.SessionCwd != nil {
			cwd = *o.SessionCwd
		}
		context := map[string]any{"profile_key": o.ProfileKey, "session_id": o.SessionID, "platform": o.Platform,
			"parent_session_id": o.ParentSessionID, "model": o.Model, "cwd": cwd}
		session = relayWarnOnError(ctx, "conversation initialization", func() (*RelaySession, error) {
			return c.openConversationSession(ctx, rt, context)
		})
	}
	turnCwd := ""
	switch {
	case o.TurnCwd != nil:
		turnCwd, _ = relayScopeInput(*o.TurnCwd)["cwd"].(string)
	case o.SessionCwd != nil:
		turnCwd, _ = relayScopeInput(*o.SessionCwd)["cwd"].(string)
	case session != nil:
		session.Mu.Lock()
		turnCwd = session.Cwd
		session.Mu.Unlock()
	}
	return &ConversationLease{ProfileKey: o.ProfileKey, SessionID: o.SessionID, Platform: o.Platform, Host: host,
		Session: session, ParentSessionID: o.ParentSessionID, TurnCwd: turnCwd}
}

func (c *RelaySessionCoordinator) openConversationSession(ctx context.Context, host *RelayRuntime, context map[string]any) (*RelaySession, error) {
	c.prepareSession(ctx, host, context)
	var cwd *string
	if s, ok := context["cwd"].(string); ok {
		cwd = &s
	}
	sessionID, _ := context["session_id"].(string)
	parentSessionID, _ := context["parent_session_id"].(string)
	platform, _ := context["platform"].(string)
	if platform == "" {
		platform = "unknown"
	}
	o := RelaySessionOptions{Metadata: map[string]any{"icebath.execution_surface": platform}, Cwd: cwd}
	if parentSessionID != "" && parentSessionID != sessionID {
		return host.RegisterSubagent(ctx, map[string]any{"parent_session_id": parentSessionID, "child_session_id": sessionID}, o)
	}
	return host.EnsureSession(ctx, map[string]any{"session_id": sessionID}, o)
}

// Starts a turn on the lease, pushing its scope unless a concurrent turn owns the session stack
func (c *RelaySessionCoordinator) BeginTurn(ctx context.Context, lease *ConversationLease, turnID, taskID string, metadata map[string]any) (*RelayTurnContext, error) {
	if lease.Released() {
		return nil, errors.New("Icebath Relay conversation lease is released")
	}
	turn := &RelayTurnContext{Lease: lease, TurnID: turnID, TaskID: taskID, LogicalLLMCalls: pydict.New[string, any]()}
	turn.relayEnabled.Store(true)
	key := relayTurnKey{lease.ProfileKey, lease.SessionID}
	c.activeTurnsMu.Lock()
	if len(c.activeTurns[key]) > 0 {
		turn.relayEnabled.Store(false)
		relayLogger().WarnContext(ctx, fmt.Sprintf("Skipping Relay instrumentation for concurrent Icebath turn %s in session %s", turnID, lease.SessionID))
	} else {
		c.activeTurns[key] = map[*RelayTurnContext]bool{turn: true}
		turn.activeRegistered = true
	}
	c.activeTurnsMu.Unlock()
	var host *RelayRuntime
	if turn.RelayEnabled() {
		host = lease.LiveRuntime()
	}
	if host != nil {
		relayWarnOnError(ctx, "segment rotation", func() (struct{}, error) {
			relayMaybeRotateSegment(ctx, host, lease.Session)
			return struct{}{}, nil
		})
		turnMetadata := map[string]any{}
		for k, v := range metadata {
			turnMetadata[k] = v
		}
		surface := lease.Platform
		if surface == "" {
			surface = "unknown"
		}
		for k, v := range RelayRuntimeMetadata(host.RuntimeID, map[string]any{"icebath.execution_surface": surface}) {
			turnMetadata[k] = v
		}
		timeout := RelayScopeOpTimeout
		if host.Relay.Scope == nil || host.Relay.Scope.Push == nil {
			return nil, relayAttributeError("NemoRelayScope", "push")
		}
		push, parent := host.Relay.Scope.Push, lease.Session.CurrentHandle()
		turn.Handle = relayWarnOnError(ctx, "turn initialization", func() (any, error) {
			kwargs := map[string]any{"handle": parent, "input": relayScopeInput(lease.TurnCwd), "metadata": turnMetadata}
			return host.RunInSession(ctx, lease.Session, func(c context.Context) (any, error) {
				return push(c, RelayTurnScope, host.Relay.ScopeType.Function, kwargs)
			}, false, &timeout)
		})
	}
	turn.previousTurn, _ = relayCurrentTurnVar.Get(ctx).(*RelayTurnContext)
	relayCurrentTurnVar.Set(ctx, turn)
	return turn, nil
}

// Rotates the session scope when compaction flagged it or the turn cap is hit
func relayMaybeRotateSegment(ctx context.Context, host *RelayRuntime, session *RelaySession) {
	cfg := relaySegmentsConfigLazy.get(ctx)
	session.Mu.Lock()
	compaction := cfg.onCompaction && session.RotatePending
	capHit := cfg.maxTurns > 0 && session.SegmentTurns >= cfg.maxTurns
	session.Mu.Unlock()
	if compaction || capHit {
		reason := "max_turns"
		if compaction {
			reason = "compaction"
		}
		host.RotateSessionScope(ctx, session, reason)
	}
}

// Closes the turn scope and its logical children, then unregisters and unwinds the turn
func (c *RelaySessionCoordinator) EndTurn(ctx context.Context, turn *RelayTurnContext, outcome string) {
	turn.finalizeMu.Lock()
	defer turn.finalizeMu.Unlock()
	if turn.Closed() {
		relayResetTurnContext(ctx, turn)
		return
	}
	turn.closed.Store(true)
	lease := turn.Lease
	host := lease.LiveRuntime()
	if host != nil {
		c.closeTurnScope(ctx, host, turn, outcome)
	}
	if turn.activeRegistered && host != nil {
		lease.Session.Mu.Lock()
		lease.Session.SegmentTurns++
		lease.Session.Mu.Unlock()
	}
	if rt, ok := lease.Host.(*RelayRuntime); ok && lease.ParentSessionID != "" {
		rt.UnregisterSubagent(ctx, map[string]any{"child_session_id": lease.SessionID})
	}
	c.unregisterActiveTurn(turn)
	relayResetTurnContext(ctx, turn)
	c.consumeDeferredClose(ctx, lease)
}

// Pops the turn's logical LLM children then the turn scope itself
func (c *RelaySessionCoordinator) closeTurnScope(ctx context.Context, host *RelayRuntime, turn *RelayTurnContext, outcome string) {
	relayFinishLogicalCalls(ctx, turn, outcome)
	if failure := host.closeScopeHandle(ctx, turn.Lease.Session, turn.Handle, map[string]any{"outcome": outcome}, false, "turn scope close failed", false, false); failure != "" {
		relayLogger().WarnContext(ctx, "Icebath Relay turn finalization failed: "+failure)
	}
}

// Closes a session whose rotating compaction close was deferred once its last live turn left
func (c *RelaySessionCoordinator) consumeDeferredClose(ctx context.Context, lease *ConversationLease) {
	defer relayFailOpen(ctx, "deferred session close")()
	host := lease.LiveRuntime()
	if host == nil {
		return
	}
	lease.Session.Mu.Lock()
	pending := lease.Session.ClosePending && !lease.Session.Closing
	lease.Session.Mu.Unlock()
	if pending && !c.HasActiveTurn(lease.ProfileKey, lease.SessionID) {
		host.CloseSession(ctx, map[string]any{"session_id": lease.SessionID})
	}
}

// Flags rotation for an in place compaction or closes the old session of a rotating one
func (c *RelaySessionCoordinator) NotifySessionCompacted(ctx context.Context, profileKey, sessionID, oldSessionID string) {
	defer relayFailOpen(ctx, "compaction notification")()
	if !relaySegmentsConfigLazy.get(ctx).onCompaction {
		return
	}
	host, ok := c.Registry.ForProfile(ctx, profileKey, true).(*RelayRuntime)
	if !ok || host == nil {
		return
	}
	if oldSessionID != "" && oldSessionID != sessionID {
		old := host.LookupSession(oldSessionID)
		if old != nil && c.HasActiveTurn(profileKey, oldSessionID) {
			relayFlagOpenSession(old, &old.ClosePending)
		} else {
			host.CloseSession(ctx, map[string]any{"session_id": oldSessionID})
		}
		return
	}
	if session := host.LookupSession(sessionID); session != nil {
		relayFlagOpenSession(session, &session.RotatePending)
	}
}

// Whether a turn is still running for one profile session
func (c *RelaySessionCoordinator) HasActiveTurn(profileKey, sessionID string) bool {
	c.activeTurnsMu.Lock()
	defer c.activeTurnsMu.Unlock()
	return len(c.activeTurns[relayTurnKey{profileKey, sessionID}]) > 0
}

func (c *RelaySessionCoordinator) unregisterActiveTurn(turn *RelayTurnContext) {
	if !turn.activeRegistered {
		return
	}
	key := relayTurnKey{turn.Lease.ProfileKey, turn.Lease.SessionID}
	c.activeTurnsMu.Lock()
	defer c.activeTurnsMu.Unlock()
	if active := c.activeTurns[key]; active != nil {
		delete(active, turn)
		if len(active) == 0 {
			delete(c.activeTurns, key)
		}
	}
	turn.activeRegistered = false
}

// Closes logical LLM children before sibling task aggregation scopes
func (c *RelaySessionCoordinator) FinishLogicalCalls(ctx context.Context, turn *RelayTurnContext, outcome string) {
	turn.finalizeMu.Lock()
	defer turn.finalizeMu.Unlock()
	if !turn.Closed() {
		relayFinishLogicalCalls(ctx, turn, outcome)
	}
}

// Closes logical LLM scopes newest first, keeping the unclosed prefix when one cannot close
func relayFinishLogicalCalls(ctx context.Context, turn *RelayTurnContext, outcome string) {
	lease := turn.Lease
	host := lease.LiveRuntime()
	if host == nil {
		return
	}
	turn.LogicalLLMMu.Lock()
	ids, handles := turn.LogicalLLMCalls.Keys(), turn.LogicalLLMCalls.Values()
	turn.LogicalLLMCalls = pydict.New[string, any]()
	turn.LogicalLLMMu.Unlock()
	for len(ids) > 0 {
		last := len(ids) - 1
		failure := host.closeScopeHandle(ctx, lease.Session, handles[last], map[string]any{"outcome": outcome}, false, "logical LLM scope close failed", false, false)
		if failure == "" {
			ids, handles = ids[:last], handles[:last]
			continue
		}
		turn.LogicalLLMMu.Lock()
		for i, id := range ids {
			if !turn.LogicalLLMCalls.Has(id) {
				turn.LogicalLLMCalls.Set(id, handles[i])
			}
		}
		turn.LogicalLLMMu.Unlock()
		relayLogger().WarnContext(ctx, "Icebath Relay logical LLM finalization failed: "+failure)
		break
	}
}

// Unwinds turn without disturbing a newer context local turn
func relayResetTurnContext(ctx context.Context, turn *RelayTurnContext) {
	if current, _ := relayCurrentTurnVar.Get(ctx).(*RelayTurnContext); current != turn {
		return
	}
	previous, seen := turn.previousTurn, map[*RelayTurnContext]bool{turn: true}
	for previous != nil && previous.Closed() && !seen[previous] {
		seen[previous] = true
		previous = previous.previousTurn
	}
	if previous != nil && previous.Closed() {
		previous = nil
	}
	relayCurrentTurnVar.Set(ctx, previous)
}

// Releases a caller lease without closing a resumable conversation
func (c *RelaySessionCoordinator) ReleaseConversation(lease *ConversationLease) {
	lease.released.Store(true)
}

// Closes the session scope of a finished conversation
func (c *RelaySessionCoordinator) FinalizeConversation(ctx context.Context, profileKey, sessionID string) {
	if host, ok := c.Registry.ForProfile(ctx, profileKey, false).(*RelayRuntime); ok && host != nil {
		host.CloseSession(ctx, map[string]any{"session_id": sessionID})
	}
}

// Process wide conversation and turn coordinator
var RelayCoordinator = NewRelaySessionCoordinator(RelayHosts)

// Turn context inherited by the current work
func CurrentRelayTurn(ctx context.Context) *RelayTurnContext {
	turn, _ := relayCurrentTurnVar.Get(ctx).(*RelayTurnContext)
	return turn
}

// Whether the inherited turn may create Relay instrumentation
func RelayInstrumentationEnabled(ctx context.Context) bool {
	turn := CurrentRelayTurn(ctx)
	return turn == nil || (turn.RelayEnabled() && !turn.Closed())
}

// Live turn of the active profile, of sessionID when given
func ActiveRelayTurn(ctx context.Context, sessionID *string) *RelayTurnContext {
	turn := CurrentRelayTurn(ctx)
	if turn == nil || !turn.RelayEnabled() || turn.Closed() || turn.Lease.Released() {
		return nil
	}
	lease := turn.Lease
	if lease.ProfileKey != CurrentRelayProfileKey(ctx) || (sessionID != nil && lease.SessionID != *sessionID) {
		return nil
	}
	if rt, ok := lease.Host.(*RelayRuntime); ok && (lease.Session == nil || rt.GetSession(lease.SessionID) != lease.Session) {
		return nil
	}
	return turn
}

// Active turn or session parent for managed Relay execution, nothing inside a managed callback
func ResolveRelayExecutionContext(ctx context.Context, sessionID string) (*RelayRuntime, *RelaySession, any, error) {
	if relayManagedCallbackDepthVar.Get(ctx).(int) > 0 || !RelayInstrumentationEnabled(ctx) {
		return nil, nil, nil, nil
	}
	if turn := ActiveRelayTurn(ctx, &sessionID); turn != nil {
		if host := turn.Lease.LiveRuntime(); host != nil {
			session := turn.Lease.Session
			parent := turn.Handle
			if parent == nil {
				parent = session.CurrentHandle()
			}
			return host, session, parent, nil
		}
	}
	rt := GetRelayRuntime(ctx, false, "")
	if rt == nil || !rt.ManagedExecutionEnabled() {
		return nil, nil, nil, nil
	}
	session := rt.GetSession(sessionID)
	if session == nil {
		var err error
		if session, err = rt.EnsureSession(ctx, map[string]any{"session_id": sessionID}, RelaySessionOptions{}); err != nil {
			return nil, nil, nil, err
		}
	}
	if session == nil {
		return rt, nil, nil, nil
	}
	return rt, session, session.CurrentHandle(), nil
}

// Relay rewritten arguments at the icebath authorization boundary
func ApplyRelayToolRequestIntercepts(ctx context.Context, sessionID, toolName string, args map[string]any) (map[string]any, error) {
	if sessionID == "" {
		return args, nil
	}
	rt := GetRelayRuntime(ctx, false, "")
	if rt == nil {
		return args, nil
	}
	return rt.ApplyToolRequestIntercepts(ctx, sessionID, toolName, args)
}

// Whether relayErr is the binding's wrapper around callbackErr rather than a policy error
func IsRelayWrappedCallbackError(relayErr, callbackErr error) bool {
	if relayIdentical(relayErr, callbackErr) {
		return true
	}
	if callbackErr == nil {
		return false
	}
	re, ok := relayErr.(*RelayError)
	if !ok || re.Class != "RuntimeError" {
		return false
	}
	kind := reflect.TypeOf(callbackErr)
	for kind.Kind() == reflect.Pointer && kind.Name() == "" {
		kind = kind.Elem()
	}
	pkg := kind.PkgPath()
	if i := strings.LastIndex(pkg, "/"); i >= 0 {
		pkg = pkg[i+1:]
	}
	for _, name := range []string{kind.Name(), pkg + "." + kind.Name()} {
		if strings.HasPrefix(re.Message, "internal error: "+name+": "+callbackErr.Error()) {
			return true
		}
	}
	return false
}

// Relay runtime of the active profile, nil for the no op host or when create is false and none exists
func GetRelayRuntime(ctx context.Context, create bool, profileKey string) *RelayRuntime {
	rt, _ := RelayHosts.ForProfile(ctx, profileKey, create).(*RelayRuntime)
	return rt
}

// Resets every profile scoped Relay host for isolated tests
func ResetRelayRuntimeForTests() {
	RelayCoordinator.activeTurnsMu.Lock()
	clear(RelayCoordinator.activeTurns)
	RelayCoordinator.activeTurnsMu.Unlock()
	RelayHosts.ShutdownAll()
	relayPlugins.resetForTests(context.Background())
	relayProfileKeyMu.Lock()
	clear(relayProfileKeyCache)
	relayProfileKeyMu.Unlock()
}
