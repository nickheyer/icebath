package observability

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	runtimesMu           sync.Mutex
	runtimes             = pydict.New[string, *Runtime]()
	consentReconcileMu   sync.Mutex
	consentReconcileDone bool
	processMarkHooks     = Set("post_auxiliary_call")
	newMetricsSender     = func(store *SharedMetricsStore, endpoint string, opts ...SenderOption) pendingSender {
		return NewSharedMetricsSender(store, endpoint, opts...)
	}
)

// Send pass a runtime starts, the sender class the Python module attribute names
type pendingSender interface {
	SendPending(ctx context.Context) SendOutcome
}

// Package hooks other modules call, replaceable like the Python module attributes
var (
	relayEnabled            func(ctx context.Context) bool
	recordProcessMark       func(ctx context.Context, mark string, data map[string]string)
	recordProcessMarksSaved func(ctx context.Context, marks []Mark) int
	recordModelSwitchAfter  func(ctx context.Context, sessionID string)
	shutdownRuntimes        func(ctx context.Context)
)

func init() {
	relayEnabled = Enabled
	recordProcessMark = RecordProcessMark
	recordProcessMarksSaved = RecordProcessMarksSaved
	recordModelSwitchAfter = RecordModelSwitchAfter
	shutdownRuntimes = ShutdownRuntimes
	tools.RelayCoordinator.RegisterSessionInitializer(SubscriberName, prepareCoreSession)
}

type relayHookHandler func(ctx context.Context, env *tools.Env, rt *Runtime, kw map[string]any) error

var relayHookHandlers = map[string]relayHookHandler{
	"on_session_start": func(ctx context.Context, _ *tools.Env, rt *Runtime, kw map[string]any) error {
		return rt.recordClientActive(ctx, kw)
	},
	"pre_llm_call": func(ctx context.Context, env *tools.Env, rt *Runtime, kw map[string]any) error {
		return rt.startUserTurn(ctx, env, kw)
	},
	"pre_api_request": func(ctx context.Context, env *tools.Env, rt *Runtime, kw map[string]any) error {
		return rt.startModelCall(ctx, env, kw)
	},
	"pre_tool_call": func(ctx context.Context, env *tools.Env, rt *Runtime, kw map[string]any) error {
		return rt.startToolCall(ctx, env, withRuntimeToolset(ctx, kw))
	},
	"post_tool_call": func(ctx context.Context, _ *tools.Env, rt *Runtime, kw map[string]any) error {
		return rt.recordToolCall(ctx, withRuntimeToolset(ctx, kw))
	},
	"post_approval_response": func(ctx context.Context, _ *tools.Env, rt *Runtime, kw map[string]any) error {
		return rt.recordApproval(ctx, kw)
	},
	"on_skill_lifecycle": func(ctx context.Context, _ *tools.Env, rt *Runtime, kw map[string]any) error {
		return rt.recordSkillLifecycle(ctx, kw)
	},
	"post_api_request": func(ctx context.Context, _ *tools.Env, rt *Runtime, kw map[string]any) error {
		return rt.updateModelCall(ctx, kw, true)
	},
	"post_auxiliary_call": func(ctx context.Context, _ *tools.Env, rt *Runtime, kw map[string]any) error {
		return rt.recordAuxiliaryTokens(ctx, kw)
	},
	"api_request_error": func(ctx context.Context, _ *tools.Env, rt *Runtime, kw map[string]any) error {
		return rt.updateModelCall(ctx, kw, false)
	},
	"on_session_end": func(ctx context.Context, _ *tools.Env, rt *Runtime, kw map[string]any) error {
		return rt.finishTask(ctx, kw)
	},
	"subagent_stop": closeChildSession,
	"on_session_finalize": func(ctx context.Context, _ *tools.Env, rt *Runtime, kw map[string]any) error {
		return rt.closeSession(ctx, kw)
	},
	"on_session_reset": func(ctx context.Context, _ *tools.Env, rt *Runtime, kw map[string]any) error {
		return rt.closeSession(ctx, kw)
	},
}

// HandledHooks are the lifecycle hooks the shared metrics runtime projects
var HandledHooks = func() ValueSet {
	names := make([]string, 0, len(relayHookHandlers))
	for name := range relayHookHandlers {
		names = append(names, name)
	}
	return Set(names...)
}()

// Read only config snapshot of the active profile, empty when absent
func rawConfig(ctx context.Context) (map[string]any, error) {
	cfg, err := config.ReadRawConfigReadonly(ctx, home.Dir(ctx))
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return cfg, nil
}

// Opt in send policy resolved from the read only config snapshot
func resolvedSendConfig(ctx context.Context) (SendConfig, error) {
	cfg, err := rawConfig(ctx)
	if err != nil {
		return SendConfig{}, err
	}
	return ResolveSendConfig(cfg), nil
}

// Reconciles the store's consent windows with the send setting in one write transaction
func reconcileStoreConsent(ctx context.Context, store *SharedMetricsStore, sendEnabled bool) error {
	return store.connection(ctx, busyTimeoutMS, func(conn state.SQLiteConn) error {
		return state.WriteTxn(ctx, conn, func() error { return ReconcileSendConsent(ctx, conn, sendEnabled, utcNow()) })
	})
}

// Enabled reads the active profile's shared metrics policy, tearing down its runtime when off
func Enabled(ctx context.Context) bool {
	profileKey := tools.CurrentRelayProfileKey(ctx)
	var node any
	if cfg, err := rawConfig(ctx); err != nil {
		slog.Debug("Unable to read Icebath shared-metrics policy", "error", err)
	} else {
		node = cfg
	}
	for _, key := range []string{"telemetry", "shared_metrics"} {
		d, _ := asDict(node)
		node = d[key]
	}
	if d, ok := asDict(node); ok && d["enabled"] == true {
		return true
	}
	runtimesMu.Lock()
	defer runtimesMu.Unlock()
	rt, present := runtimes.Get(profileKey)
	if !present {
		return false
	}
	runtimes.Delete(profileKey)
	if rt != nil {
		rt.Deactivate()
	}
	return false
}

// Whether the runtime handles a hook while shared metrics are on
func relayHandlesHook(ctx context.Context, hookName string) bool {
	return HandledHooks.Has(hookName) && relayEnabled(ctx)
}

// Reconciles send consent windows with config once per process, skipping when nothing is on disk and consent is off
func reconcileSendConsentOnce(ctx context.Context) {
	consentReconcileMu.Lock()
	if consentReconcileDone {
		consentReconcileMu.Unlock()
		return
	}
	consentReconcileDone = true
	consentReconcileMu.Unlock()
	err := func() error {
		resolved, err := resolvedSendConfig(ctx)
		if err != nil {
			return err
		}
		defaultPath := filepath.Join(home.Dir(ctx), "telemetry", "shared_metrics", "metrics.sqlite3")
		if _, statErr := os.Stat(defaultPath); !resolved.Send && statErr != nil {
			return nil
		}
		store, err := NewSharedMetricsStore(ctx, "", "")
		if err != nil {
			return err
		}
		return reconcileStoreConsent(ctx, store, resolved.Send)
	}()
	if err != nil {
		slog.Warn("Unable to reconcile shared-metrics send consent", "error", err)
	}
}

// Whether the inherited turn lets a process level mark through, a skipped turn gating open or closed
func processMarksAdmitted(ctx context.Context) bool {
	turn := tools.CurrentRelayTurn(ctx)
	return turn == nil || turn.RelayEnabled()
}

// Projects one lifecycle event into the core Relay integration
func relayObserveLifecycle(ctx context.Context, env *tools.Env, hookName string, kwargs map[string]any) {
	reconcileSendConsentOnce(ctx)
	if !relayHandlesHook(ctx, hookName) {
		return
	}
	admitted := false
	if processMarkHooks.Has(hookName) {
		admitted = processMarksAdmitted(ctx)
	} else {
		admitted = relayInstrumentationEnabled(ctx)
	}
	if !admitted {
		return
	}
	rt := getRuntime(ctx, false, nil)
	if rt == nil {
		return
	}
	relayGuarded("Icebath shared metrics hook failed: "+hookName, func() error {
		return relayHookHandlers[hookName](ctx, env, rt, kwargs)
	})
}

// Attaches the toolset the runtime registry declares for the event's tool
func withRuntimeToolset(ctx context.Context, event map[string]any) map[string]any {
	toolName := eventStr(event, "tool_name")
	if pyrepr.Truthy(event["toolset"]) || toolName == "" {
		return event
	}
	toolset := toolsetForTool(ctx, toolName)
	if toolset == "" {
		toolset = "other"
	}
	return mergeEvent(event, map[string]any{"toolset": toolset})
}

// Registry toolset of a tool, empty when unknown
var toolsetForTool = func(ctx context.Context, name string) string { return tools.Default.ToolsetForTool(ctx, "", name) }

// Closes a finished subagent's child session
func closeChildSession(ctx context.Context, _ *tools.Env, rt *Runtime, kw map[string]any) error {
	if id := eventStr(kw, "child_session_id"); id != "" {
		return rt.closeSession(ctx, map[string]any{"session_id": id})
	}
	return nil
}

// Prepares the profile subscriber before the coordinator opens a session scope
func prepareCoreSession(ctx context.Context, host *tools.RelayRuntime, _ map[string]any) error {
	if host.ProfileKey == tools.CurrentRelayProfileKey(ctx) && relayEnabled(ctx) {
		getRuntime(ctx, true, host)
	}
	return nil
}

// StartTaskRun starts task metrics at the outer execution boundary
func StartTaskRun(ctx context.Context, env *tools.Env, sessionID, taskID, platform, parentSessionID string) {
	runTaskHook(ctx, true, map[string]any{
		"session_id": sessionID, "task_id": taskID, "platform": platform, "parent_session_id": parentSessionID,
	}, func(rt *Runtime, event map[string]any) error {
		_, _, err := rt.startTask(ctx, env, event)
		return err
	})
}

// ShutdownRuntimes runs each live runtime's exit flush now for a process leaving without exit handlers
func ShutdownRuntimes(ctx context.Context) {
	runtimesMu.Lock()
	var live []*Runtime
	for _, rt := range runtimes.Values() {
		if rt != nil {
			live = append(live, rt)
		}
	}
	runtimesMu.Unlock()
	for _, rt := range live {
		relaySafe(func() error { rt.Shutdown(); return nil })
	}
}

// Live runtime of the active profile without creating one
func liveRuntime(ctx context.Context) *Runtime {
	runtimesMu.Lock()
	defer runtimesMu.Unlock()
	rt, _ := runtimes.Get(tools.CurrentRelayProfileKey(ctx))
	return rt
}

// CloseSessionRun emits the summary of a session a surface retired without a finalize hook
func CloseSessionRun(ctx context.Context, sessionID string) {
	if sessionID == "" || !relayEnabled(ctx) {
		return
	}
	if rt := liveRuntime(ctx); rt != nil {
		relaySafe(func() error { return rt.closeSession(ctx, map[string]any{"session_id": sessionID}) })
	}
}

// RotateSegment links a compression rotated session id to its conversation
func RotateSegment(ctx context.Context, oldSessionID, newSessionID string) {
	if oldSessionID == "" || newSessionID == "" || oldSessionID == newSessionID || !relayEnabled(ctx) {
		return
	}
	if rt := liveRuntime(ctx); rt != nil {
		relaySafe(func() error { rt.rotateSegment(ctx, oldSessionID, newSessionID); return nil })
	}
}

// FinishTaskRun finishes task metrics for every return or error path
func FinishTaskRun(ctx context.Context, sessionID, taskID, platform string, result map[string]any, err error) {
	event := mergeEvent(map[string]any{"session_id": sessionID, "task_id": taskID, "platform": platform}, terminalFlags(result, err))
	runTaskHook(ctx, false, event, func(rt *Runtime, event map[string]any) error { return rt.finishTask(ctx, event) })
}

// RecordProcessMark emits one process level decision mark when shared metrics are on
func RecordProcessMark(ctx context.Context, mark string, data map[string]string) {
	if !relayEnabled(ctx) || !processMarksAdmitted(ctx) {
		return
	}
	if rt := getRuntime(ctx, true, nil); rt != nil {
		relaySafe(func() error { return rt.recordProcessMark(ctx, mark, relayData(data)) })
	}
}

// RecordProcessMarksSaved records facts recovered from a file and returns how many rows are settled
func RecordProcessMarksSaved(ctx context.Context, marks []Mark) int {
	if !relayEnabled(ctx) || !processMarksAdmitted(ctx) {
		return len(marks)
	}
	rt := getRuntime(ctx, true, nil)
	if rt == nil {
		return 0
	}
	n, _ := relayGuardedValue("Icebath shared metrics operation failed", func() (int, error) { return rt.recordProcessMarksSaved(ctx, marks) })
	return n
}

// RecordSessionFriction records one friction signal in the active profile's runtime
func RecordSessionFriction(ctx context.Context, signal, sessionID string, route map[string]string, turns int) {
	if !relayInstrumentationEnabled(ctx) {
		return
	}
	if rt := getRuntime(ctx, true, nil); rt != nil {
		relaySafe(func() error { return rt.recordFriction(ctx, signal, sessionID, route, turns) })
	}
}

// RecordSessionTools records the tools one primary request sent without creating a runtime
func RecordSessionTools(ctx context.Context, sessionID string, agent MetricsAgent, toolsForAPI []map[string]any) {
	if rt := liveRuntime(ctx); rt != nil {
		relaySafe(func() error { return rt.recordSessionTools(ctx, sessionID, agent, toolsForAPI) })
	}
}

// RecordKnownCacheBreak counts a prompt cache break the agent caused
func RecordKnownCacheBreak(ctx context.Context, cause string, route map[string]string, sessionID string) {
	if !relayInstrumentationEnabled(ctx) {
		return
	}
	if rt := getRuntime(ctx, true, nil); rt != nil {
		relaySafe(func() error { rt.recordKnownCacheBreak(ctx, cause, route, sessionID); return nil })
	}
}

// RecordModelSwitchAfter counts turns before a switch for a session this process served
func RecordModelSwitchAfter(ctx context.Context, sessionID string) {
	if sessionID == "" || !relayEnabled(ctx) || !relayInstrumentationEnabled(ctx) {
		return
	}
	if rt := liveRuntime(ctx); rt != nil {
		relaySafe(func() error { return rt.recordSwitchAfter(ctx, sessionID) })
	}
}

// Runs one task hook on the active profile's runtime while shared metrics are on
func runTaskHook(ctx context.Context, retryFailed bool, event map[string]any, method func(rt *Runtime, event map[string]any) error) {
	if !relayEnabled(ctx) {
		return
	}
	if rt := getRuntime(ctx, retryFailed, nil); rt != nil {
		relaySafe(func() error { return method(rt, event) })
	}
}

// Bounded completed, failed, interrupted and exit reason flags for a task's return or error
func terminalFlags(result map[string]any, err error) map[string]any {
	if err != nil {
		mro := pyExceptionMRO(err)
		interrupted := errors.Is(err, context.Canceled) || slices.Contains(mro, "KeyboardInterrupt") ||
			slices.Contains(mro, "InterruptedError") || (len(mro) > 0 && mro[0] == "CancelledError")
		reason := "system_aborted"
		switch {
		case interrupted:
			reason = "interrupted_by_user"
		case slices.Contains(mro, "TimeoutError"):
			reason = "timed_out"
		}
		return map[string]any{
			"completed": false, "failed": !interrupted, "interrupted": interrupted,
			"turn_exit_reason": reason, "failure_class": "exception",
		}
	}
	failed := result["failed"] == true
	reason := pyText(result["turn_exit_reason"])
	if reason == "" {
		reason = pyText(result["failure_reason"])
	}
	if reason == "" {
		reason = "unknown"
		if failed {
			reason = "failed"
		}
	}
	return map[string]any{
		"completed": result["completed"] == true, "failed": failed, "interrupted": result["interrupted"] == true,
		"turn_exit_reason": reason, "failure_reason": pyText(result["failure_reason"]),
	}
}

// Runtime of the active profile, created once, a failed creation retried only when asked
func getRuntime(ctx context.Context, retryFailed bool, host *tools.RelayRuntime) *Runtime {
	profileKey := tools.CurrentRelayProfileKey(ctx)
	runtimesMu.Lock()
	defer runtimesMu.Unlock()
	rt, present := runtimes.Get(profileKey)
	if rt != nil {
		if host == nil || rt.host == host {
			return rt
		}
		rt.Deactivate()
	} else if present && !retryFailed {
		return nil
	}
	created, err := newRuntime(ctx, host)
	if err != nil {
		slog.Warn("Icebath shared metrics initialization failed", "error", err)
		runtimes.Set(profileKey, nil)
		return nil
	}
	runtimes.Set(profileKey, created)
	return created
}

// Shuts down and forgets every profile runtime for isolated tests
func resetForTests() {
	runtimesMu.Lock()
	all := runtimes.Values()
	runtimes = pydict.New[string, *Runtime]()
	runtimesMu.Unlock()
	for _, rt := range all {
		if rt != nil {
			rt.Shutdown()
		}
	}
}

// Records a tool metric event under its named mark when collection is on, true when the event was a metric
func RecordToolMetricEvent(ctx context.Context, env *tools.Env, ev tools.Event) bool {
	if ev.Kind != "metric" {
		return false
	}
	name, _ := ev.Data["name"].(string)
	if name == "" {
		return true
	}
	if name == TerminalOutcomeMark {
		outcome, _ := ev.Data["outcome"].(string)
		RecordTerminalOutcome(ctx, env, ev.Data["command"], ev.Data["backend"], nil, outcome)
		return true
	}
	if name == ExecutionBackendMark {
		kind, _ := ev.Data["kind"].(string)
		var errorClass any
		if class, _ := ev.Data["error_class"].(string); class != "" {
			errorClass = class
		}
		RecordExecutionBackend(ctx, env, kind, ev.Data["backend"], ev.Data["result"], errorClass)
		return true
	}
	data := make(map[string]string, len(ev.Data))
	for k, v := range ev.Data {
		if s, ok := v.(string); ok && k != "name" {
			data[k] = s
		}
	}
	emit(ctx, name, func() map[string]string { return data })
	return true
}
