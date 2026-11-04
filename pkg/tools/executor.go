package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/state"
)

// Sequential and concurrent tool call execution driven by the agent through ExecutorHost

const (
	largeToolResultTrimChars        = 1000000
	maxToolWorkers                  = 8
	defaultImageParallelRequests    = 4
	defaultConcurrentToolTimeoutS   = 420.0
	startOrderGateTimeoutS          = 120.0
	authorizationGateLockTimeoutS   = 360.0
	toolActivityHeartbeatInterval   = 30 * time.Second
	sequentialInterruptPollInterval = time.Second
	PrunedToolArgumentsError        = "suspected_pruned_tool_arguments"
	ReasonUserInterrupt             = "user interrupt"
	defaultConcurrentBatchWaitSlice = 5 * time.Second
	abandonedWorkerGrace            = 3 * time.Second
	heartbeatThreadJoinTimeout      = 2 * time.Second
	sequentialHeartbeatEverySeconds = 30
)

// PrunedToolArgumentsMessage explains why a call carrying a compression marker was refused
var PrunedToolArgumentsMessage = "Tool was not executed because effect-capable arguments contain an icebath context-compression artifact. " +
	"Recover the exact content from its durable source or re-read it, then issue a complete new call; " +
	"do not retry these arguments. To remove a marker that already landed in a file, match it by its " +
	strings.Trim(CompressionMarkerPrefix, "⟪:") + " prefix (e.g. a terminal sed on that line) instead of quoting the full marker."

// Tools that supervise their own liveness and get no generic sequential deadline
var sequentialDeadlineExemptTools = map[string]bool{"delegate_task": true, "manage_connections": true}

// Rounds made only of these mute tool progress
var HousekeepingTools = map[string]bool{"memory": true, "todo_list": true, "skill_manage": true, "session_search": true}

var userInterruptReasons = map[string]bool{ReasonUserInterrupt: true}

// Normalizes a timeout, non positive or NaN means unbounded, else capped at the platform safe maximum
func clampTimeoutSeconds(seconds float64, ok bool) time.Duration {
	if !ok || math.IsNaN(seconds) || seconds <= 0 {
		return 0
	}
	if seconds > MaxSafeTimeoutSeconds {
		seconds = MaxSafeTimeoutSeconds
	}
	return time.Duration(seconds * float64(time.Second))
}

// Resolves timeouts.<key> then the env var then the default, invalid values fall through with a warning
func resolveTimeout(env *Env, key string, def float64, hasDefault bool, envVar string) time.Duration {
	if raw := cfgTree(env).Get("timeouts." + key); raw != nil {
		if _, isBool := raw.(bool); !isBool {
			if f, ok := config.AsFloat(raw); ok && !math.IsNaN(f) {
				return clampTimeoutSeconds(f, true)
			}
		}
		toolsLogger(env).Warn("timeouts: invalid value in config.yaml; ignoring", "key", key, "value", raw)
	}
	if envVar != "" {
		for _, name := range []string{envVar, strings.Replace(envVar, "ICEBATH_", "HERMES_", 1)} {
			rawEnv := strings.TrimSpace(os.Getenv(name))
			if rawEnv == "" {
				continue
			}
			if f, err := strconv.ParseFloat(rawEnv, 64); err == nil {
				return clampTimeoutSeconds(f, true)
			}
			toolsLogger(env).Warn("invalid timeout env value; ignoring", "name", name, "value", rawEnv)
		}
	}
	return clampTimeoutSeconds(def, hasDefault)
}

// Per batch concurrent deadline, zero disables
func resolveConcurrentToolTimeout(env *Env) time.Duration {
	return resolveTimeout(env, "tools.concurrent_batch", defaultConcurrentToolTimeoutS, true, "ICEBATH_CONCURRENT_TOOL_TIMEOUT_S")
}

// Deadline for one sequential call, else the concurrent batch deadline so the two never drift
func resolveSequentialToolTimeout(env *Env) time.Duration {
	concurrent := resolveConcurrentToolTimeout(env)
	return resolveTimeout(env, "tools.sequential_call", concurrent.Seconds(), concurrent > 0, "")
}

// Authorization lock bound equal to the human wait ceiling so a prompt stays answerable
func authorizationGateLockTimeout(env *Env) time.Duration {
	ceiling := humanWaitCeiling(env)
	if ceiling <= 0 {
		return time.Duration(authorizationGateLockTimeoutS * float64(time.Second))
	}
	return time.Duration(ceiling * float64(time.Second))
}

// Configured image generation parallelism cap
func imageGenerateParallelLimit(env *Env) int {
	limit := defaultImageParallelRequests
	if block := cfgTree(env).Map("image_gen"); block != nil {
		if n, ok := toolSearchAsInt(block["max_parallel_requests"]); ok {
			limit = n
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > maxToolWorkers {
		limit = maxToolWorkers
	}
	return limit
}

// InterruptSkipWording words why a call was skipped from the recorded reason
func InterruptSkipWording(reason string) string {
	if reason == "" {
		return "Turn interrupted"
	}
	if userInterruptReasons[reason] {
		return strings.ToUpper(reason[:1]) + reason[1:]
	}
	return "Turn aborted — " + strings.ReplaceAll(reason, "_", " ")
}

// Spinner is the quiet mode progress indicator a host may show for one call
type Spinner interface {
	Stop(text string)
}

// InlineToolContext is the per call state an inline executor may need beyond its arguments
type InlineToolContext struct {
	TaskID     string
	ToolCallID string
	Messages   *[]*state.Message
}

// InlineToolExecutor runs an agent level tool that needs live agent state
type InlineToolExecutor func(ctx context.Context, host ExecutorHost, args map[string]any, ictx InlineToolContext) (string, error)

var (
	inlineExecutorsMu   sync.RWMutex
	inlineToolExecutors = map[string]InlineToolExecutor{}
)

// RegisterInlineToolExecutor installs the agent level executor for a tool name
func RegisterInlineToolExecutor(name string, fn InlineToolExecutor) {
	inlineExecutorsMu.Lock()
	inlineToolExecutors[name] = fn
	inlineExecutorsMu.Unlock()
}

func inlineToolExecutor(name string) InlineToolExecutor {
	inlineExecutorsMu.RLock()
	defer inlineExecutorsMu.RUnlock()
	return inlineToolExecutors[name]
}

// ExecutorHost is the live agent state the executors read and mutate
type ExecutorHost interface {
	Env() *Env
	SessionID() string
	TurnID() string
	APIRequestID() string
	ValidToolNames() map[string]bool
	EnabledToolsets() []string
	DisabledToolsets() []string
	ContextLength() int
	QuietMode() bool
	VerboseLogging() bool
	ToolProgressMode() string
	LogPrefix() string
	LogPrefixChars() int
	Print(text string)
	VPrint(text string, force bool)
	WrapVerbose(prefix, text string) string
	ShouldEmitQuietToolMessages() bool
	ShouldStartQuietSpinner() bool
	StartSpinner(label string) Spinner
	CuteToolMessage(name string, args map[string]any, duration float64, result any) string
	Interrupted() bool
	ToolInterruptReason() string
	Interrupt(message, toolReason string)
	TouchActivity(label string)
	SetCurrentTool(name string)
	Guardrails() *ToolCallGuardrailController
	GuardrailBlockResult(d ToolGuardrailDecision) string
	AppendGuardrailObservation(name string, args map[string]any, result string, failed bool, toolCallID string) string
	RecordFileMutationResult(name string, args map[string]any, result string, isError bool, taskID string)
	ObserveOnboardingResult(name string, args map[string]any, result string) string
	ObserveToolOutcome(name string, isError bool)
	NoteToolResult(name, toolCallID, result, persisted string)
	RecordToolBatch(batch []*state.Message, contentsBefore []any)
	SubdirectoryHints(name string, args map[string]any) string
	ToolResultContentForActiveModel(name string, result string) any
	ToolProgressCallback() func(event, name, preview string, args map[string]any, extra map[string]any)
	ToolStartCallback() func(toolCallID, name string, args map[string]any)
	ToolCompleteCallback() func(toolCallID, name string, args map[string]any, result string)
	ToolResultMetadataCallback() func(toolCallID, name string, args map[string]any, result string) map[string]any
	CheckpointEnabled() bool
	EnsureFileCheckpoint(name string, args map[string]any, taskID string)
	EnsureTerminalCheckpoint(cwd, label string)
	FlushMessagesToSessionDB(messages []*state.Message) (bool, error)
	BeforeToolFlush(messages *[]*state.Message)
	IncrementalPersistenceFailed() bool
	MarkIncrementalPersistenceFailed(cause string)
	NoteToolDispatched(name string)
	DispatchDelegateTask(ctx context.Context, args map[string]any) (string, error)
	ContextEngineTool(name string) (func(ctx context.Context, args map[string]any, messages *[]*state.Message) (string, error), bool)
	MemoryManagerTool(name string) (func(ctx context.Context, args map[string]any) (string, error), bool)
	ActiveSandbox(taskID string) Environment
	AttachLiveNotes(msg *state.Message)
	KeepLiveNotesAfterTurnBudget(batch []*state.Message)
	SyncRewrittenToolRows(batch []*state.Message, contentsBefore []any)
	ApplyPendingSteer(messages *[]*state.Message, numTools int)
	MarkTrimAfterToolBatch()
	SessionSource() string
	RedactToolArgsForDisplay(name string, args map[string]any) map[string]any
	BuildToolPreview(name string, args map[string]any) string
	ToolRowEmoji(name string, args map[string]any) string
	BuildToolLabel(name string, args map[string]any) string
}

// Executor runs tool rounds for one agent
type Executor struct {
	Host       ExecutorHost
	Dispatcher *Dispatcher
	workersMu  sync.Mutex
	workers    map[WorkerID]bool
}

// NewExecutor binds an executor to its host and dispatcher
func NewExecutor(host ExecutorHost, d *Dispatcher) *Executor {
	if d == nil {
		d = NewDispatcher(Default)
	}
	return &Executor{Host: host, Dispatcher: d, workers: map[WorkerID]bool{}}
}

// Tool result budget scaled to the agent's context window, once per turn
func (e *Executor) budget() BudgetConfig {
	return BudgetForContextWindow(e.Host.Env(), e.Host.ContextLength())
}

func (e *Executor) hookIDs(taskID, toolCallID string) ToolHookIDs {
	return ToolHookIDs{TaskID: taskID, SessionID: e.Host.SessionID(), ToolCallID: toolCallID, TurnID: e.Host.TurnID(), APIRequestID: e.Host.APIRequestID()}
}

// Registers a worker for interrupt fan out and clears its bit on exit
func (e *Executor) registerWorker(ctx context.Context) (context.Context, WorkerID, func()) {
	id := NewWorkerID()
	e.workersMu.Lock()
	e.workers[id] = true
	e.workersMu.Unlock()
	return WithWorker(ctx, id), id, func() {
		e.workersMu.Lock()
		delete(e.workers, id)
		e.workersMu.Unlock()
		SetInterrupt(id, false, "")
	}
}

// InterruptWorkers raises the interrupt bit on every tracked worker
func (e *Executor) InterruptWorkers(reason string) {
	e.workersMu.Lock()
	ids := make([]WorkerID, 0, len(e.workers))
	for id := range e.workers {
		ids = append(ids, id)
	}
	e.workersMu.Unlock()
	for _, id := range ids {
		SetInterrupt(id, true, reason)
	}
}

func interruptWorkerIDs(ids []WorkerID, reason string) {
	for _, id := range ids {
		SetInterrupt(id, true, reason)
	}
}

// ParseToolArguments parses model emitted arguments without repairing them
func ParseToolArguments(raw string) (map[string]any, string) {
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
		if m, ok := parsed.(map[string]any); ok {
			return m, ""
		}
	}
	return map[string]any{}, jsonCompact(map[string]any{"error": "Invalid tool arguments", "message": "Tool arguments must be a valid JSON object; tool was not executed."})
}

// ToolCallRef is the identity of one call as every hook and result message sees it
type ToolCallRef struct {
	Name   string
	Args   map[string]any
	TaskID string
	CallID string
	Trace  []map[string]any
}

// Emits the one terminal post tool call for this call
func (e *Executor) emitPost(ctx context.Context, ref *ToolCallRef, result string, durationMS int, status, errorType, errorMessage string, trace []map[string]any) {
	if trace == nil {
		trace = ref.Trace
	}
	e.Dispatcher.EmitPostToolCall(ctx, e.Host.Env(), PostToolCallEvent{ToolName: ref.Name, Args: ref.Args, Result: result,
		IDs: e.hookIDs(ref.TaskID, ref.CallID), DurationMS: durationMS, Status: status, ErrorType: errorType, ErrorMessage: errorMessage,
		MiddlewareTrace: append([]map[string]any(nil), trace...)})
}

func (e *Executor) emitInvalidArguments(ctx context.Context, ref *ToolCallRef, result string) {
	e.emitPost(ctx, ref, result, 0, "error", "invalid_tool_arguments", "Tool arguments must be a valid JSON object", []map[string]any{})
}

// Deferrable names the session may invoke through tool_call read through the definitions memo
func (e *Executor) toolSearchScopedNames(ctx context.Context) map[string]bool {
	defs := e.Dispatcher.Registry.ToolDefinitions(ctx, e.Host.Env(), DefinitionOptions{Enabled: e.Host.EnabledToolsets(), Disabled: e.Host.DisabledToolsets(), Quiet: true, SkipToolSearchAssembly: true})
	return ScopedDeferrableNames(ctx, e.Host.Env(), defs)
}

// Peels the tool_call bridge so downstream hooks see the underlying tool, enforcing session scope here
func (e *Executor) unwrapToolSearchCall(ctx context.Context, name string, args map[string]any, flattenProbe bool) (string, map[string]any, string) {
	if name != ToolCallName {
		return name, args, ""
	}
	env := e.Host.Env()
	underlying, underlyingArgs, errMsg := ResolveUnderlyingCall(ctx, env, args, nil)
	if errMsg != "" || underlying == "" || underlying == ConnectorBatchSentinel {
		return name, args, ""
	}
	if !e.toolSearchScopedNames(ctx)[underlying] {
		return name, args, ScopeBlockReason(ctx, env, underlying)
	}
	scopeBlock := ValidateDeferredCallArgs(ctx, env, underlying, underlyingArgs)
	if scopeBlock == "" {
		return underlying, underlyingArgs, ""
	}
	if flattenProbe {
		var probe map[string]any
		if err := json.Unmarshal([]byte(scopeBlock), &probe); err == nil {
			errText, _ := probe["error"].(string)
			hint, _ := probe["hint"].(string)
			params := probe["parameters"]
			if params == nil {
				params = map[string]any{}
			}
			scopeBlock = strings.TrimSpace(fmt.Sprintf("%s Parameters schema: %s. %s", errText, jsonCompact(params), hint))
		}
	}
	return name, args, scopeBlock
}

// ParsedCall is one model tool call after alias canonicalization, arg parsing, and bridge unwrap
type ParsedCall struct {
	ToolCall   state.ToolCall
	Name       string
	Args       map[string]any
	Trace      []map[string]any
	ParseError string
	ScopeBlock string
}

// Ref builds the call reference every hook and result message sees
func (p *ParsedCall) Ref(taskID string) *ToolCallRef {
	return &ToolCallRef{Name: p.Name, Args: p.Args, TaskID: taskID, CallID: CoalesceToolCallID(p.ToolCall.ID), Trace: p.Trace}
}

// ParseToolCall canonicalizes the name, parses the arguments, and unwraps a bridge call
func (e *Executor) ParseToolCall(ctx context.Context, tc state.ToolCall, flattenProbe bool) *ParsedCall {
	name := CanonicalToolName(tc.Function.Name)
	args, parseError := ParseToolArguments(tc.Function.Arguments)
	scopeBlock := ""
	if parseError == "" {
		name, args, scopeBlock = e.unwrapToolSearchCall(ctx, name, args, flattenProbe)
	}
	return &ParsedCall{ToolCall: tc, Name: name, Args: args, Trace: []map[string]any{}, ParseError: parseError, ScopeBlock: scopeBlock}
}

// ManagedToolResult is the outcome of one managed dispatch
type ManagedToolResult struct {
	Result     string
	Args       map[string]any
	Trace      []map[string]any
	Blocked    bool
	Dispatched bool
	// TimedOut and Cancelled mark synthesized results whose terminal hook already fired
	TimedOut  bool
	Cancelled bool
}

// AuthorizationGate serializes policy prompts and excludes human approval waits from batch deadlines
type AuthorizationGate struct {
	env         *Env
	sem         chan struct{}
	lockTimeout time.Duration
	sessionKey  string
	baseline    float64
}

// NewAuthorizationGate snapshots the session key and human wait baseline on the submitting side
func NewAuthorizationGate(env *Env, lockTimeout time.Duration) *AuthorizationGate {
	if lockTimeout <= 0 {
		lockTimeout = authorizationGateLockTimeout(env)
	}
	g := &AuthorizationGate{env: env, sem: make(chan struct{}, 1), lockTimeout: lockTimeout, sessionKey: getCurrentSessionKey(env, "")}
	g.baseline = g.humanWaitSeconds()
	return g
}

func (g *AuthorizationGate) humanWaitSeconds() float64 {
	return HumanWaitSeconds(g.env, g.sessionKey)
}

// Run executes the callback under the bounded serialization lock, unserialized once the bound expires
func (g *AuthorizationGate) Run(callback func()) {
	select {
	case g.sem <- struct{}{}:
		defer func() { <-g.sem }()
		callback()
	case <-time.After(g.lockTimeout):
		toolsLogger(g.env).Warn("authorization gate lock not acquired (holder wedged in a pre_tool_call plugin or approval round-trip?); running prompt unserialized",
			"seconds", g.lockTimeout.Seconds())
		callback()
	}
}

// ExcludedSeconds is the human approval wait accrued since the batch started
func (g *AuthorizationGate) ExcludedSeconds() float64 {
	return math.Max(0, g.humanWaitSeconds()-g.baseline)
}

// Stamps activity every interval until stopped or the worker is interrupted
func (e *Executor) runWithActivityHeartbeat(ctx context.Context, name string, fn func() (string, error)) (string, error) {
	stop := make(chan struct{})
	done := make(chan struct{})
	worker := WorkerOf(ctx)
	go func() {
		defer close(done)
		ticker := time.NewTicker(toolActivityHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if IsWorkerInterrupted(worker) {
					return
				}
				e.Host.TouchActivity("tool running: " + name)
			}
		}
	}()
	defer func() {
		close(stop)
		select {
		case <-done:
		case <-time.After(heartbeatThreadJoinTimeout):
		}
	}()
	return fn()
}

// Synthesizes the result for a blocked call and emits its terminal post tool call
func (e *Executor) blockedToolResult(ctx context.Context, ref *ToolCallRef, blockBody map[string]any, blockErrorType string, decision *ToolGuardrailDecision) string {
	var result, errorType, errorMessage string
	if blockBody != nil {
		result = jsonCompact(blockBody)
		errorType = blockErrorType
		if msg, ok := blockBody["message"].(string); ok && msg != "" {
			errorMessage = msg
		} else {
			errorMessage, _ = blockBody["error"].(string)
		}
	} else {
		result = e.Host.GuardrailBlockResult(*decision)
		errorType = "guardrail_block"
		errorMessage = decision.Message
		if errorMessage == "" {
			errorMessage = "Tool blocked by guardrail policy"
		}
	}
	e.emitPost(ctx, ref, result, 0, "blocked", errorType, errorMessage, nil)
	return result
}

// Runs pre tool call plugin hooks returning the block message and the final args
func (e *Executor) preToolBlock(ctx context.Context, ref *ToolCallRef) (string, map[string]any) {
	block, modified := e.Dispatcher.dispatchPreToolCallHooks(ctx, e.Host.Env(), ref.Name, ref.Args, e.hookIDs(ref.TaskID, ref.CallID), ref.Trace, DispatchOptions{})
	if modified == nil {
		return block, ref.Args
	}
	return block, modified
}

type dispatchOnceInput struct {
	execute        func(ctx context.Context, args map[string]any) (string, error)
	scopeBlock     string
	displayIndex   int
	beginExecution func(callback func()) error
	gate           *AuthorizationGate
	slot           *terminalSlot
}

// Icebath policy then the one real dispatch, advancing the start order exactly once on every path
func (e *Executor) dispatchAuthorizedOnce(ctx context.Context, st *ManagedToolResult, ref *ToolCallRef, in dispatchOnceInput) (string, error) {
	advance := func(callback func()) error {
		if in.beginExecution != nil {
			return in.beginExecution(callback)
		}
		if callback != nil {
			callback()
		}
		return nil
	}
	blockMessage, blockErrorType := in.scopeBlock, "tool_scope_block"
	if blockMessage == "" {
		blockErrorType = "plugin_block"
		resolve := func() { blockMessage, ref.Args = e.preToolBlock(ctx, ref) }
		if in.gate == nil {
			resolve()
		} else {
			in.gate.Run(resolve)
		}
		st.Args = ref.Args
	}
	var blockBody map[string]any
	if blockMessage != "" {
		blockBody = map[string]any{"error": blockMessage}
	}
	if blockBody == nil {
		if pruned := ContextPrunedArgumentPaths(ref.Name, ref.Args); len(pruned) > 0 {
			paths := make([]any, len(pruned))
			for i, p := range pruned {
				paths[i] = p
			}
			blockBody = map[string]any{"error": PrunedToolArgumentsError, "message": PrunedToolArgumentsMessage, "argument_paths": paths}
			blockErrorType = PrunedToolArgumentsError
		}
	}
	var decision *ToolGuardrailDecision
	if blockBody == nil {
		d := e.Host.Guardrails().BeforeCall(ref.Name, ref.Args)
		if !d.AllowsExecution() {
			decision = &d
		}
	}
	if blockBody != nil || decision != nil {
		if err := advance(nil); err != nil {
			return "", err
		}
		st.Blocked = true
		return e.blockedToolResult(ctx, ref, blockBody, blockErrorType, decision), nil
	}
	e.Host.NoteToolDispatched(ref.Name)
	if in.slot != nil && ref.Name == "terminal" {
		if err := in.slot.prepare(ctx, ref); err != nil {
			return "", err
		}
	}
	if err := advance(func() { e.beginToolExecution(ref, in.displayIndex) }); err != nil {
		return "", err
	}
	return e.runWithActivityHeartbeat(ctx, ref.Name, func() (string, error) { return in.execute(ctx, ref.Args) })
}

type managedInput struct {
	name           string
	args           map[string]any
	taskID         string
	toolCallID     string
	execute        func(ctx context.Context, args map[string]any) (string, error)
	scopeBlock     string
	displayIndex   int
	trace          []map[string]any
	beginExecution func(callback func()) error
	gate           *AuthorizationGate
	slot           *terminalSlot
}

// Runs relay rewrites before icebath policy and dispatches exactly once
func (e *Executor) runToolExecutionMiddleware(ctx context.Context, in managedInput) (*ManagedToolResult, error) {
	env := e.Host.Env()
	trace := in.trace
	if trace == nil {
		trace = []map[string]any{}
	}
	st := &ManagedToolResult{Args: in.args, Trace: trace}
	var dispatchMu sync.Mutex
	ids := e.hookIDs(in.taskID, in.toolCallID)
	authorizedDispatch := func(finalArgs map[string]any) (string, error) {
		dispatchMu.Lock()
		if st.Dispatched {
			dispatchMu.Unlock()
			return "", fmt.Errorf("icebath tool execution callback invoked more than once")
		}
		st.Dispatched = true
		st.Blocked = false
		st.Args = finalArgs
		dispatchMu.Unlock()
		if in.slot != nil {
			if err := in.slot.checkCancelled(); err != nil {
				return "", err
			}
		}
		ref := &ToolCallRef{Name: in.name, Args: finalArgs, TaskID: in.taskID, CallID: in.toolCallID, Trace: trace}
		return e.dispatchAuthorizedOnce(ctx, st, ref, dispatchOnceInput{execute: in.execute, scopeBlock: in.scopeBlock,
			displayIndex: in.displayIndex, beginExecution: in.beginExecution, gate: in.gate, slot: in.slot})
	}
	pipeline := func(relayArgs map[string]any) (string, error) {
		requestArgs, _, requestTrace := e.Dispatcher.ApplyToolRequestMiddleware(ctx, env, in.name, relayArgs, in.args, ids)
		trace = trace[:0]
		trace = append(trace, requestTrace...)
		st.Trace = trace
		return e.Dispatcher.RunToolExecutionMiddleware(ctx, env, in.name, requestArgs, in.args, ids, func(nextArgs map[string]any) (string, error) {
			if nextArgs == nil {
				nextArgs = requestArgs
			}
			return authorizedDispatch(nextArgs)
		})
	}
	var result string
	var err error
	if e.Dispatcher.Relay != nil {
		result, _, err = e.Dispatcher.Relay(ctx, env, in.name, in.args, ids, pipeline)
	} else {
		result, err = pipeline(in.args)
	}
	if err != nil {
		return st, err
	}
	st.Result = result
	st.Trace = trace
	return st, nil
}

// Runs the user visible and checkpoint preflight on final tool arguments
func (e *Executor) beginToolExecution(ref *ToolCallRef, displayIndex int) {
	host := e.Host
	displayArgs := host.RedactToolArgsForDisplay(ref.Name, ref.Args)
	if displayArgs == nil {
		displayArgs = ref.Args
	}
	if e.toolProgressEnabled() {
		prefix := "Tool"
		if displayIndex > 0 {
			prefix = fmt.Sprintf("Tool %d", displayIndex)
		}
		if host.VerboseLogging() {
			host.Print(fmt.Sprintf("  \U0001F4DE %s: %s(%v)", prefix, ref.Name, sortedKeys(displayArgs)))
			host.Print(host.WrapVerbose("Args: ", jsonIndent(displayArgs)))
		} else {
			host.Print(fmt.Sprintf("  \U0001F4DE %s: %s(%v) - %s", prefix, ref.Name, sortedKeys(ref.Args), previewText(jsonCompact(displayArgs), host.LogPrefixChars())))
		}
	}
	host.SetCurrentTool(ref.Name)
	host.TouchActivity("executing tool: " + ref.Name)
	if cb := host.ToolProgressCallback(); cb != nil {
		preview := host.BuildToolPreview(ref.Name, displayArgs)
		safeCallback(func() { cb("tool.started", ref.Name, preview, displayArgs, nil) }, "Tool progress")
	}
	if cb := host.ToolStartCallback(); cb != nil {
		safeCallback(func() { cb(ref.CallID, ref.Name, displayArgs) }, "Tool start")
	}
	if !host.CheckpointEnabled() {
		return
	}
	func() {
		defer func() { recover() }()
		switch ref.Name {
		case "write_file", "patch":
			host.EnsureFileCheckpoint(ref.Name, ref.Args, ref.TaskID)
		case "terminal":
			command, _ := ref.Args["command"].(string)
			if IsDestructiveCommand(command) {
				cwd, _ := ref.Args["workdir"].(string)
				label := command
				if len(label) > 60 {
					label = label[:60]
				}
				host.EnsureTerminalCheckpoint(cwd, "before terminal: "+label)
			}
		}
	}()
}

func safeCallback(fn func(), label string) {
	defer func() {
		if rec := recover(); rec != nil {
			envLogDebug(label+" callback error", rec)
		}
	}()
	fn()
}

func envLogDebug(msg string, rec any) {
	toolsLogger(nil).Debug(msg, "panic", rec)
}

func previewText(text string, limit int) string {
	if len(text) > limit {
		return text[:limit] + "..."
	}
	return text
}

func (e *Executor) toolProgressEnabled() bool {
	return !e.Host.QuietMode() && e.Host.ToolProgressMode() != "off"
}

// Fires the completion callback unless blocked, then the output risk projection
func (e *Executor) emitToolCompleteAndRisk(ref *ToolCallRef, result string, risk map[string]any, blocked bool) {
	host := e.Host
	if !blocked {
		if cb := host.ToolCompleteCallback(); cb != nil {
			displayArgs := host.RedactToolArgsForDisplay(ref.Name, ref.Args)
			if displayArgs == nil {
				displayArgs = ref.Args
			}
			safeCallback(func() { cb(ref.CallID, ref.Name, displayArgs, result) }, "Tool complete")
		}
	}
	if risk != nil {
		if level, _ := risk["risk"].(string); level != "low" {
			if cb := host.ToolProgressCallback(); cb != nil {
				safeCallback(func() {
					cb("tool.output_risk", ref.Name, "", nil, map[string]any{"tool_call_id": ref.CallID, "risk_metadata": risk})
				}, "Tool output risk")
			}
		}
	}
}

// Flushes tool progress to the session DB before projecting it to any UI
func (e *Executor) flushAfterToolProgress(messages *[]*state.Message, stage string) bool {
	host := e.Host
	host.BeforeToolFlush(messages)
	persisted, err := host.FlushMessagesToSessionDB(*messages)
	if err != nil {
		host.MarkIncrementalPersistenceFailed(ClassifyPersistenceError(err))
		toolsLogger(host.Env()).Warn("Incremental tool-call persistence failed", "stage", stage, "error", err)
		return false
	}
	if !persisted {
		host.MarkIncrementalPersistenceFailed("")
	}
	return persisted
}

// Remembers the spillover path a persisted result landed at so a later stub cannot dangle
func (e *Executor) recordPersistedPathForStub(toolCallID string, result string) {
	candidates := []string{result}
	if env, ok := multimodalEnvelope(result); ok {
		candidates = nil
		if summary, ok := env["text_summary"].(string); ok {
			candidates = append(candidates, summary)
		}
		if parts, ok := env["content"].([]any); ok {
			for _, p := range parts {
				if m, ok := p.(map[string]any); ok {
					if t, ok := m["text"].(string); ok {
						candidates = append(candidates, t)
					}
				}
			}
		}
	}
	for _, c := range candidates {
		if p := ExtractPersistedPath(c); p != "" {
			e.Host.Guardrails().RecordPersistedResult(toolCallID, p)
			return
		}
	}
}

// Parses a multimodal envelope JSON string into its map
func multimodalEnvelope(result string) (map[string]any, bool) {
	if !strings.Contains(result, "_multimodal") {
		return nil, false
	}
	m, ok := safeJSONLoads(result).(map[string]any)
	if !ok || !IsMultimodalToolResult(m) {
		return nil, false
	}
	return m, true
}

// Spills oversized text parts of a multimodal envelope through the string persistence policy
func (e *Executor) persistMultimodalTextParts(ctx context.Context, envelope map[string]any, toolName, toolCallID string, sandbox Environment, budget BudgetConfig) map[string]any {
	parts, _ := envelope["content"].([]any)
	var bounded []any
	firstReplacement := ""
	spilled := 0
	for _, part := range parts {
		m, ok := part.(map[string]any)
		if ok {
			if kind, _ := m["type"].(string); kind == "text" {
				if text, ok := m["text"].(string); ok {
					partID := toolCallID
					if spilled > 0 {
						partID = fmt.Sprintf("%s_part%d", toolCallID, spilled)
					}
					replaced := MaybePersistToolResult(ctx, e.Host.Env(), sandbox, text, toolName, partID, budget, ThresholdResolve)
					if replaced != text {
						copied := make(map[string]any, len(m))
						for k, v := range m {
							copied[k] = v
						}
						copied["text"] = replaced
						part = copied
						if firstReplacement == "" {
							firstReplacement = replaced
						}
						spilled++
					}
				}
			}
		}
		bounded = append(bounded, part)
	}
	if firstReplacement == "" {
		return envelope
	}
	out := make(map[string]any, len(envelope))
	for k, v := range envelope {
		out[k] = v
	}
	out["content"] = bounded
	if summary, ok := out["text_summary"].(string); ok && len(summary) > budget.ResolveThreshold(e.Host.Env(), toolName) {
		out["text_summary"] = firstReplacement
	}
	return out
}

// Committed is the outcome of committing one result
type committed struct {
	persisted string
	display   string
	risk      map[string]any
}

type commitInput struct {
	budget            BudgetConfig
	duration          float64
	isError           bool
	blocked           bool
	effectDisposition string
	observed          bool
	errorPreview      func(string) string
	successLogChars   int
	logSuccess        bool
	verboseText       func(string) string
}

// Observes, logs, persists, wraps, appends, flushes, and projects one tool result
func (e *Executor) commitToolResult(ctx context.Context, messages *[]*state.Message, ref *ToolCallRef, result string, in commitInput) *committed {
	host := e.Host
	log := toolsLogger(host.Env())
	host.ObserveToolOutcome(ref.Name, in.isError)
	if !(in.blocked || in.isError) {
		result = host.ObserveOnboardingResult(ref.Name, ref.Args, result)
	}
	if in.observed {
		if !in.blocked {
			result = host.AppendGuardrailObservation(ref.Name, ref.Args, result, in.isError, ref.CallID)
		}
		if in.isError {
			preview := result
			if in.errorPreview != nil {
				preview = in.errorPreview(result)
			}
			log.Warn("Tool returned error", "tool", ref.Name, "seconds", fmt.Sprintf("%.2f", in.duration), "result", preview)
		} else if in.logSuccess {
			log.Info("tool completed", "tool", ref.Name, "seconds", fmt.Sprintf("%.2f", in.duration), "chars", in.successLogChars)
		}
		if !in.blocked {
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						log.Debug("file-mutation verifier record failed", "panic", rec)
					}
				}()
				host.RecordFileMutationResult(ref.Name, ref.Args, result, in.isError, ref.TaskID)
			}()
		}
		if host.VerboseLogging() {
			text := result
			if in.verboseText != nil {
				text = in.verboseText(result)
			}
			log.Debug("Tool completed", "tool", ref.Name, "seconds", fmt.Sprintf("%.2f", in.duration))
			log.Debug("Tool result", "chars", len(text), "text", text)
		}
	}
	host.SetCurrentTool("")
	suffix := ""
	if in.isError {
		suffix = " (error)"
	}
	host.TouchActivity(fmt.Sprintf("tool completed: %s (%.1fs)%s", ref.Name, in.duration, suffix))
	sandbox := host.ActiveSandbox(ref.TaskID)
	persisted := result
	var persistedEnvelope map[string]any
	if envelope, ok := multimodalEnvelope(result); ok {
		persistedEnvelope = e.persistMultimodalTextParts(ctx, envelope, ref.Name, ref.CallID, sandbox, in.budget)
		persisted = jsonCompact(persistedEnvelope)
	} else {
		persisted = MaybePersistToolResult(ctx, host.Env(), sandbox, result, ref.Name, ref.CallID, in.budget, ThresholdResolve)
	}
	e.recordPersistedPathForStub(ref.CallID, persisted)
	host.NoteToolResult(ref.Name, ref.CallID, result, persisted)
	if hints := host.SubdirectoryHints(ref.Name, ref.Args); hints != "" {
		if persistedEnvelope != nil {
			AppendSubdirHintToMultimodal(persistedEnvelope, hints)
			persisted = jsonCompact(persistedEnvelope)
		} else {
			persisted += hints
		}
	}
	content := host.ToolResultContentForActiveModel(ref.Name, persisted)
	toolMessage := MakeToolResultMessage(ref.Name, content, ref.CallID, in.effectDisposition)
	host.AttachLiveNotes(toolMessage)
	if !in.blocked {
		if cb := host.ToolResultMetadataCallback(); cb != nil {
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						log.Debug("Tool result metadata callback error", "panic", rec)
					}
				}()
				displayArgs := host.RedactToolArgsForDisplay(ref.Name, ref.Args)
				if displayArgs == nil {
					displayArgs = ref.Args
				}
				if metadata := cb(ref.CallID, ref.Name, displayArgs, result); len(metadata) > 0 {
					if toolMessage.DisplayMetadata == nil {
						toolMessage.DisplayMetadata = map[string]any{}
					}
					for k, v := range metadata {
						toolMessage.DisplayMetadata[k] = v
					}
				}
			}()
		}
	}
	*messages = append(*messages, toolMessage)
	if !e.flushAfterToolProgress(messages, "tool result "+ref.Name) {
		return nil
	}
	if !in.blocked {
		if cb := host.ToolProgressCallback(); cb != nil {
			safeCallback(func() {
				cb("tool.completed", ref.Name, "", nil, map[string]any{"duration": in.duration, "is_error": in.isError, "result": result})
			}, "Tool progress")
		}
	}
	if len(result) >= largeToolResultTrimChars {
		host.MarkTrimAfterToolBatch()
	}
	var risk map[string]any
	if toolMessage.DisplayMetadata != nil {
		risk, _ = toolMessage.DisplayMetadata[ToolOutputRiskKey].(map[string]any)
	}
	return &committed{persisted: persisted, display: result, risk: risk}
}

// Per turn aggregate budget enforcement then steer injection
func (e *Executor) finalizeToolBatch(ctx context.Context, messages *[]*state.Message, taskID string, numTools int, budget BudgetConfig) {
	if numTools <= 0 || numTools > len(*messages) {
		return
	}
	batch := (*messages)[len(*messages)-numTools:]
	before := make([]any, len(batch))
	for i, m := range batch {
		before[i] = m.Content
	}
	EnforceTurnBudget(ctx, e.Host.Env(), e.Host.ActiveSandbox(taskID), batch, budget)
	e.Host.KeepLiveNotesAfterTurnBudget(batch)
	e.Host.SyncRewrittenToolRows(batch, before)
	e.Host.RecordToolBatch(batch, before)
	e.Host.ApplyPendingSteer(messages, numTools)
}

func (e *Executor) printToolCompleted(index int, duration float64, result string) {
	host := e.Host
	if host.VerboseLogging() {
		host.Print(fmt.Sprintf("  ✅ Tool %d completed in %.2fs", index, duration))
		host.Print(host.WrapVerbose("Result: ", result))
		return
	}
	host.Print(fmt.Sprintf("  ✅ Tool %d completed in %.2fs - %s", index, duration, previewText(result, host.LogPrefixChars())))
}

type skipOptions struct {
	content            string
	hookErrorType      string
	hookID             func(state.ToolCall) string
	flushStage         string
	stopOnFlushFailure bool
	hasFlushStage      bool
}

// Appends one tool result per unstarted call so the assistant turn never lacks matching results
func (e *Executor) appendSkippedToolResults(ctx context.Context, messages *[]*state.Message, toolCalls []state.ToolCall, taskID string, opts skipOptions) bool {
	wording := InterruptSkipWording(e.Host.ToolInterruptReason())
	for _, tc := range toolCalls {
		name := tc.Function.Name
		if name == "" {
			name = "tool"
		}
		result := strings.ReplaceAll(opts.content, "{name}", name)
		*messages = append(*messages, MakeToolResultMessage(name, result, CoalesceToolCallID(tc.ID), "none"))
		if opts.hookErrorType != "" {
			id := CoalesceToolCallID(tc.ID)
			if opts.hookID != nil {
				id = opts.hookID(tc)
			}
			ref := &ToolCallRef{Name: name, Args: map[string]any{}, TaskID: taskID, CallID: id, Trace: []map[string]any{}}
			e.emitPost(ctx, ref, result, 0, "cancelled", opts.hookErrorType, "Tool execution skipped. "+wording, nil)
		}
		if opts.hasFlushStage {
			if !e.flushAfterToolProgress(messages, opts.flushStage+" "+name) && opts.stopOnFlushFailure {
				return false
			}
		}
	}
	return true
}

func cancelledContent(wording string) string {
	return "[Tool execution cancelled — {name} was skipped. " + wording + "]"
}

// Concurrent batch machinery

type toolOutcome struct {
	ref      *ToolCallRef
	result   string
	duration float64
	isError  bool
	blocked  bool
}

// Gate bound under the batch deadline so parked workers are never falsely reported timed out
func startOrderGateTimeout(batchTimeout time.Duration) time.Duration {
	stock := time.Duration(startOrderGateTimeoutS * float64(time.Second))
	if batchTimeout <= 0 {
		return stock
	}
	if half := batchTimeout / 2; half < stock {
		return half
	}
	return stock
}

var errBatchAbandoned = fmt.Errorf("batch abandoned before dispatch")

// StartOrderGate serializes worker dispatch by submit order so prompts appear in call order
type StartOrderGate struct {
	mu        sync.Mutex
	cond      *sync.Cond
	nextOrder int
	timeout   time.Duration
	abandoned bool
	env       *Env
}

func newStartOrderGate(env *Env, timeout time.Duration) *StartOrderGate {
	g := &StartOrderGate{timeout: timeout, env: env}
	g.cond = sync.NewCond(&g.mu)
	return g
}

// Abandon releases every parked worker so none dispatches a tool the turn already gave up on
func (g *StartOrderGate) Abandon() {
	g.mu.Lock()
	g.abandoned = true
	g.mu.Unlock()
	g.cond.Broadcast()
}

// Waits for order, runs the callback, and advances, false when abandoned
func (g *StartOrderGate) beginInOrder(order int, callback func(), toolName string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	deadline := time.Now().Add(g.timeout)
	inOrder := true
	for g.nextOrder < order && !g.abandoned {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			inOrder = false
			break
		}
		timer := time.AfterFunc(remaining, g.cond.Broadcast)
		g.cond.Wait()
		timer.Stop()
	}
	if g.abandoned {
		return false
	}
	if !inOrder {
		toolsLogger(g.env).Warn("start-order gate timed out; proceeding out of order", "tool", toolName, "order", order, "next", g.nextOrder)
	}
	defer func() {
		if order+1 > g.nextOrder {
			g.nextOrder = order + 1
		}
		g.cond.Broadcast()
	}()
	if callback != nil {
		callback()
	}
	return true
}

// workerStartOnce is one worker's handle on the gate that advances at most once
type workerStartOnce struct {
	gate     *StartOrderGate
	order    int
	toolName string
	advanced bool
	mu       sync.Mutex
}

func (w *workerStartOnce) advance(callback func()) error {
	w.mu.Lock()
	if w.advanced {
		w.mu.Unlock()
		return nil
	}
	w.advanced = true
	w.mu.Unlock()
	if !w.gate.beginInOrder(w.order, callback, w.toolName) {
		return errBatchAbandoned
	}
	return nil
}

type concurrentBatch struct {
	exec        *Executor
	messages    *[]*state.Message
	taskID      string
	parsed      []*ParsedCall
	timeout     time.Duration
	results     []*toolOutcome
	resultsMu   sync.Mutex
	gate        *StartOrderGate
	authGate    *AuthorizationGate
	timedOut    map[int]bool
	workerIDs   map[int]WorkerID
	workerIDsMu sync.Mutex
}

func (e *Executor) newConcurrentBatch(messages *[]*state.Message, taskID string, parsed []*ParsedCall, timeout time.Duration) *concurrentBatch {
	b := &concurrentBatch{exec: e, messages: messages, taskID: taskID, parsed: parsed, timeout: timeout,
		results: make([]*toolOutcome, len(parsed)), timedOut: map[int]bool{}, workerIDs: map[int]WorkerID{}}
	for i, pc := range parsed {
		if pc.ParseError != "" {
			b.results[i] = &toolOutcome{ref: pc.Ref(taskID), result: pc.ParseError, duration: 0, isError: true, blocked: true}
		}
	}
	b.gate = newStartOrderGate(e.Host.Env(), startOrderGateTimeout(timeout))
	b.authGate = NewAuthorizationGate(e.Host.Env(), 0)
	return b
}

// Runs one call through the middleware and synthesizes its slot outcome, nil when abandoned at the gate
func (b *concurrentBatch) dispatchWorker(ctx context.Context, index int, ref *ToolCallRef, scopeBlock string, startGate *workerStartOnce) *toolOutcome {
	e := b.exec
	start := time.Now()
	blocked, dispatched := false, false
	var result string
	managed, err := e.runToolExecutionMiddleware(ctx, managedInput{name: ref.Name, args: ref.Args, taskID: ref.TaskID, toolCallID: ref.CallID,
		execute: func(ctx context.Context, next map[string]any) (string, error) {
			return e.invokeTool(ctx, ref.Name, next, ref.TaskID, ref.CallID, b.messages, ref.Trace)
		},
		scopeBlock: scopeBlock, displayIndex: index + 1, trace: ref.Trace, beginExecution: startGate.advance, gate: b.authGate})
	if err == errBatchAbandoned {
		toolsLogger(e.Host.Env()).Info("tool abandoned at start-order gate; skipping dispatch", "tool", ref.Name)
		return nil
	}
	if err != nil {
		result = fmt.Sprintf("Error executing tool '%s': %s", ref.Name, err.Error())
		toolsLogger(e.Host.Env()).Error("invoke tool raised", "tool", ref.Name, "error", err)
	} else {
		result, ref.Args, ref.Trace = managed.Result, managed.Args, managed.Trace
		blocked, dispatched = managed.Blocked, managed.Dispatched
	}
	duration := time.Since(start).Seconds()
	if !blocked && !dispatched {
		e.emitPost(ctx, ref, result, int(duration*1000), "", "", "", nil)
	}
	isError, _ := ClassifyToolFailure(ref.Name, &result)
	if isError {
		toolsLogger(e.Host.Env()).Info("tool failed", "tool", ref.Name, "seconds", fmt.Sprintf("%.2f", duration), "result", previewText(result, 200))
	} else {
		toolsLogger(e.Host.Env()).Info("tool completed", "tool", ref.Name, "seconds", fmt.Sprintf("%.2f", duration), "chars", len(result))
	}
	return &toolOutcome{ref: ref, result: result, duration: duration, isError: isError, blocked: blocked}
}

func (b *concurrentBatch) runWorker(ctx context.Context, index, startOrder int) {
	e := b.exec
	pc := b.parsed[index]
	ctx, id, release := e.registerWorker(ctx)
	defer release()
	b.workerIDsMu.Lock()
	b.workerIDs[index] = id
	b.workerIDsMu.Unlock()
	if e.Host.Interrupted() {
		SetInterrupt(id, true, e.Host.ToolInterruptReason())
	}
	startGate := &workerStartOnce{gate: b.gate, order: startOrder, toolName: pc.Name}
	defer startGate.advance(nil)
	outcome := b.dispatchWorker(ctx, index, pc.Ref(b.taskID), pc.ScopeBlock, startGate)
	if outcome != nil {
		b.resultsMu.Lock()
		b.results[index] = outcome
		b.resultsMu.Unlock()
	}
}

func (b *concurrentBatch) runningNames(done []chan struct{}, runnable []int) []string {
	var names []string
	for k, ch := range done {
		select {
		case <-ch:
		default:
			names = append(names, b.parsed[runnable[k]].Name)
		}
	}
	return names
}

// Waits with periodic heartbeats and interrupt checks, true when the batch was abandoned
func (b *concurrentBatch) awaitCompletion(done []chan struct{}, runnable []int, deadline time.Time, hasDeadline bool) bool {
	e := b.exec
	host := e.Host
	start := time.Now()
	for {
		waitTimeout := defaultConcurrentBatchWaitSlice
		if hasDeadline {
			remaining := time.Until(deadline.Add(time.Duration(b.authGate.ExcludedSeconds() * float64(time.Second))))
			if remaining > 0 && remaining < waitTimeout {
				waitTimeout = remaining
			}
			if remaining <= 0 {
				waitTimeout = 0
			}
		}
		if waitTimeout > 0 {
			waitAll(done, waitTimeout)
		}
		pending := 0
		for _, ch := range done {
			select {
			case <-ch:
			default:
				pending++
			}
		}
		if pending == 0 {
			return false
		}
		timedOut := hasDeadline && !time.Now().Before(deadline.Add(time.Duration(b.authGate.ExcludedSeconds()*float64(time.Second))))
		if timedOut {
			for k, ch := range done {
				select {
				case <-ch:
				default:
					b.timedOut[runnable[k]] = true
				}
			}
			names := b.runningNames(done, runnable)
			if len(names) > 5 {
				names = names[:5]
			}
			toolsLogger(host.Env()).Warn("concurrent tool batch timed out", "seconds", fmt.Sprintf("%.1f", b.timeout.Seconds()), "still_running", len(b.timedOut), "tools", strings.Join(names, ", "))
		} else if host.Interrupted() {
			host.VPrint(fmt.Sprintf("%s⚡ Interrupt: cancelling %d pending concurrent tool(s)", host.LogPrefix(), pending), true)
		} else {
			elapsed := int(time.Since(start).Seconds())
			if elapsed > 0 && elapsed%30 < 6 {
				still := b.runningNames(done, runnable)
				if len(still) > 3 {
					still = still[:3]
				}
				host.TouchActivity(fmt.Sprintf("concurrent tools running (%ds, %d remaining: %s)", elapsed, pending, strings.Join(still, ", ")))
			}
			continue
		}
		b.gate.Abandon()
		if timedOut {
			e.InterruptWorkers("")
		} else {
			waitAll(done, abandonedWorkerGrace)
		}
		return true
	}
}

// Waits until every channel closes or the timeout elapses
func waitAll(done []chan struct{}, timeout time.Duration) {
	deadline := time.After(timeout)
	for _, ch := range done {
		select {
		case <-ch:
		case <-deadline:
			return
		}
	}
}

// Dispatches the runnable calls on bounded goroutines and waits for the batch
func (b *concurrentBatch) run(ctx context.Context) {
	var runnable []int
	for i, pc := range b.parsed {
		if pc.ParseError == "" {
			runnable = append(runnable, i)
		}
	}
	if len(runnable) == 0 {
		return
	}
	hasDeadline := b.timeout > 0
	deadline := time.Now().Add(b.timeout)
	maxWorkers := maxToolWorkers
	for _, i := range runnable {
		if b.parsed[i].Name == "image_generate" {
			if limit := imageGenerateParallelLimit(b.exec.Host.Env()); limit < maxWorkers {
				maxWorkers = limit
			}
		}
	}
	if len(runnable) < maxWorkers {
		maxWorkers = len(runnable)
	}
	sem := make(chan struct{}, maxWorkers)
	done := make([]chan struct{}, len(runnable))
	for submitIndex, i := range runnable {
		ch := make(chan struct{})
		done[submitIndex] = ch
		go func(index, order int, ch chan struct{}) {
			defer close(ch)
			sem <- struct{}{}
			defer func() { <-sem }()
			b.runWorker(ctx, index, order)
		}(i, submitIndex, ch)
	}
	if b.awaitCompletion(done, runnable, deadline, hasDeadline) {
		b.gate.Abandon()
	}
}

// Synthesizes the result for a slot no worker filled and emits its terminal post tool call
func (e *Executor) unfinishedToolResult(ctx context.Context, ref *ToolCallRef, timedOut bool, timeout time.Duration) (string, float64, string) {
	host := e.Host
	switch {
	case timedOut:
		suffix := "the configured timeout"
		if timeout > 0 {
			suffix = fmt.Sprintf("%.1fs", timeout.Seconds())
		}
		result := fmt.Sprintf("Error executing tool '%s': timed out after %s", ref.Name, suffix)
		e.emitPost(ctx, ref, result, int(timeout.Milliseconds()), "timeout", "tool_timeout", result, nil)
		return result, timeout.Seconds(), "unknown"
	case host.Interrupted():
		why := InterruptSkipWording(host.ToolInterruptReason())
		result := fmt.Sprintf("[Tool execution cancelled — %s was skipped. %s]", ref.Name, why)
		e.emitPost(ctx, ref, result, 0, "cancelled", "keyboard_interrupt", "Tool execution cancelled. "+why, nil)
		return result, 0, ""
	}
	result := fmt.Sprintf("Error executing tool '%s': thread did not return a result", ref.Name)
	e.emitPost(ctx, ref, result, 0, "error", "thread_missing_result", result, nil)
	return result, 0, ""
}

// Appends every slot's result in original call order, false at the first failed flush
func (e *Executor) appendBatchResults(ctx context.Context, messages *[]*state.Message, taskID string, b *concurrentBatch, budget BudgetConfig) bool {
	host := e.Host
	for i, pc := range b.parsed {
		b.resultsMu.Lock()
		r := b.results[i]
		b.resultsMu.Unlock()
		var ref *ToolCallRef
		var result, effect string
		var duration float64
		var isError, blocked bool
		if r == nil {
			ref, isError, blocked = pc.Ref(taskID), true, false
			result, duration, effect = e.unfinishedToolResult(ctx, ref, b.timedOut[i], b.timeout)
		} else {
			ref, result, duration, isError, blocked = r.ref, r.result, r.duration, r.isError, r.blocked
			if blocked {
				effect = "none"
			}
			if pc.ParseError != "" {
				e.emitInvalidArguments(ctx, ref, r.result)
			}
		}
		c := e.commitToolResult(ctx, messages, ref, result, commitInput{budget: budget, duration: duration, isError: isError, blocked: blocked,
			effectDisposition: effect, observed: r != nil, errorPreview: func(res string) string { return previewText(MultimodalTextSummary(multimodalOrString(res)), 200) }})
		if c == nil {
			return false
		}
		if host.ShouldEmitQuietToolMessages() {
			host.Print("  " + host.CuteToolMessage(ref.Name, ref.Args, duration, c.display))
		} else if e.toolProgressEnabled() {
			e.printToolCompleted(i+1, duration, MultimodalTextSummary(multimodalOrString(c.display)))
		}
		e.emitToolCompleteAndRisk(ref, c.display, c.risk, blocked)
	}
	return true
}

// A multimodal envelope map when the string holds one, else the string
func multimodalOrString(result string) any {
	if m, ok := multimodalEnvelope(result); ok {
		return m
	}
	return result
}

// ExecuteToolCallsConcurrent runs calls concurrently and appends results in original order
func (e *Executor) ExecuteToolCallsConcurrent(ctx context.Context, toolCalls []state.ToolCall, messages *[]*state.Message, taskID string, finalize bool) {
	host := e.Host
	numTools := len(toolCalls)
	budget := e.budget()
	if host.Interrupted() {
		host.Print(fmt.Sprintf("%s⚡ Interrupt: skipping %d tool call(s)", host.LogPrefix(), numTools))
		e.appendSkippedToolResults(ctx, messages, toolCalls, taskID, skipOptions{content: cancelledContent(InterruptSkipWording(host.ToolInterruptReason())),
			hookErrorType: "user_interrupt", flushStage: "cancelled tool result", hasFlushStage: true, stopOnFlushFailure: false})
		return
	}
	parsed := make([]*ParsedCall, len(toolCalls))
	names := make([]string, len(toolCalls))
	for i, tc := range toolCalls {
		parsed[i] = e.ParseToolCall(ctx, tc, false)
		names[i] = parsed[i].Name
	}
	namesStr := strings.Join(names, ", ")
	if e.toolProgressEnabled() {
		host.Print(fmt.Sprintf("  ⚡ Concurrent: %d tool calls — %s", numTools, namesStr))
	}
	timeout := resolveConcurrentToolTimeout(host.Env())
	batch := e.newConcurrentBatch(messages, taskID, parsed, timeout)
	host.SetCurrentTool(namesStr)
	host.TouchActivity(fmt.Sprintf("executing %d tools concurrently: %s", numTools, namesStr))
	spinner := e.startQuietToolSpinner("", nil, true, fmt.Sprintf("⚡ running %d tools concurrently", numTools))
	func() {
		defer func() {
			if spinner != nil {
				finished, total := 0, 0.0
				batch.resultsMu.Lock()
				for _, r := range batch.results {
					if r != nil {
						finished++
						total += r.duration
					}
				}
				batch.resultsMu.Unlock()
				spinner.Stop(fmt.Sprintf("⚡ %d/%d tools completed in %.1fs total", finished, numTools, total))
			}
		}()
		batch.run(ctx)
	}()
	if !e.appendBatchResults(ctx, messages, taskID, batch, budget) {
		return
	}
	if finalize {
		e.finalizeToolBatch(ctx, messages, taskID, len(parsed), budget)
	}
}

// Sequential dispatch

// Starts the quiet mode spinner for one call or returns nil
func (e *Executor) startQuietToolSpinner(name string, args map[string]any, gate bool, label string) Spinner {
	host := e.Host
	if !host.ShouldEmitQuietToolMessages() || (gate && !host.ShouldStartQuietSpinner()) {
		return nil
	}
	if label == "" {
		displayArgs := host.RedactToolArgsForDisplay(name, args)
		if displayArgs == nil {
			displayArgs = args
		}
		text := host.BuildToolLabel(name, displayArgs)
		if text == "" {
			text = name
		}
		label = host.ToolRowEmoji(name, displayArgs) + " " + text
	}
	return host.StartSpinner(label)
}

// Stops the spinner with the cute completion line or prints it when no spinner ran
func (e *Executor) finishQuietToolSpinner(spinner Spinner, name string, args map[string]any, duration float64, result string) {
	host := e.Host
	if spinner != nil || host.ShouldEmitQuietToolMessages() {
		cute := host.CuteToolMessage(name, args, duration, multimodalOrString(result))
		if spinner != nil {
			spinner.Stop(cute)
		} else {
			host.VPrint("  "+cute, false)
		}
	}
}

func delegateSpinnerLabel(args map[string]any) string {
	action := ""
	if a, ok := args["action"]; ok && a != nil {
		action = strings.ToLower(strings.TrimSpace(fmt.Sprint(a)))
	}
	switch action {
	case "list", "steer", "stop":
		return "\U0001F500 subagent " + action
	}
	if tasks, ok := args["tasks"].([]any); ok && len(tasks) > 0 {
		return fmt.Sprintf("\U0001F500 delegating %d tasks · (/agents to monitor)", len(tasks))
	}
	goal, _ := args["goal"].(string)
	if len([]rune(goal)) > 30 {
		goal = string([]rune(goal)[:30])
	}
	if goal != "" {
		return "\U0001F500 " + goal + " · (/agents to monitor)"
	}
	return "\U0001F500 delegating · (/agents to monitor)"
}

// sequentialDispatch is how one sequential call executes with its spinner and error policy
type sequentialDispatch struct {
	execute                func(ctx context.Context, args map[string]any) (string, error)
	spinner                Spinner
	trace                  []map[string]any
	errorResult            func(error) string
	errorLog               string
	handlesInterruptCancel bool
	isDelegate             bool
	finishSpinner          bool
	finishInFinally        bool
	transformApplied       bool
}

// Picks the execute callable for one sequential call and starts its spinner
func (e *Executor) resolveSequentialDispatch(ctx context.Context, ref *ToolCallRef, messages *[]*state.Message) *sequentialDispatch {
	host := e.Host
	name, args := ref.Name, ref.Args
	if name != "delegate_task" {
		if inline := inlineToolExecutor(name); inline != nil {
			ictx := InlineToolContext{TaskID: ref.TaskID, ToolCallID: ref.CallID, Messages: messages}
			return &sequentialDispatch{execute: func(ctx context.Context, next map[string]any) (string, error) { return inline(ctx, host, next, ictx) },
				finishSpinner: true, finishInFinally: false}
		}
	}
	if name == "delegate_task" {
		spinner := e.startQuietToolSpinner(name, args, true, delegateSpinnerLabel(args))
		return &sequentialDispatch{execute: func(ctx context.Context, next map[string]any) (string, error) {
			return host.DispatchDelegateTask(ctx, next)
		},
			spinner: spinner, isDelegate: true, finishSpinner: true, finishInFinally: true}
	}
	if fn, ok := host.ContextEngineTool(name); ok {
		return &sequentialDispatch{execute: func(ctx context.Context, next map[string]any) (string, error) { return fn(ctx, next, messages) },
			spinner: e.startQuietToolSpinner(name, args, false, ""),
			errorResult: func(err error) string {
				return jsonCompact(map[string]any{"error": fmt.Sprintf("Context engine tool '%s' failed: %s", name, err.Error())})
			},
			errorLog: "context engine handle_tool_call raised", finishSpinner: true, finishInFinally: true}
	}
	if fn, ok := host.MemoryManagerTool(name); ok {
		return &sequentialDispatch{execute: func(ctx context.Context, next map[string]any) (string, error) { return fn(ctx, next) },
			spinner: e.startQuietToolSpinner(name, args, true, ""),
			errorResult: func(err error) string {
				return jsonCompact(map[string]any{"error": fmt.Sprintf("Memory tool '%s' failed: %s", name, err.Error())})
			},
			errorLog: "memory manager handle_tool_call raised", finishSpinner: true, finishInFinally: true}
	}
	execute := func(ctx context.Context, next map[string]any) (string, error) {
		return e.dispatchRegistry(ctx, name, next, ref.TaskID, ref.CallID, ref.Trace), nil
	}
	var spinner Spinner
	if host.QuietMode() {
		spinner = e.startQuietToolSpinner(name, args, true, "")
	}
	return &sequentialDispatch{execute: execute, spinner: spinner, trace: ref.Trace,
		errorResult: func(err error) string { return fmt.Sprintf("Error executing tool '%s': %s", name, err.Error()) },
		errorLog:    "handle_function_call raised", handlesInterruptCancel: true, finishSpinner: host.QuietMode(), finishInFinally: true, transformApplied: true}
}

// Registry tools go through the dispatcher with the inner observer suppressed
func (e *Executor) dispatchRegistry(ctx context.Context, name string, args map[string]any, taskID, toolCallID string, trace []map[string]any) string {
	host := e.Host
	var enabledTools []string
	if valid := host.ValidToolNames(); len(valid) > 0 {
		enabledTools = sortedSet(valid)
	}
	call := &Call{Name: name, Args: args, ToolCallID: toolCallID, TaskID: taskID, SessionID: host.SessionID(), TurnID: host.TurnID(),
		APIRequestID: host.APIRequestID(), Env: host.Env()}
	return e.Dispatcher.DispatchWith(ctx, call, DispatchOptions{SkipPreToolCallHook: true, SkipToolRequestMiddleware: true,
		SkipToolExecutionMiddleware: true, SuppressPostToolCallHook: true, Trace: append([]map[string]any(nil), trace...),
		EnabledTools: enabledTools, EnabledToolsets: host.EnabledToolsets(), DisabledToolsets: host.DisabledToolsets()})
}

// Resolves inline agent level tools then the registry for the concurrent path
func (e *Executor) invokeTool(ctx context.Context, name string, args map[string]any, taskID, toolCallID string, messages *[]*state.Message, trace []map[string]any) (string, error) {
	if inline := inlineToolExecutor(name); inline != nil {
		return inline(ctx, e.Host, args, InlineToolContext{TaskID: taskID, ToolCallID: toolCallID, Messages: messages})
	}
	if name == "delegate_task" {
		return e.Host.DispatchDelegateTask(ctx, args)
	}
	if fn, ok := e.Host.ContextEngineTool(name); ok {
		return fn(ctx, args, messages)
	}
	if fn, ok := e.Host.MemoryManagerTool(name); ok {
		return fn(ctx, args)
	}
	return e.dispatchRegistry(ctx, name, args, taskID, toolCallID, trace), nil
}

// Waits for the worker in interrupt poll slices extending the deadline by human approval wait
func (e *Executor) pollSequentialFuture(done <-chan struct{}, name string, deadline time.Time, hasDeadline bool, started time.Time, gate *AuthorizationGate) string {
	host := e.Host
	lastHeartbeat := 0
	for {
		waitSlice := sequentialInterruptPollInterval
		if hasDeadline {
			remaining := time.Until(deadline.Add(time.Duration(gate.ExcludedSeconds() * float64(time.Second))))
			if remaining <= 0 {
				return "timeout"
			}
			if remaining < waitSlice {
				waitSlice = remaining
			}
		}
		select {
		case <-done:
			return "done"
		case <-time.After(waitSlice):
		}
		select {
		case <-done:
			return "done"
		default:
		}
		if host.Interrupted() {
			return "interrupted"
		}
		elapsed := int(time.Since(started).Seconds())
		if elapsed-lastHeartbeat >= sequentialHeartbeatEverySeconds {
			lastHeartbeat = elapsed
			host.TouchActivity(fmt.Sprintf("sequential tool running (%ds): %s", elapsed, name))
		}
	}
}

type sequentialResult struct {
	managed *ManagedToolResult
	err     error
}

// Runs one sequential call on a worker goroutine under the executor's deadline
func (e *Executor) runSequentialToolExecutionMiddleware(ctx context.Context, in managedInput, prepared *terminalSlot) (*ManagedToolResult, error) {
	host := e.Host
	env := host.Env()
	var timeout time.Duration
	if !sequentialDeadlineExemptTools[in.name] {
		timeout = resolveSequentialToolTimeout(env)
	}
	timeout = extendToolTimeout(ctx, env, in.name, timeout)
	ref := &ToolCallRef{Name: in.name, Args: in.args, TaskID: in.taskID, CallID: in.toolCallID, Trace: in.trace}
	if prepared != nil {
		if err := prepared.checkCancelled(); err != nil {
			return nil, err
		}
	}
	if NeverParallelTools[in.name] {
		return e.runToolExecutionMiddleware(ctx, in)
	}
	var gate *AuthorizationGate
	var done chan struct{}
	var outcome *sequentialResult
	var workerIDs []WorkerID
	var workerMu sync.Mutex
	if prepared != nil {
		gate = prepared.batch.authGate
		done = prepared.done
		workerIDs = prepared.workerIDs()
		outcome = prepared.result()
	} else {
		gate = NewAuthorizationGate(env, 0)
		done = make(chan struct{})
		result := &sequentialResult{}
		outcome = result
		in.gate = gate
		go func() {
			defer close(done)
			wctx, id, release := e.registerWorker(ctx)
			defer release()
			workerMu.Lock()
			workerIDs = append(workerIDs, id)
			workerMu.Unlock()
			result.managed, result.err = e.runToolExecutionMiddleware(wctx, in)
		}()
	}
	started := time.Now()
	deadline := started.Add(timeout)
	st := e.pollSequentialFuture(done, in.name, deadline, timeout > 0, started, gate)
	if st == "done" {
		return outcome.managed, outcome.err
	}
	workerMu.Lock()
	ids := append([]WorkerID(nil), workerIDs...)
	workerMu.Unlock()
	if prepared != nil {
		ids = prepared.workerIDs()
	}
	var message string
	var managed *ManagedToolResult
	if st == "interrupted" {
		interruptWorkerIDs(ids, host.ToolInterruptReason())
		select {
		case <-done:
			return outcome.managed, outcome.err
		case <-time.After(abandonedWorkerGrace):
		}
		reason := host.ToolInterruptReason()
		if reason == "" {
			reason = "interrupt requested"
		}
		message = fmt.Sprintf("[Tool execution cancelled — %s was abandoned: %s]", in.name, reason)
		toolsLogger(env).Info("sequential tool abandoned", "tool", in.name, "reason", reason, "seconds", fmt.Sprintf("%.1f", time.Since(started).Seconds()))
		e.emitPost(ctx, ref, message, int(time.Since(started).Milliseconds()), "cancelled", "tool_interrupted", "Tool execution cancelled: "+reason, nil)
		managed = &ManagedToolResult{Result: message, Args: ref.Args, Trace: ref.Trace, Dispatched: true, Cancelled: true}
	} else {
		message = fmt.Sprintf("Error executing tool '%s': timed out after %.1fs", in.name, timeout.Seconds())
		toolsLogger(env).Warn("sequential tool timed out", "tool", in.name, "seconds", fmt.Sprintf("%.1f", timeout.Seconds()))
		e.emitPost(ctx, ref, message, int(timeout.Milliseconds()), "timeout", "tool_timeout", message, nil)
		managed = &ManagedToolResult{Result: message, Args: ref.Args, Trace: ref.Trace, Dispatched: true, TimedOut: true}
	}
	if prepared != nil {
		prepared.batch.close()
		if st == "timeout" {
			host.Interrupt("", "terminal batch timeout")
		}
	}
	if st == "timeout" {
		interruptWorkerIDs(ids, "")
	}
	return managed, nil
}

func (e *Executor) skipRemainingSequential(ctx context.Context, messages *[]*state.Message, remaining []state.ToolCall, taskID, notice string, opts skipOptions) bool {
	e.Host.VPrint(fmt.Sprintf("%s⚡ Interrupt: skipping %d %s", e.Host.LogPrefix(), len(remaining), notice), true)
	return e.appendSkippedToolResults(ctx, messages, remaining, taskID, opts)
}

func (e *Executor) appendInvalidArgumentsResult(ctx context.Context, messages *[]*state.Message, ref *ToolCallRef, parseError string) bool {
	e.emitInvalidArguments(ctx, ref, parseError)
	*messages = append(*messages, MakeToolResultMessage(ref.Name, parseError, ref.CallID, ""))
	return e.flushAfterToolProgress(messages, "invalid tool arguments "+ref.Name)
}

// Runs one sequential call with its spinner and error policy
func (e *Executor) runSequentialCall(ctx context.Context, dispatch *sequentialDispatch, ref *ToolCallRef, scopeBlock string, messages *[]*state.Message, remaining []state.ToolCall, displayIndex int, start time.Time, prepared *terminalSlot) (*ManagedToolResult, float64) {
	host := e.Host
	var spinnerResult string
	managed, err := e.runSequentialToolExecutionMiddleware(ctx, managedInput{name: ref.Name, args: ref.Args, taskID: ref.TaskID, toolCallID: ref.CallID,
		execute: dispatch.execute, scopeBlock: scopeBlock, displayIndex: displayIndex, trace: dispatch.trace}, prepared)
	if err == nil {
		ref.Args = managed.Args
		spinnerResult = managed.Result
	} else if dispatch.errorResult != nil {
		result := dispatch.errorResult(err)
		toolsLogger(host.Env()).Error(dispatch.errorLog, "tool", ref.Name, "error", err)
		managed = &ManagedToolResult{Result: result, Args: ref.Args, Trace: ref.Trace}
	} else {
		managed = &ManagedToolResult{Result: fmt.Sprintf("Error executing tool '%s': %s", ref.Name, err.Error()), Args: ref.Args, Trace: ref.Trace}
	}
	duration := time.Since(start).Seconds()
	if dispatch.finishSpinner {
		e.finishQuietToolSpinner(dispatch.spinner, ref.Name, ref.Args, duration, spinnerResult)
	}
	return managed, duration
}

// Terminal hook, observe, commit, and completion callbacks for one sequential result
func (e *Executor) publishSequentialResult(ctx context.Context, messages *[]*state.Message, ref *ToolCallRef, managed *ManagedToolResult, duration float64, index int, budget BudgetConfig, transformApplied bool) bool {
	host := e.Host
	ref.Args, ref.Trace = managed.Args, managed.Trace
	result := managed.Result
	executionTimedOut := managed.TimedOut || managed.Cancelled
	durationMS := int(duration * 1000)
	if !managed.Blocked && !executionTimedOut {
		e.emitPost(ctx, ref, result, durationMS, "", "", "", nil)
		if !transformApplied {
			result = e.Dispatcher.ApplyTransformToolResult(ctx, host.Env(), PostToolCallEvent{ToolName: ref.Name, Args: ref.Args, Result: result,
				IDs: e.hookIDs(ref.TaskID, ref.CallID), DurationMS: durationMS})
		}
	}
	isError, _ := ClassifyToolFailure(ref.Name, &result)
	effect := ""
	if executionTimedOut {
		effect = "unknown"
	}
	c := e.commitToolResult(ctx, messages, ref, result, commitInput{budget: budget, duration: duration, isError: isError, blocked: managed.Blocked,
		effectDisposition: effect, observed: true,
		errorPreview: func(res string) string {
			if host.VerboseLogging() {
				return res
			}
			return previewText(res, 200)
		},
		successLogChars: len(result), logSuccess: true,
		verboseText: func(res string) string { return MultimodalTextSummary(multimodalOrString(res)) }})
	if c == nil {
		return false
	}
	markTerminalBatchOutcome(ctx, isError || managed.Blocked)
	e.emitToolCompleteAndRisk(ref, c.display, c.risk, managed.Blocked)
	if e.toolProgressEnabled() {
		e.printToolCompleted(index, duration, c.persisted)
	}
	return true
}

// ExecuteToolCallsSequential runs calls one at a time with adjacent terminal calls batched for approval
func (e *Executor) ExecuteToolCallsSequential(ctx context.Context, toolCalls []state.ToolCall, messages *[]*state.Message, taskID string, finalize bool) {
	for _, run := range e.terminalApprovalRuns(ctx, toolCalls) {
		bctx, closeBatch := e.terminalApprovalBatch(ctx, run, messages, taskID)
		e.executeToolCallsSequential(bctx, run, messages, taskID)
		closeBatch()
		if e.Host.IncrementalPersistenceFailed() {
			return
		}
	}
	if finalize {
		e.finalizeToolBatch(ctx, messages, taskID, len(toolCalls), e.budget())
	}
}

func (e *Executor) executeToolCallsSequential(ctx context.Context, toolCalls []state.ToolCall, messages *[]*state.Message, taskID string) {
	host := e.Host
	budget := e.budget()
	wording := func() string { return InterruptSkipWording(host.ToolInterruptReason()) }
	for i, tc := range toolCalls {
		if host.IncrementalPersistenceFailed() {
			return
		}
		if host.Interrupted() {
			if !e.skipRemainingSequential(ctx, messages, toolCalls[i:], taskID, "tool call(s)", skipOptions{content: cancelledContent(wording()),
				hookErrorType: "user_interrupt", hookID: func(tc state.ToolCall) string { return tc.ID }, flushStage: "cancelled tool result", hasFlushStage: true, stopOnFlushFailure: true}) {
				return
			}
			break
		}
		pc := e.ParseToolCall(ctx, tc, true)
		ref := pc.Ref(taskID)
		if pc.ParseError != "" {
			if !e.appendInvalidArgumentsResult(ctx, messages, ref, pc.ParseError) {
				return
			}
			continue
		}
		start := time.Now()
		prepared := takePreparedCall(ctx, ref.CallID)
		dispatch := e.resolveSequentialDispatch(ctx, ref, messages)
		managed, duration := e.runSequentialCall(ctx, dispatch, ref, pc.ScopeBlock, messages, toolCalls[i:], i+1, start, prepared)
		if !e.publishSequentialResult(ctx, messages, ref, managed, duration, i+1, budget, dispatch.transformApplied) {
			return
		}
		if host.Interrupted() && i+1 < len(toolCalls) {
			if !e.skipRemainingSequential(ctx, messages, toolCalls[i+1:], taskID, "remaining tool call(s)", skipOptions{
				content: "[Tool execution skipped — {name} was not started. " + wording() + "]", flushStage: "skipped tool result", hasFlushStage: true, stopOnFlushFailure: true}) {
				return
			}
			break
		}
	}
}

// ExecuteToolCallsSegmented runs a mixed batch as ordered parallel and sequential segments
func (e *Executor) ExecuteToolCallsSegmented(ctx context.Context, toolCalls []state.ToolCall, messages *[]*state.Message, taskID string, segments []BatchSegment) {
	host := e.Host
	if segments == nil {
		execCwd := ""
		if sandbox := host.ActiveSandbox(taskID); sandbox != nil {
			execCwd = sandbox.Cwd()
		}
		segments = PlanToolBatchSegments(ctx, host.Env(), toolCalls, execCwd)
	}
	for _, seg := range segments {
		if host.IncrementalPersistenceFailed() {
			return
		}
		if seg.Mode == "parallel" {
			e.ExecuteToolCallsConcurrent(ctx, seg.Calls, messages, taskID, false)
		} else {
			e.ExecuteToolCallsSequential(ctx, seg.Calls, messages, taskID, false)
		}
		if host.IncrementalPersistenceFailed() {
			return
		}
	}
	if len(toolCalls) > 0 {
		e.finalizeToolBatch(ctx, messages, taskID, len(toolCalls), e.budget())
	}
}

var diskFullMarkers = []string{"no space left on device", "not enough space", "database or disk is full", "disk full", "full disk", "enospc"}

var dbCorruptionMarkers = []string{"malformed", "file is not a database", "not a database", "database corruption"}

type persistenceCausePhrase struct {
	markers []string
	cause   string
}

var persistenceCauseByPhrase = []persistenceCausePhrase{
	{[]string{"turn lease"}, "turn_lease"},
	{[]string{"closed by compression"}, "compression_closed"},
	{[]string{"being compressed", "compression lease"}, "compression"},
	{[]string{"deleted state.db-wal", "deleted state.db-shm"}, "deleted_wal"},
	{[]string{"was replaced underneath"}, "replaced"},
	{dbCorruptionMarkers, "corrupt"},
	{[]string{"locked", "busy"}, "locked"},
	{[]string{"foreign key constraint failed"}, "session_row_missing"},
}

func containsAnyMarker(text string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// Corruption SQLite attributes to the FTS index layer rather than the transcript store
func isFTSScopedCorruption(text string) bool {
	if !containsAnyMarker(text, dbCorruptionMarkers) {
		return false
	}
	return strings.Contains(text, "fts5:") || strings.Contains(text, "messages_fts")
}

// ClassifyPersistenceError buckets a session write failure so the user's guidance matches the cause
func ClassifyPersistenceError(err error) string {
	if err == nil {
		return "unknown"
	}
	text := strings.ToLower(err.Error())
	if isFTSScopedCorruption(text) {
		return "fts_index"
	}
	for _, p := range persistenceCauseByPhrase {
		if containsAnyMarker(text, p.markers) {
			return p.cause
		}
	}
	if containsAnyMarker(text, diskFullMarkers) || containsAnyMarker(text, []string{"disk", "readonly", "read-only"}) {
		return "disk"
	}
	return "unknown"
}
