package agent

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"

	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Agent slots the tool executor reads and mutates across a tool round
type toolExecutorState struct {
	toolExec        *tools.Executor
	toolExecOnce    sync.Once
	toolDispatcher  *tools.Dispatcher
	toolCallCtx     context.Context
	delegateSpinner tools.Spinner
	pluginLiveNotes plugins.PluginLiveNotes
}

// Host view whose flush and interrupt take the executor shapes beside the codex runtime ones
type agentExecutorHost struct{ *AIAgent }

// Live flush where a disabled store reads persisted, the None that is not False
func (h agentExecutorHost) FlushMessagesToSessionDB(messages []*state.Message) (bool, error) {
	if h.persistDisabled || h.sessionDB == nil {
		return true, nil
	}
	return h.flushLiveMessages(messages, nil)
}

func (h agentExecutorHost) Interrupt(message, toolReason string) {
	h.InterruptWith(message, InterruptOptions{ToolReason: toolReason})
}

// Progress hook in the executor shape, a codex shaped hook adapted to it
func (h agentExecutorHost) ToolProgressCallback() toolProgressFn {
	switch cb := progressFn(h.toolProgressCallback).(type) {
	case toolProgressFn:
		return cb
	case codexProgressFn:
		return func(event, name, preview string, args map[string]any, extra map[string]any) {
			cb(event, name, preview, args, codexProgressExtra(extra))
		}
	}
	return nil
}

// Start hook in the executor shape, a codex shaped hook adapted to it
func (h agentExecutorHost) ToolStartCallback() toolStartFn {
	switch cb := h.toolStartCallback.(type) {
	case toolStartFn:
		return cb
	case codexStartFn:
		return func(toolCallID, name string, args map[string]any) { cb(toolCallID, name, args) }
	}
	return nil
}

// Complete hook in the executor shape, a codex shaped hook adapted to it
func (h agentExecutorHost) ToolCompleteCallback() toolCompleteFn {
	switch cb := h.toolCompleteCallback.(type) {
	case toolCompleteFn:
		return cb
	case codexCompleteFn:
		return func(toolCallID, name string, args map[string]any, result string) { cb(toolCallID, name, args, result) }
	}
	return nil
}

var _ tools.ExecutorHost = agentExecutorHost{}

type (
	toolProgressFn  = func(event, name, preview string, args map[string]any, extra map[string]any)
	toolStartFn     = func(toolCallID, name string, args map[string]any)
	toolCompleteFn  = func(toolCallID, name string, args map[string]any, result string)
	codexProgressFn = func(event, name string, preview any, args any, extra *provider.CodexToolProgressExtra)
	codexStartFn    = func(callID, name string, args any)
	codexCompleteFn = func(callID, name string, args any, result string)
)

// Codex completion fields of an executor progress extra, nil when the event carries none
func codexProgressExtra(extra map[string]any) *provider.CodexToolProgressExtra {
	if extra == nil {
		return nil
	}
	out := &provider.CodexToolProgressExtra{}
	if d, ok := extra["duration"].(float64); ok {
		out.Duration = &d
	}
	out.IsError, _ = extra["is_error"].(bool)
	if r, ok := extra["result"].(string); ok {
		out.Result = &r
	}
	return out
}

// Executor progress extra keys of a codex completion, nil when the event carries none
func toolProgressExtra(extra *provider.CodexToolProgressExtra) map[string]any {
	if extra == nil {
		return nil
	}
	out := map[string]any{"is_error": extra.IsError}
	if extra.Duration != nil {
		out["duration"] = *extra.Duration
	}
	if extra.Result != nil {
		out["result"] = *extra.Result
	}
	return out
}

// One executor per agent over the process wide dispatcher so interrupts reach every tracked worker
func (a *AIAgent) toolExecutor() *tools.Executor {
	a.toolExecOnce.Do(func() {
		d := a.toolDispatcher
		if d == nil {
			d = tools.DefaultDispatcher
		}
		a.toolExec = tools.NewExecutor(agentExecutorHost{a}, d)
	})
	return a.toolExec
}

// Agent copy of the tool env whose events record tool metrics before reaching the host listener
func withToolMetricSink(ctx context.Context, env *tools.Env) *tools.Env {
	if env == nil {
		return nil
	}
	metricsCtx := context.WithoutCancel(ctx)
	out := *env
	prev := env.Notify
	out.Notify = func(ev tools.Event) {
		if observability.RecordToolMetricEvent(metricsCtx, &out, ev) {
			return
		}
		if prev != nil {
			prev(ev)
		}
	}
	return &out
}

// Context of the running tool round, the background context outside one
func (a *AIAgent) toolContext() context.Context {
	if a.toolCallCtx != nil {
		return a.toolCallCtx
	}
	return context.Background()
}

func toolSearchScopedNames(a *AIAgent) map[string]bool {
	return tools.ToolSearchScopedNames(a.toolContext(), agentExecutorHost{a})
}

func (a *AIAgent) Env() *tools.Env { return a.env }
func (a *AIAgent) TurnID() string  { return a.currentTurnID }

// Task id of the running turn, empty between turns
func (a *AIAgent) TaskID() string           { return a.currentTaskID }
func (a *AIAgent) APIRequestID() string     { return a.currentAPIRequestID }
func (a *AIAgent) QuietMode() bool          { return a.quietMode }
func (a *AIAgent) VerboseLogging() bool     { return a.verboseLogging }
func (a *AIAgent) ToolProgressMode() string { return a.toolProgressMode }
func (a *AIAgent) LogPrefix() string        { return a.logPrefix }
func (a *AIAgent) LogPrefixChars() int      { return a.logPrefixChars }
func (a *AIAgent) GatewaySessionKey() string {
	return a.gatewaySessionKey
}
func (a *AIAgent) CheckpointManager() *tools.CheckpointManager { return a.checkpointMgr }
func (a *AIAgent) SetDelegateSpinner(spinner tools.Spinner)    { a.delegateSpinner = spinner }
func (a *AIAgent) MarkTrimAfterToolBatch()                     { a.trimAfterToolBatch = true }
func (a *AIAgent) IncrementalPersistenceFailed() bool          { return a.incrementalPersistenceFailed }

// Context window of the active compressor, zero when the agent has none
func (a *AIAgent) ContextLength() int {
	if a.contextCompressor == nil {
		return 0
	}
	return a.contextCompressor.Base().ContextLength
}

// Builtin print of the executor progress lines
func (a *AIAgent) Print(text string) { a.safePrint(text, false) }

func (a *AIAgent) VPrint(text string, force bool) { a.vprint(text, force, false) }

func (a *AIAgent) ShouldEmitQuietToolMessages() bool { return a.shouldEmitQuietToolMessages() }

func (a *AIAgent) ShouldStartQuietSpinner() bool { return a.shouldStartQuietSpinner() }

// Started dots spinner on the agent printer behind a random waiting face
func (a *AIAgent) StartSpinner(label string) tools.Spinner {
	ctx := a.homeContext()
	faces, err := waitingFaces(ctx)
	if err != nil {
		panic(err)
	}
	return startKawaiiSpinner(ctx, faces[rand.IntN(len(faces))]+" "+label, "dots", a.printFn)
}

func (a *AIAgent) CuteToolMessage(name string, args map[string]any, duration float64, result any) string {
	return getCuteToolMessage(a.homeContext(), a.env, name, args, duration, result)
}

// Interrupt bit read under the lock its publisher holds
func (a *AIAgent) Interrupted() bool {
	defer icLock(a.pendingRedirectLock)()
	return a.interruptRequested.Load()
}

func (a *AIAgent) ToolInterruptReason() string {
	defer icLock(a.pendingRedirectLock)()
	return a.toolInterruptReason
}

// Names the running tool under the activity lock the summary reads it with
func (a *AIAgent) SetCurrentTool(name string) {
	lock := a.livenessActivityLock()
	lock.Lock()
	a.currentTool = name
	lock.Unlock()
}

// Feeds the turn end verifier, a failed identity probe logged like the executor swallows it
func (a *AIAgent) RecordFileMutationResult(name string, args map[string]any, result string, isError bool, taskID string) {
	if err := a.recordFileMutationResult(name, args, result, isError, taskID); err != nil {
		runAgentLog().Warn("file mutation result not recorded", "tool", name, "error", err)
	}
}

// Settles an onboarding beat from a general tool result when the chat carries setup_beat
func (a *AIAgent) ObserveOnboardingResult(name string, args map[string]any, result string) string {
	return tools.ObserveOnboardingToolResult(a.sessionDB, a.sessionID, a.validToolNames["setup_beat"], name, args, result)
}

func (a *AIAgent) ObserveToolOutcome(name string, isError bool) {
	observability.ObserveToolOutcome(a.toolContext(), a, name, isError)
}

func recordGuardrailDecision(a *AIAgent, action, code string) {
	observability.RecordGuardrailDecision(a.toolContext(), a, action, code)
}

func recordGuardrailWarnings(a *AIAgent, d tools.ToolGuardrailDecision, stallKind string) {
	observability.RecordGuardrailWarnings(a.toolContext(), a, &d, stallKind)
}

// Records the tool result for observability, then repaints automation cards a tool moved mid turn
func (a *AIAgent) NoteToolResult(name, toolCallID, result, persisted string) {
	observability.NoteToolResult(a.toolContext(), a, name, toolCallID, result, persisted)
	a.publishControlIfChanged()
}

// Efficiency rows of the finished batch through their dict views
func (a *AIAgent) RecordToolBatch(batch []*state.Message, contentsBefore []any) {
	rows := make([]map[string]any, len(batch))
	for i, m := range batch {
		if m != nil {
			rows[i] = state.MessageToDict(m)
		}
	}
	observability.RecordToolBatch(a.toolContext(), a, rows, contentsBefore)
}

// Hint text for directories this call visits first, empty without a tracker
func (a *AIAgent) SubdirectoryHints(name string, args map[string]any) string {
	t, _ := a.subdirectoryHints.(*tools.SubdirectoryHintTracker)
	if t == nil {
		return ""
	}
	hints, _ := t.CheckToolCall(name, args)
	return hints
}

func (a *AIAgent) ToolResultContentForActiveModel(name string, result string) any {
	return a.toolResultContentForActiveModel(a.toolContext(), name, result)
}

func (a *AIAgent) ToolResultMetadataCallback() func(toolCallID, name string, args map[string]any, result string) map[string]any {
	cb, _ := a.toolResultMetadataCallback.(func(toolCallID, name string, args map[string]any, result string) map[string]any)
	return cb
}

// Run budget wrap up then the iteration budget warning before the tool result flush
func (a *AIAgent) BeforeToolFlush(messages *[]*state.Message) {
	maybeInjectRunBudgetWrapup(a, *messages)
	maybeInjectIterationBudgetWarning(a, *messages)
}

// Latches the persistence failure, an empty cause keeping the recorded one or reading unknown
func (a *AIAgent) MarkIncrementalPersistenceFailed(cause string) {
	a.incrementalPersistenceFailed = true
	if cause != "" {
		a.lastPersistenceErrorCause = cause
	} else if a.lastPersistenceErrorCause == nil {
		a.lastPersistenceErrorCause = "unknown"
	}
}

// Resets the memory and skill nudge counters when their tool runs
func (a *AIAgent) NoteToolDispatched(name string) {
	switch name {
	case "memory":
		a.turnsSinceMemory = 0
	case "skill_manage":
		a.itersSinceSkill = 0
	}
}

// Context engine handler for a tool the engine registered, called with the live message dicts
func (a *AIAgent) ContextEngineTool(name string) (func(ctx context.Context, args map[string]any, messages *[]*state.Message) (string, error), bool) {
	if !a.contextEngineToolNames[name] || a.contextCompressor == nil {
		return nil, false
	}
	return func(ctx context.Context, args map[string]any, messages *[]*state.Message) (string, error) {
		rows := []map[string]any{}
		if messages != nil {
			rows = make([]map[string]any, 0, len(*messages))
			for _, m := range *messages {
				rows = append(rows, state.MessageToDict(m))
			}
		}
		return a.contextCompressor.HandleToolCall(name, args, map[string]any{"messages": rows}), nil
	}, true
}

// Memory provider tool owner, a nil interface when the agent carries no manager
func (a *AIAgent) MemoryManager() tools.MemoryToolManager {
	if a.memoryManager == nil {
		return nil
	}
	return a.memoryManager
}

// Moves the finished LSP verdicts of this session and task onto the live notes queue
func (a *AIAgent) ForwardDeferredLSPNotes() {
	tools.LSPForwardToAgent(a.sessionID, a.currentTaskID, a.queuePluginNotice)
}

// Live terminal environment of the task, nil when none is active
func (a *AIAgent) ActiveSandbox(taskID string) tools.Environment {
	env, err := tools.GetActiveEnv(a.env, taskID)
	if err != nil {
		return nil
	}
	return env
}

func (a *AIAgent) AttachLiveNotes(msg *state.Message) {
	plugins.AttachToToolMessage(&a.pluginLiveNotes, msg)
}

func (a *AIAgent) KeepLiveNotesAfterTurnBudget(batch []*state.Message) {
	plugins.KeepAfterTurnBudget(&a.pluginLiveNotes, batch)
}

// Queues a plugin notice for the next tool result or user turn
func (a *AIAgent) queuePluginNotice(key, text string) {
	plugins.QueuePluginNotice(&a.pluginLiveNotes, key, text)
}

func popPluginNotices(a *AIAgent) string { return plugins.PopPluginNotices(&a.pluginLiveNotes) }

// Delivers a pending steer as its own user row after the batch, requeued when no tool row landed
func (a *AIAgent) ApplyPendingSteer(messages *[]*state.Message, numTools int) {
	if numTools <= 0 || len(*messages) == 0 {
		return
	}
	steerText := a.drainPendingSteer()
	if steerText == "" {
		return
	}
	found := false
	for j := len(*messages) - 1; j > max(len(*messages)-numTools-1, -1); j-- {
		if m := (*messages)[j]; m != nil && m.Role == "tool" {
			found = true
			break
		}
	}
	if !found {
		requeuePendingSteer(a, steerText)
		return
	}
	*messages = append(*messages, state.MessageFromDict(SteerUserRow(steerText)))
	preview := steerText
	if r := []rune(steerText); len(r) > 120 {
		preview = string(r[:120]) + "..."
	}
	runAgentLog().Info(fmt.Sprintf("Delivered /steer to agent after tool batch (%d chars) as new user message: %s", len([]rune(steerText)), preview))
}

func (a *AIAgent) RedactToolArgsForDisplay(name string, args map[string]any) map[string]any {
	return tools.RedactToolArgsForDisplay(a.env, name, args)
}

func (a *AIAgent) BuildToolPreview(name string, args map[string]any) string {
	return tools.BuildToolPreview(a.env, name, args, nil)
}

func (a *AIAgent) ToolRowEmoji(name string, args map[string]any) string {
	return tools.ToolRowEmoji(a.env, name, args, "⚡")
}

func (a *AIAgent) BuildToolLabel(name string, args map[string]any) string {
	return tools.BuildToolLabel(a.env, name, args, nil)
}
