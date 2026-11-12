package observability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	snapshotRecheckNS = int64(3_600) * 1_000_000_000
	maxLineages       = 512
	retiredTurnsKept  = 256
	coldExpectedKept  = 256
)

var (
	relayClockEpoch = time.Now()
	monotonicNS     = func() int64 { return int64(time.Since(relayClockEpoch)) }
	abortedFlags    = map[string]any{"failed": true, "turn_exit_reason": "system_aborted"}
	dispatchedSrcs  = Set("a2a", "kanban")
)

// Declared conversation of the running call, the portal conversation id pkg/provider publishes
var ConversationContextHook func(ctx context.Context) string

// Text of one event key, empty for a missing or falsy value
func eventStr(event map[string]any, key string) string { return pyText(event[key]) }

// Session id and the key's value when both are non empty
func sessionPair(event map[string]any, key string) (taskKey, bool) {
	sessionID, value := eventStr(event, "session_id"), eventStr(event, key)
	return taskKey{sessionID, value}, sessionID != "" && value != ""
}

// Provider local retry ordinal, zero when absent or malformed
func retryOrdinal(event map[string]any) int {
	n, ok := pyInt(event["retry_count"])
	if !ok || n.Sign() <= 0 {
		return 0
	}
	if !n.IsInt64() {
		return int(^uint(0) >> 1)
	}
	return int(n.Int64())
}

// Drops a key only while it still points at the owner session
func forget[K comparable](index *pydict.Dict[K, *metricsSession], key K, owner *metricsSession) {
	if v, ok := index.Get(key); ok && v == owner {
		index.Delete(key)
	}
}

// Active turn handle when it owns this exact task, else the session handle
func taskParentHandle(ctx context.Context, session *metricsSession, taskID string) any {
	sid := session.sessionID
	if turn := tools.ActiveRelayTurn(ctx, &sid); turn != nil && turn.Lease != nil && turn.Lease.SessionID == sid && turn.TaskID == taskID && turn.Handle != nil {
		return turn.Handle
	}
	return session.relaySession.CurrentHandle()
}

// Declares background or one_shot for a finite cli run, leaving every other event untouched
func withLaunchEntrypoint(ctx context.Context, env *tools.Env, event map[string]any) map[string]any {
	if pyrepr.Truthy(event["entrypoint"]) || pyrepr.Truthy(event["parent_task_id"]) || pyrepr.Truthy(event["parent_session_id"]) || ExecutionSurface(ctx, event) != "cli" {
		return event
	}
	if !tools.IsSingleQuerySession(ctx, env) {
		return event
	}
	source := pystr.Lower(pystr.Strip(tools.SessionEnvValue(env, "ICEBATH_SESSION_SOURCE")))
	entrypoint := "one_shot"
	if dispatchedSrcs.Has(source) {
		entrypoint = "background"
	}
	return mergeEvent(event, map[string]any{"entrypoint": entrypoint})
}

// Whole milliseconds since a monotonic start, never negative
func relayElapsedMS(startedNS int64) int { return int(max(0, (monotonicNS()-startedNS)/1_000_000)) }

// Task handle when given, else the session handle
func scopeHandle(session *metricsSession, task *taskRun) any {
	if task != nil {
		return task.handle
	}
	return session.relaySession.CurrentHandle()
}

// Partial hook context matches without crossing known call boundaries
func identitiesCompatible(candidate, observed ToolCallIdentity) bool {
	if observed[2] == "" || candidate[2] != observed[2] {
		return false
	}
	for i := range 2 {
		if candidate[i] != "" && observed[i] != "" && candidate[i] != observed[i] {
			return false
		}
	}
	return true
}

type toolCallKey struct {
	taskID   string
	identity ToolCallIdentity
}

// Open tool calls of a task whose identity is compatible with the observed one
func compatibleToolCallKeys(session *metricsSession, taskID string, identity ToolCallIdentity) []toolCallKey {
	var out []toolCallKey
	for _, key := range session.toolCalls.Keys() {
		if key.taskID == taskID && identitiesCompatible(key.identity, identity) {
			out = append(out, key)
		}
	}
	return out
}

// Single distinct session of a sequence, else nil
func soleSession(items []*metricsSession) *metricsSession {
	var found *metricsSession
	for _, s := range items {
		if found != nil && s != found {
			return nil
		}
		found = s
	}
	return found
}

// Event merged with overlays, later keys winning
func mergeEvent(base map[string]any, overlays ...map[string]any) map[string]any {
	out := make(map[string]any, len(base)+2)
	maps.Copy(out, base)
	for _, o := range overlays {
		maps.Copy(out, o)
	}
	return out
}

// String fields as Relay payload values, nil staying nil
func relayData(m map[string]string) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Route or the fallback when the route is missing or empty
func orRoute(route, fallback map[string]string) map[string]string {
	if len(route) == 0 {
		return fallback
	}
	return route
}

// Logs and swallows a failed callback or panic, reporting whether it returned
func relayGuardedValue[T any](message string, fn func() (T, error)) (out T, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn(message, "error", fmt.Sprint(r))
			ok = false
		}
	}()
	v, err := fn()
	if err != nil {
		slog.Warn(message, "error", err)
		return out, false
	}
	return v, true
}

// Logs and swallows a failed operation under the generic message
func relaySafe(fn func() error) {
	relayGuardedValue("Icebath shared metrics operation failed", func() (struct{}, error) { return struct{}{}, fn() })
}

// Logs and swallows a failed operation under its own message
func relayGuarded(message string, fn func() error) {
	relayGuardedValue(message, func() (struct{}, error) { return struct{}{}, fn() })
}

type modelCallKey struct{ taskID, requestID string }

type modelCall struct {
	handle     any
	taskID     string
	fields     map[string]string
	errorClass string
	ttftBucket string
	startedNS  int64
}

type toolCall struct {
	handle          any
	category        string
	toolName        string
	startedNS       int64
	approvalOutcome string
}

type taskRun struct {
	taskID                string
	handle                any
	context               *pyctx.Context
	startedNS             int64
	startFields           map[string]string
	modelCallIDs          map[string]bool
	toolCallIDs           map[ToolCallIdentity]bool
	turnIDs               []string
	retiredTurnIDs        map[string]bool
	completedToolCallIDs  map[ToolCallIdentity]bool
	unidentifiedToolCalls int
	retryCount            int
	modelRoute            map[string]string
	selectedRoute         map[string]string
	cost                  *TurnCost
}

// Whether a turn id was remembered on the task
func (t *taskRun) hasTurn(turnID string) bool { return slices.Contains(t.turnIDs, turnID) }

// Counted tool calls of the task, identified and unidentified
func (t *taskRun) toolCallCount() int { return len(t.toolCallIDs) + t.unidentifiedToolCalls }

// Completed identities as a sequence for the connector batch checks
func (t *taskRun) completed() []ToolCallIdentity {
	return slices.Collect(maps.Keys(t.completedToolCallIDs))
}

type taskKey struct{ sessionID, taskID string }

type metricsSession struct {
	sessionID      string
	relaySession   *tools.RelaySession
	mu             sync.Mutex
	closing        bool
	modelCalls     *pydict.Dict[modelCallKey, *modelCall]
	tasks          *pydict.Dict[string, *taskRun]
	toolCalls      *pydict.Dict[toolCallKey, *toolCall]
	retiredTurnIDs []string
	startFields    map[string]string
	turns          int
	failedTurns    int
	lastOutcome    string
	firstTurnNS    int64
	lastTurnNS     int64
	modelState     *ModelSessionState
	retiring       bool
	efficiency     *SessionEfficiency
	connectors     *ConnectorSessionState
	routeRun       *RouteRun
	apiCalls       int
	toolCallTotal  int
	replies        int
}

// Fresh metrics session over one Relay session
func newMetricsSession(sessionID string, relaySession *tools.RelaySession) *metricsSession {
	return &metricsSession{
		sessionID: sessionID, relaySession: relaySession,
		modelCalls: pydict.New[modelCallKey, *modelCall](), tasks: pydict.New[string, *taskRun](),
		toolCalls: pydict.New[toolCallKey, *toolCall](), lastOutcome: "unknown",
		modelState: NewModelSessionState(), efficiency: NewSessionEfficiency(),
		connectors: &ConnectorSessionState{}, routeRun: &RouteRun{},
	}
}

type peakLineage struct {
	open       map[string]bool
	peak       *ModelSessionState
	tools      *ToolUsage
	tally      *SessionTally
	run        *RouteRun
	spent      *SpentTurns
	connectors *ConnectorSessionState
}

// Merges one closed segment's context peak, the fullest fill winning and a limit hit sticking
func absorbPeak(into, segment *ModelSessionState) {
	if segment.PeakRoute == nil {
		return
	}
	into.LimitHit = into.LimitHit || segment.LimitHit
	fuller := segment.PeakWindow != nil && (into.PeakWindow == nil || segment.PeakTokens / *segment.PeakWindow > into.PeakTokens / *into.PeakWindow)
	if into.PeakRoute == nil || fuller {
		into.PeakRoute, into.PeakTokens, into.PeakWindow = segment.PeakRoute, segment.PeakTokens, segment.PeakWindow
	}
}

// Runtime layers shared metrics state on one profile's core Relay host
type Runtime struct {
	host           *tools.RelayRuntime
	relay          *tools.NemoRelay
	baseCtx        context.Context
	sessionsMu     sync.Mutex
	active         bool
	sessions       *pydict.Dict[string, *metricsSession]
	taskSessionsMu sync.Mutex
	taskSessions   *pydict.Dict[taskKey, *metricsSession]
	turnSessions   *pydict.Dict[taskKey, *metricsSession]
	lineageMu      sync.Mutex
	lineages       *pydict.Dict[string, *peakLineage]
	lineageOf      *pydict.Dict[string, string]
	coldMu         sync.Mutex
	coldExpected   *pydict.Dict[string, struct{}]
	taskCreationMu sync.Mutex
	sendMu         sync.Mutex
	sendThread     chan struct{}
	flushMu        sync.Mutex
	flushPending   bool
	flushThread    chan struct{}
	snapshotMu     sync.Mutex
	snapshotAt     *int64
	subscriberName string
	Subscriber     *SharedMetricsSubscriber
	registered     atomic.Bool
	unregisterExit func()
}

// Builds the runtime over the given host or the active profile's, registering its subscriber
func newRuntime(ctx context.Context, host *tools.RelayRuntime) (*Runtime, error) {
	resolved := host
	if resolved == nil {
		resolved = tools.GetRelayRuntime(ctx, true, "")
	}
	if resolved == nil {
		return nil, errors.New("Icebath core Relay runtime is unavailable")
	}
	r := &Runtime{
		host: resolved, relay: resolved.Relay, baseCtx: context.WithoutCancel(ctx), active: true,
		sessions: pydict.New[string, *metricsSession](), taskSessions: pydict.New[taskKey, *metricsSession](),
		turnSessions: pydict.New[taskKey, *metricsSession](), lineages: pydict.New[string, *peakLineage](),
		lineageOf: pydict.New[string, string](), coldExpected: pydict.New[string, struct{}](),
		subscriberName: SubscriberName + "." + resolved.RuntimeID,
	}
	info, err := installVersionInfo()
	if err != nil {
		return nil, err
	}
	store, err := NewSharedMetricsStore(ctx, "", "")
	if err != nil {
		return nil, err
	}
	runtimeID := resolved.RuntimeID
	r.Subscriber, err = NewSharedMetricsSubscriber(ctx, store, info.BaseVersion, &runtimeID)
	if err != nil {
		return nil, err
	}
	if r.relay.Subscribers == nil || r.relay.Subscribers.Register == nil {
		return nil, tools.RelayAttributeError("NemoRelaySubscribers", "register")
	}
	subscriberCtx := r.baseCtx
	subscriber := r.Subscriber
	if err := r.relay.Subscribers.Register(r.subscriberName, func(event *tools.RelayEventView) {
		subscriber.Call(subscriberCtx, &RelayEvent{
			Kind: event.Kind, Name: event.Name, Category: event.Category, ScopeCategory: event.ScopeCategory,
			CategoryProfile: event.CategoryProfile, Metadata: event.Metadata, Data: event.Data,
		})
	}); err != nil {
		return nil, err
	}
	if err := r.host.RetainManagedExecution(r.subscriberName); err != nil {
		return nil, err
	}
	r.registered.Store(true)
	r.unregisterExit = tools.RegisterRelayAtExit(r.Shutdown)
	return r, nil
}

// Schema and runtime instance metadata stamped on every emitted Relay event
func (r *Runtime) eventMetadata() map[string]any {
	return map[string]any{SchemaKey: SchemaVersion, tools.RelayRuntimeInstanceKey: r.host.RuntimeID}
}

// Emits one Relay scope event through the binding
func (r *Runtime) scopeEvent(ctx context.Context, name string, kwargs map[string]any) (any, error) {
	if r.relay.Scope == nil || r.relay.Scope.Event == nil {
		return nil, tools.RelayAttributeError("NemoRelayScope", "event")
	}
	return r.relay.Scope.Event(ctx, name, kwargs)
}

// Flushes every Relay subscriber through the binding
func (r *Runtime) subscribersFlush() error {
	if r.relay.Subscribers == nil || r.relay.Subscribers.Flush == nil {
		return tools.RelayAttributeError("NemoRelaySubscribers", "flush")
	}
	return r.relay.Subscribers.Flush()
}

// Reads the scope stack before running a Relay operation
func (r *Runtime) withScopeStack(ctx context.Context, fn func(ctx context.Context) (any, error)) (any, error) {
	if r.relay.GetScopeStack == nil {
		return nil, tools.RelayAttributeError("nemo_relay", "get_scope_stack")
	}
	if _, err := r.relay.GetScopeStack(ctx); err != nil {
		return nil, err
	}
	return fn(ctx)
}

// Runs an operation in a fresh copy of the task context
func (r *Runtime) runInTask(ctx context.Context, task *taskRun, fn func(ctx context.Context) (any, error)) (any, error) {
	return r.withScopeStack(pyctx.With(ctx, task.context.Copy()), fn)
}

// Runs under the task context when the call belongs to a task, else the session
func (r *Runtime) runScoped(ctx context.Context, session *metricsSession, task *taskRun, fn func(ctx context.Context) (any, error)) (any, error) {
	if task != nil {
		return r.runInTask(ctx, task, fn)
	}
	return r.host.RunInSession(ctx, session.relaySession, fn, false, nil)
}

// Emits one Relay mark under the task scope when given, else the session scope
func (r *Runtime) mark(ctx context.Context, session *metricsSession, task *taskRun, name string, data map[string]any) error {
	kwargs := map[string]any{"handle": scopeHandle(session, task), "data": data, "metadata": r.eventMetadata()}
	_, err := r.runScoped(ctx, session, task, func(ctx context.Context) (any, error) { return r.scopeEvent(ctx, name, kwargs) })
	return err
}

// Binds this runtime's profile home for work outside its own turns
func (r *Runtime) ownerScope(ctx context.Context) context.Context {
	return home.WithOverride(ctx, r.host.ProfileKey)
}

// Metrics session for an event, opening the Relay session once, nil when closing or inactive
func (r *Runtime) ensureSession(ctx context.Context, event map[string]any) (*metricsSession, error) {
	sessionID := eventStr(event, "session_id")
	if sessionID == "" {
		return nil, nil
	}
	r.sessionsMu.Lock()
	if !r.active {
		r.sessionsMu.Unlock()
		return nil, nil
	}
	relaySession, err := r.host.EnsureSession(ctx, event, tools.RelaySessionOptions{})
	if err != nil || relaySession == nil {
		r.sessionsMu.Unlock()
		return nil, err
	}
	session, ok := r.sessions.Get(sessionID)
	if !ok {
		session = newMetricsSession(sessionID, relaySession)
		r.sessions.Set(sessionID, session)
		r.shareSpent(session)
	}
	r.sessionsMu.Unlock()
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closing {
		return nil, nil
	}
	return session, nil
}

// Emits one payload free activation attempt under the session scope
func (r *Runtime) recordClientActive(ctx context.Context, event map[string]any) error {
	session, err := r.ensureSession(ctx, event)
	if err != nil || session == nil {
		return err
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return r.emitClientActiveLocked(ctx, session)
}

// Activation mark and a due install snapshot, the caller holding the session lock
func (r *Runtime) emitClientActiveLocked(ctx context.Context, session *metricsSession) error {
	if session.closing {
		return nil
	}
	if err := r.mark(ctx, session, nil, ClientActiveMark, map[string]any{}); err != nil {
		return err
	}
	relaySafe(func() error { return r.emitInstallSnapshotIfDue(ctx, session) })
	return nil
}

// Install snapshot mark at most once per store latch, rereading the latch hourly
func (r *Runtime) emitInstallSnapshotIfDue(ctx context.Context, session *metricsSession) error {
	now := monotonicNS()
	r.snapshotMu.Lock()
	if r.snapshotAt != nil && now-*r.snapshotAt < snapshotRecheckNS {
		r.snapshotMu.Unlock()
		return nil
	}
	r.snapshotAt = &now
	r.snapshotMu.Unlock()
	due, err := r.Subscriber.Store.InstallSnapshotDue(ctx)
	if err != nil || !due {
		return err
	}
	cfg, err := rawConfig(ctx)
	if err != nil {
		return err
	}
	fields, err := CollectInstallSnapshot(ctx, cfg)
	if err != nil {
		return err
	}
	return r.mark(ctx, session, nil, InstallSnapshotMark, relayData(fields))
}

// Opens one Relay function scope for a task run and returns its owning session
func (r *Runtime) startTask(ctx context.Context, env *tools.Env, event map[string]any) (*metricsSession, *taskRun, error) {
	pair, ok := sessionPair(event, "task_id")
	if !ok {
		return nil, nil, nil
	}
	taskID := pair.taskID
	r.taskCreationMu.Lock()
	defer r.taskCreationMu.Unlock()
	if owner := r.taskSession(event, false); owner != nil {
		owner.mu.Lock()
		defer owner.mu.Unlock()
		if owner.closing {
			return nil, nil, nil
		}
		task, _ := owner.tasks.Get(taskID)
		if task != nil && !r.admits(owner, task, event, false) {
			return nil, nil, nil
		}
		if task == nil {
			return nil, nil, nil
		}
		return owner, task, nil
	}
	session, err := r.ensureSession(ctx, event)
	if err != nil || session == nil {
		return nil, nil, err
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	turnID := eventStr(event, "turn_id")
	session.relaySession.Mu.Lock()
	relayContext := session.relaySession.Context
	session.relaySession.Mu.Unlock()
	if session.closing || (turnID != "" && slices.Contains(session.retiredTurnIDs, turnID)) || relayContext == nil {
		return nil, nil, nil
	}
	if err := r.emitClientActiveLocked(ctx, session); err != nil {
		return nil, nil, err
	}
	taskContext := relayContext.Copy()
	startFields := TaskStartFields(ctx, withLaunchEntrypoint(ctx, env, event))
	if r.relay.Scope == nil || r.relay.Scope.Push == nil {
		return nil, nil, tools.RelayAttributeError("NemoRelayScope", "push")
	}
	kwargs := map[string]any{
		"handle": taskParentHandle(ctx, session, taskID), "input": relayData(startFields), "metadata": r.eventMetadata(),
	}
	scopeType := r.relay.ScopeType.Function
	handle, err := r.withScopeStack(pyctx.With(ctx, taskContext), func(ctx context.Context) (any, error) {
		return r.relay.Scope.Push(ctx, TaskScope, scopeType, kwargs)
	})
	if err != nil {
		return nil, nil, err
	}
	retired := make(map[string]bool, len(session.retiredTurnIDs))
	for _, id := range session.retiredTurnIDs {
		retired[id] = true
	}
	task := &taskRun{
		taskID: taskID, handle: handle, context: taskContext, startedNS: monotonicNS(), startFields: startFields,
		modelCallIDs: map[string]bool{}, toolCallIDs: map[ToolCallIdentity]bool{}, retiredTurnIDs: retired,
		completedToolCallIDs: map[ToolCallIdentity]bool{}, cost: &TurnCost{},
	}
	session.tasks.Set(taskID, task)
	r.taskSessionsMu.Lock()
	r.taskSessions.Set(pair, session)
	r.taskSessionsMu.Unlock()
	r.rememberTurn(session, task, event)
	return session, task, nil
}

// Opens the task a user message started, marking it a user turn
func (r *Runtime) startUserTurn(ctx context.Context, env *tools.Env, event map[string]any) error {
	session, task, err := r.startTask(ctx, env, event)
	if task != nil {
		session.mu.Lock()
		task.cost.UserTurn = true
		session.mu.Unlock()
	}
	return err
}

// Opens one model call lifecycle under its task, counting repeated starts as retries
func (r *Runtime) startModelCall(ctx context.Context, env *tools.Env, event map[string]any) error {
	taskID := eventStr(event, "task_id")
	session, task, err := r.taskPair(ctx, env, event, true, true)
	if err != nil {
		return err
	}
	if taskID != "" && task == nil {
		return nil
	}
	if session == nil {
		if session, err = r.ensureSession(ctx, event); err != nil || session == nil {
			return err
		}
	}
	requestID := eventStr(event, "api_request_id")
	if requestID == "" {
		return nil
	}
	key := modelCallKey{taskID, requestID}
	fields := ModelCallFields(event)
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closing || (task != nil && !r.admits(session, task, event, true)) {
		return nil
	}
	if existing, ok := session.modelCalls.Get(key); ok {
		existing.fields = fields
		if task != nil {
			task.retryCount++
		}
		return nil
	}
	if task != nil {
		task.modelRoute = fields
		if len(task.selectedRoute) == 0 {
			task.selectedRoute = fields
		}
		task.modelCallIDs[requestID] = true
		if retryOrdinal(event) > 0 {
			task.retryCount++
		}
	}
	if r.relay.LLM == nil || r.relay.LLM.Call == nil {
		return tools.RelayAttributeError("NemoRelayLLM", "call")
	}
	if r.relay.LLMRequest == nil {
		return tools.RelayAttributeError("nemo_relay", "LLMRequest")
	}
	request, err := r.relay.LLMRequest(map[string]any{}, map[string]any{})
	if err != nil {
		return err
	}
	kwargs := map[string]any{"handle": scopeHandle(session, task), "metadata": r.eventMetadata(), "model_name": ModelCallProfileModel}
	handle, err := r.runScoped(ctx, session, task, func(ctx context.Context) (any, error) {
		return r.relay.LLM.Call(ctx, ModelCallScope, request, kwargs)
	})
	if err != nil {
		return err
	}
	session.modelCalls.Set(key, &modelCall{handle: handle, taskID: taskID, fields: fields, errorClass: "none", ttftBucket: "unknown", startedNS: monotonicNS()})
	return nil
}

// Model and provider fields of a call as optional keyword values
func optField(fields map[string]string, key string) any {
	if v, ok := fields[key]; ok {
		return v
	}
	return nil
}

// Refreshes the located model call from the event, finish closing it as a success
func (r *Runtime) updateModelCall(ctx context.Context, event map[string]any, finish bool) error {
	session := r.anySession(event)
	if session == nil {
		return nil
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closing {
		return nil
	}
	key, ok := existingModelCallKey(session, event)
	if !ok {
		return nil
	}
	call, _ := session.modelCalls.Get(key)
	if call == nil {
		return nil
	}
	call.fields = ModelCallFields(event)
	if !finish {
		call.errorClass = ModelErrorClass(event)
		session.modelState.ObserveError(call.fields, call.errorClass)
		return nil
	}
	task, _ := session.tasks.Get(call.taskID)
	if task != nil && task.cost.UserTurn {
		session.replies++
	}
	session.modelState.ObserveCall(call.fields, event["usage"], event["context_length"])
	r.observeTurnCall(ctx, session, task, call, event)
	call.ttftBucket = TtftBucket(event)
	r.finishModelCall(ctx, session, key, "success")
	tokens := ModelTokenFields(event["usage"], optField(call.fields, "model"), optField(call.fields, "provider"), "primary", nil)
	if tokens != nil {
		owner, _ := session.tasks.Get(call.taskID)
		relayGuarded("Icebath shared-metrics token mark failed", func() error {
			return r.mark(ctx, session, owner, ModelTokensMark, tokens)
		})
	}
	return nil
}
