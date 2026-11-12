package tools

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Tools the agent loop intercepts because they need live agent state
var AgentLoopTools = map[string]bool{"todo_list": true, "memory": true, "session_search": true, "delegate_task": true}

var (
	legacyAliasMu     sync.RWMutex
	legacyToolAliases = map[string]string{
		"todo": "todo_list", "cronjob": "cronjob_manage", "process": "process_manage", "tour": "gui_tour", "tip": "show_tip",
	}
)

// RegisterLegacyToolAlias accepts an old tool name at every dispatch seam
func RegisterLegacyToolAlias(alias, canonical string) {
	legacyAliasMu.Lock()
	legacyToolAliases[alias] = canonical
	legacyAliasMu.Unlock()
}

// CanonicalToolName maps a legacy alias to the advertised name
func CanonicalToolName(name string) string {
	legacyAliasMu.RLock()
	defer legacyAliasMu.RUnlock()
	if c, ok := legacyToolAliases[name]; ok {
		return c
	}
	return name
}

var readSearchTools = map[string]bool{"read_file": true, "search_files": true}

const toolErrorMaxLen = 2048

var toolErrorStripRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)</?(?:tool_call|function_call|result|response|output|input|system|assistant|user)>`),
	regexp.MustCompile("(?m)^\\s*```(?:json|xml|html|markdown)?\\s*"),
	regexp.MustCompile("(?m)\\s*```\\s*$"),
	regexp.MustCompile(`(?s)<!\[CDATA\[.*?\]\]>`),
}

// SanitizeToolError strips structural framing tokens from an error before the model reads it
func SanitizeToolError(errorMsg string) string {
	if errorMsg == "" {
		return "[TOOL_ERROR] "
	}
	sanitized := errorMsg
	for _, p := range toolErrorStripRes {
		sanitized = p.ReplaceAllString(sanitized, "")
	}
	if len(pystr.Runes(sanitized)) > toolErrorMaxLen {
		sanitized = pystr.Slice(sanitized, 0, toolErrorMaxLen-3) + "..."
	}
	return "[TOOL_ERROR] " + sanitized
}

// ToolHookIDs are the identity fields every hook and middleware call carries
type ToolHookIDs struct {
	TaskID       string
	SessionID    string
	ToolCallID   string
	TurnID       string
	APIRequestID string
}

func hookIDsOf(call *Call) ToolHookIDs {
	return ToolHookIDs{TaskID: call.TaskID, SessionID: call.SessionID, ToolCallID: call.ToolCallID, TurnID: call.TurnID, APIRequestID: call.APIRequestID}
}

// PreToolDirective is one pre tool call hook answer with block, approve, or modify semantics
type PreToolDirective struct {
	Action  string
	Message string
	RuleKey string
	Args    map[string]any
}

// PreToolCallHook inspects a call before dispatch
type PreToolCallHook func(ctx context.Context, env *Env, name string, args map[string]any, ids ToolHookIDs, trace []map[string]any) PreToolDirective

// PostToolCallEvent describes one finished tool call for observers
type PostToolCallEvent struct {
	ToolName        string
	Args            map[string]any
	Result          string
	IDs             ToolHookIDs
	DurationMS      int
	Status          string
	ErrorType       string
	ErrorMessage    string
	MiddlewareTrace []map[string]any
}

// PostToolCallHook observes a finished tool call
type PostToolCallHook func(ctx context.Context, env *Env, ev PostToolCallEvent)

// TransformToolResultHook may replace the result string before it enters context
type TransformToolResultHook func(ctx context.Context, env *Env, ev PostToolCallEvent) (string, bool)

// RequestMiddlewareDirective replaces the effective arguments when Args is set
type RequestMiddlewareDirective struct {
	Args   map[string]any
	Source string
	Reason string
	Name   string
	// Trace entries of a chain that ran inside this middleware, recorded as given in place of the single entry
	Trace []map[string]any
}

// ToolRequestMiddleware rewrites arguments before hooks, guardrails, approvals, and execution
type ToolRequestMiddleware func(ctx context.Context, env *Env, name string, args, originalArgs map[string]any, ids ToolHookIDs) RequestMiddlewareDirective

// ToolExecutionMiddleware wraps the one downstream execution and must call next at most once
type ToolExecutionMiddleware func(ctx context.Context, env *Env, name string, args, originalArgs map[string]any, ids ToolHookIDs, next func(map[string]any) (string, error)) (string, error)

// RelayFunc lets a relay rewrite a call around the icebath pipeline
type RelayFunc func(ctx context.Context, env *Env, name string, args map[string]any, ids ToolHookIDs, pipeline func(map[string]any) (string, error)) (string, map[string]any, error)

// EditApprovalFunc blocks a file mutation until an editor client approves it
type EditApprovalFunc func(ctx context.Context, env *Env, name string, args map[string]any) (string, error)

// Dispatcher routes tool calls through hooks and middleware to the registry
type Dispatcher struct {
	Registry                *Registry
	mu                      sync.RWMutex
	PreToolCall             []PreToolCallHook
	PostToolCall            []PostToolCallHook
	TransformToolResult     []TransformToolResultHook
	ToolRequestMiddleware   []ToolRequestMiddleware
	ToolExecutionMiddleware []ToolExecutionMiddleware
	EditApproval            EditApprovalFunc
	Relay                   RelayFunc
	// SafeWorker disables every observer hook for a worker that must not run plugin code
	SafeWorker func() bool
}

// NewDispatcher returns a dispatcher over the registry
func NewDispatcher(r *Registry) *Dispatcher {
	if r == nil {
		r = Default
	}
	return &Dispatcher{Registry: r, EditApproval: MaybeRequireEditApproval}
}

func (d *Dispatcher) safeWorker() bool {
	return d.SafeWorker != nil && d.SafeWorker()
}

func (d *Dispatcher) hasHook(kind string) bool {
	if d.safeWorker() {
		return false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	switch kind {
	case "post_tool_call":
		return len(d.PostToolCall) > 0
	case "transform_tool_result":
		return len(d.TransformToolResult) > 0
	case "pre_tool_call":
		return len(d.PreToolCall) > 0
	}
	return false
}

// DispatchOptions tune one call through the dispatcher
type DispatchOptions struct {
	SkipPreToolCallHook         bool
	SkipToolRequestMiddleware   bool
	SkipToolExecutionMiddleware bool
	SuppressPostToolCallHook    bool
	Trace                       []map[string]any
	// EnabledTools picks execute_code's sandbox tools, nil means the last resolved definition names
	EnabledTools []string
	// Enabled and Disabled toolsets scope the bridge catalog, nil means the session environment's
	EnabledToolsets  []string
	DisabledToolsets []string
	// AllowedTools denies every other name with DeniedFormat, nil means no whitelist
	AllowedTools map[string]bool
	DeniedFormat string
}

type enabledToolsKey struct{}

type postHookSuppressedKey struct{}

type dispatcherKey struct{}

// SuppressPostToolCallHook lets an outer executor own the terminal post tool event, false re-enables it
func SuppressPostToolCallHook(ctx context.Context, suppressed bool) context.Context {
	return context.WithValue(ctx, postHookSuppressedKey{}, suppressed)
}

// PostToolCallHookSuppressed reports whether an outer executor owns the post tool event for this context
func PostToolCallHookSuppressed(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(postHookSuppressedKey{}).(bool)
	return v
}

// WithDispatcher binds the dispatcher whose hooks nested tool calls from a handler run through
func WithDispatcher(ctx context.Context, d *Dispatcher) context.Context {
	return context.WithValue(ctx, dispatcherKey{}, d)
}

// DispatcherOf returns the bound dispatcher, the process default when none is bound
func DispatcherOf(ctx context.Context) *Dispatcher {
	if ctx != nil {
		if d, _ := ctx.Value(dispatcherKey{}).(*Dispatcher); d != nil {
			return d
		}
	}
	return DefaultDispatcher
}

// DefaultDispatcher carries the process wide plugin hooks and middleware over the default registry
var DefaultDispatcher = NewDispatcher(Default)

// HandleFunctionCall routes one tool call through hooks and middleware to the registry and returns its JSON result
func HandleFunctionCall(ctx context.Context, call *Call) string {
	return DispatcherOf(ctx).DispatchWith(ctx, call, DispatchOptions{})
}

// Bare Python style class name of an error for observer hooks
func exceptionTypeName(err error) string {
	t := reflect.TypeOf(err)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Name() == "" {
		return fmt.Sprintf("%T", err)
	}
	return t.Name()
}

// Relay rewrite applied at the authorization boundary before request middleware
var relayToolRequestIntercepts = ApplyRelayToolRequestIntercepts

// Relay intercepts then the request middleware chain, failing open to the caller's args and trace
func (d *Dispatcher) applyRequestMiddleware(ctx context.Context, env *Env, name string, args map[string]any, ids ToolHookIDs, trace []map[string]any) (map[string]any, map[string]any, []map[string]any) {
	current := args
	var relayTrace []map[string]any
	if ids.SessionID != "" {
		relayArgs, err := relayToolRequestIntercepts(ctx, ids.SessionID, name, schemaDeepCopy(args).(map[string]any))
		if err != nil {
			toolsLogger(env).Debug("tool_request middleware error", "error", err)
			fallback := make(map[string]any, len(args))
			for k, v := range args {
				fallback[k] = v
			}
			return args, fallback, trace
		}
		if !reflect.DeepEqual(relayArgs, args) {
			current = schemaDeepCopy(relayArgs).(map[string]any)
			relayTrace = []map[string]any{{"source": "nemo_relay"}}
		}
	}
	payload, original, mwTrace := d.ApplyToolRequestMiddleware(ctx, env, name, current, args, ids)
	out := append(relayTrace, mwTrace...)
	if out == nil {
		out = []map[string]any{}
	}
	return payload, original, out
}

// WithEnabledTools carries the sandbox tool list execute_code reads
func WithEnabledTools(ctx context.Context, names []string) context.Context {
	return context.WithValue(ctx, enabledToolsKey{}, names)
}

// EnabledToolsOf returns the sandbox tool list bound to the context
func EnabledToolsOf(ctx context.Context) ([]string, bool) {
	if ctx == nil {
		return nil, false
	}
	names, ok := ctx.Value(enabledToolsKey{}).([]string)
	return names, ok
}

// Derives status, error type, and error message from a tool result for observer hooks
func toolResultObserverFields(toolName, result string) (string, string, string) {
	if decoded, err := pyjson.Loads(result); err == nil {
		if data, ok := decoded.(*pyjson.Object); ok {
			if e, _ := data.Get("error"); pyrepr.Truthy(e) {
				return "error", "tool_error", pyrepr.Str(e)
			}
		}
	}
	if failed, suffix := ClassifyToolFailure(toolName, &result); failed {
		return "error", "tool_error", pystr.StripChars(pystr.Strip(suffix), "[]")
	}
	return "ok", "", ""
}

// EmitPostToolCall fires the post tool call observers unless an outer executor owns the event
func (d *Dispatcher) EmitPostToolCall(ctx context.Context, env *Env, ev PostToolCallEvent) {
	if PostToolCallHookSuppressed(ctx) || !d.hasHook("post_tool_call") {
		return
	}
	if ev.Status == "" {
		ev.Status, ev.ErrorType, ev.ErrorMessage = toolResultObserverFields(ev.ToolName, ev.Result)
	}
	d.mu.RLock()
	hooks := append([]PostToolCallHook(nil), d.PostToolCall...)
	d.mu.RUnlock()
	for _, h := range hooks {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					toolsLogger(env).Debug("post_tool_call hook error", "panic", rec)
				}
			}()
			h(ctx, env, ev)
		}()
	}
}

// ApplyTransformToolResult lets plugins replace the final result string, first string wins
func (d *Dispatcher) ApplyTransformToolResult(ctx context.Context, env *Env, ev PostToolCallEvent) string {
	if !d.hasHook("transform_tool_result") {
		return ev.Result
	}
	ev.Status, ev.ErrorType, ev.ErrorMessage = toolResultObserverFields(ev.ToolName, ev.Result)
	d.mu.RLock()
	hooks := append([]TransformToolResultHook(nil), d.TransformToolResult...)
	d.mu.RUnlock()
	result, found := ev.Result, false
	for _, h := range hooks {
		replaced, ok := func() (out string, ok bool) {
			defer func() {
				if rec := recover(); rec != nil {
					toolsLogger(env).Debug("transform_tool_result hook error", "panic", rec)
					out, ok = "", false
				}
			}()
			return h(ctx, env, ev)
		}()
		if ok && !found {
			result, found = replaced, true
		}
	}
	return result
}

// Resolves pre tool call hooks once into a block message and merged modify args
func (d *Dispatcher) dispatchPreToolCallHooks(ctx context.Context, env *Env, name string, args map[string]any, ids ToolHookIDs, trace []map[string]any, opts DispatchOptions) (string, map[string]any) {
	if opts.AllowedTools != nil && !opts.AllowedTools[name] {
		format := opts.DeniedFormat
		if format == "" {
			format = "Tool '{tool_name}' denied"
		}
		return strings.ReplaceAll(format, "{tool_name}", name), nil
	}
	if d.safeWorker() {
		return "", nil
	}
	d.mu.RLock()
	hooks := append([]PreToolCallHook(nil), d.PreToolCall...)
	d.mu.RUnlock()
	if args == nil {
		args = map[string]any{}
	}
	var modified map[string]any
	var block *PreToolDirective
	var approve *PreToolDirective
	for _, h := range hooks {
		directive := func() (out PreToolDirective) {
			defer func() {
				if rec := recover(); rec != nil {
					toolsLogger(env).Warn("pre_tool_call hook panicked", "tool", name, "panic", rec)
					out = PreToolDirective{}
				}
			}()
			return h(ctx, env, name, args, ids, append([]map[string]any(nil), trace...))
		}()
		carriesArgs := directive.Action == "modify" || directive.Action == "approve" || (directive.Action == "block" && directive.Message != "")
		if carriesArgs && len(directive.Args) > 0 {
			base := modified
			if base == nil {
				base = args
			}
			merged := make(map[string]any, len(base)+len(directive.Args))
			for k, v := range base {
				merged[k] = v
			}
			for k, v := range directive.Args {
				merged[k] = v
			}
			modified = merged
		}
		switch directive.Action {
		case "block":
			if directive.Message != "" && block == nil {
				copyD := directive
				block = &copyD
			}
		case "approve":
			if approve == nil {
				copyD := directive
				approve = &copyD
			}
		}
	}
	if block != nil {
		return block.Message, modified
	}
	if approve == nil {
		return "", modified
	}
	ruleKey := approve.RuleKey
	if ruleKey == "" {
		ruleKey = name
	}
	bound := WithObservabilityContext(env, ids.TurnID, ids.ToolCallID, ids.SessionID)
	result := func() (out GuardResult) {
		defer func() {
			if rec := recover(); rec != nil {
				out = GuardResult{Approved: false, Message: fmt.Sprintf("BLOCKED: plugin approval gate failed for %s", name)}
			}
		}()
		return RequestToolApproval(ctx, bound, name, approve.Message, ruleKey)
	}()
	if !result.Approved {
		if result.Message != "" {
			return result.Message, modified
		}
		return fmt.Sprintf("BLOCKED: plugin approval required for %s", name), modified
	}
	return "", modified
}

// ApplyToolRequestMiddleware feeds args through every request middleware, each may replace them
func (d *Dispatcher) ApplyToolRequestMiddleware(ctx context.Context, env *Env, name string, args, originalArgs map[string]any, ids ToolHookIDs) (map[string]any, map[string]any, []map[string]any) {
	if originalArgs == nil {
		originalArgs = args
	}
	original := schemaDeepCopy(originalArgs).(map[string]any)
	current := schemaDeepCopy(args).(map[string]any)
	var trace []map[string]any
	d.mu.RLock()
	chain := append([]ToolRequestMiddleware(nil), d.ToolRequestMiddleware...)
	d.mu.RUnlock()
	if len(chain) == 0 {
		return args, original, trace
	}
	for _, mw := range chain {
		directive := func() (out RequestMiddlewareDirective) {
			defer func() {
				if rec := recover(); rec != nil {
					toolsLogger(env).Warn("tool_request middleware panicked", "tool", name, "panic", rec)
					out = RequestMiddlewareDirective{}
				}
			}()
			return mw(ctx, env, name, schemaDeepCopy(current).(map[string]any), schemaDeepCopy(original).(map[string]any), ids)
		}()
		if directive.Args == nil {
			continue
		}
		current = schemaDeepCopy(directive.Args).(map[string]any)
		if directive.Trace != nil {
			trace = append(trace, directive.Trace...)
			continue
		}
		entry := map[string]any{}
		if directive.Source != "" {
			entry["source"] = directive.Source
		}
		if directive.Reason != "" {
			entry["reason"] = directive.Reason
		}
		if directive.Name != "" {
			entry["name"] = directive.Name
		}
		if len(entry) == 0 {
			entry["source"] = "plugin"
		}
		trace = append(trace, entry)
	}
	return current, original, trace
}

// Marks an error raised below a middleware frame so the frame's own failure handling never swallows it
type downstreamExecutionError struct{ original error }

func (e *downstreamExecutionError) Error() string { return e.original.Error() }

func (e *downstreamExecutionError) Unwrap() error { return e.original }

var errMiddlewareNextTwice = errors.New("tool_execution middleware called next more than once; downstream execution is single-use")

// RunToolExecutionMiddleware runs the terminal call through the execution chain with single use next frames
func (d *Dispatcher) RunToolExecutionMiddleware(ctx context.Context, env *Env, name string, args, originalArgs map[string]any, ids ToolHookIDs, terminal func(map[string]any) (string, error)) (string, error) {
	d.mu.RLock()
	chain := append([]ToolExecutionMiddleware(nil), d.ToolExecutionMiddleware...)
	d.mu.RUnlock()
	if len(chain) == 0 {
		return terminal(args)
	}
	var callAt func(index int, payload map[string]any) (string, error)
	callAt = func(index int, payload map[string]any) (string, error) {
		if index >= len(chain) {
			return terminal(payload)
		}
		frame := chain[index]
		nextCalled, nextSucceeded := false, false
		var nextResult string
		next := func(nextPayload map[string]any) (string, error) {
			if nextCalled {
				return "", errMiddlewareNextTwice
			}
			nextCalled = true
			if nextPayload == nil {
				nextPayload = payload
			}
			out, err := callAt(index+1, nextPayload)
			if err != nil {
				return "", &downstreamExecutionError{err}
			}
			nextResult, nextSucceeded = out, true
			return out, nil
		}
		out, err := func() (out string, err error) {
			defer func() {
				if rec := recover(); rec != nil {
					err = fmt.Errorf("tool_execution middleware panicked: %v", rec)
				}
			}()
			return frame(ctx, env, name, payload, originalArgs, ids, next)
		}()
		if err == nil {
			return out, nil
		}
		var downstream *downstreamExecutionError
		if errors.As(err, &downstream) {
			return "", downstream.original
		}
		if errors.Is(err, errMiddlewareNextTwice) || IsKeyboardInterrupt(ctx, err) {
			return "", err
		}
		toolsLogger(env).Warn("tool_execution middleware failed", "tool", name, "error", err)
		if nextSucceeded {
			return nextResult, nil
		}
		if nextCalled {
			return "", err
		}
		return callAt(index+1, payload)
	}
	return callAt(0, args)
}

// RegistryDispatch runs a registered handler with every panic returned as a JSON error
func (d *Dispatcher) RegistryDispatch(ctx context.Context, call *Call) (result string) {
	if scoped := EnvInTerminalScope(ctx, call.Env); scoped != call.Env {
		bound := *call
		bound.Env = scoped
		call = &bound
	}
	entry := d.Registry.Entry(ctx, call.Env, call.Name)
	if entry == nil {
		return toolErrorJSON("Unknown tool: "+call.Name, nil)
	}
	defer func() {
		if rec := recover(); rec != nil {
			toolsLogger(call.Env).Error(fmt.Sprintf("Tool %s dispatch error: %s", call.Name, boundModelErrorText(call.Env, fmt.Sprint(rec))))
			kind := "panic"
			if err, ok := rec.(error); ok {
				kind = exceptionTypeName(err)
			}
			result = toolErrorJSON(SanitizeToolError(fmt.Sprintf("Tool execution failed: %s: %v", kind, rec)), nil)
		}
	}()
	return NormalizeHandlerResult(call.Env, call.Name, entry.Handler(ctx, call))
}

// Python values of a call's args, each key read from the raw text when that text still reads back as its value
func pythonCallArgs(call *Call) map[string]any {
	return orderedArgsObject(call.RawArgs, call.Args).Vals
}

// Handles a bridge call returning the finished result or the underlying call to re dispatch, tool_call resolved from Python valued args
func (d *Dispatcher) dispatchBridgeTool(ctx context.Context, call *Call, pyArgs map[string]any, enabled, disabled []string) (string, string, map[string]any, bool) {
	if !IsBridgeTool(call.Name) {
		return "", "", nil, false
	}
	env := call.Env
	currentDefs := d.Registry.ToolDefinitions(ctx, env, DefinitionOptions{Enabled: enabled, Disabled: disabled, Quiet: true, SkipToolSearchAssembly: true})
	args := call.Args
	if args == nil {
		args = map[string]any{}
	}
	switch call.Name {
	case ToolSearchName:
		return DispatchToolSearch(ctx, env, args, currentDefs, nil, call), "", nil, true
	case ToolDescribeName:
		return DispatchToolDescribe(ctx, env, args, currentDefs, nil, call), "", nil, true
	}
	if pyArgs == nil {
		pyArgs = map[string]any{}
	}
	underlying, underlyingArgs, errMsg := ResolveUnderlyingCall(ctx, env, pyArgs, SessionToolNames(currentDefs))
	if errMsg != "" || underlying == "" {
		if errMsg == "" {
			errMsg = "tool_call could not be resolved"
		}
		return toolErrorJSON(errMsg, nil), "", nil, true
	}
	if underlying == ConnectorBatchSentinel {
		if !connectionsInScope(currentDefs) {
			return toolErrorJSON("Connectors are not available in this session.", nil), "", nil, true
		}
		return "", underlying, underlyingArgs, true
	}
	if !ScopedDeferrableNames(ctx, env, currentDefs)[underlying] {
		return toolErrorJSON(ScopeBlockReason(ctx, env, underlying), nil), "", nil, true
	}
	if probe := ValidateDeferredCallArgs(ctx, env, underlying, underlyingArgs); probe != "" {
		return probe, "", nil, true
	}
	return "", underlying, underlyingArgs, true
}

// Dispatch routes one tool call through hooks and middleware to the registry and returns its JSON result
func (d *Dispatcher) Dispatch(ctx context.Context, call *Call) string {
	return d.DispatchWith(ctx, call, DispatchOptions{})
}

// DispatchWith is Dispatch with per call options for the executor and the bridge
func (d *Dispatcher) DispatchWith(ctx context.Context, call *Call, opts DispatchOptions) string {
	if ctx == nil {
		ctx = context.Background()
	}
	env := call.Env
	args := CoerceToolArgs(ctx, d.Registry, env, call.Name, call.Args)
	if args == nil {
		args = map[string]any{}
	}
	trace := append([]map[string]any(nil), opts.Trace...)
	name := CanonicalToolName(call.Name)
	ids := hookIDsOf(call)
	start := time.Now()
	enabled, disabled := opts.EnabledToolsets, opts.DisabledToolsets
	if enabled == nil && env != nil {
		enabled = env.Enabled
	}
	if disabled == nil && env != nil {
		disabled = env.Disabled
	}
	suppressed := opts.SuppressPostToolCallHook || PostToolCallHookSuppressed(ctx)
	emit := func(result string, status, errorType, errorMessage string, durationMS int) string {
		if !suppressed {
			d.EmitPostToolCall(ctx, env, PostToolCallEvent{ToolName: name, Args: args, Result: result, IDs: ids, DurationMS: durationMS,
				Status: status, ErrorType: errorType, ErrorMessage: errorMessage, MiddlewareTrace: append([]map[string]any(nil), trace...)})
		}
		return result
	}
	elapsedMS := func() int { return int(time.Since(start).Milliseconds()) }
	resolved := &Call{Name: name, Args: args, RawArgs: call.RawArgs, ToolCallID: call.ToolCallID, TaskID: call.TaskID, SessionID: call.SessionID,
		TurnID: call.TurnID, APIRequestID: call.APIRequestID, UserTask: call.UserTask, Env: env}
	var pyArgs map[string]any
	if name == ToolCallName {
		pyArgs = coercePythonToolArgs(ctx, d.Registry, env, call.Name, pythonCallArgs(call))
	}
	if result, underlying, underlyingArgs, bridged := d.dispatchBridgeTool(ctx, resolved, pyArgs, enabled, disabled); bridged {
		if underlying == "" {
			return emit(result, "", "", "", elapsedMS())
		}
		if underlying == ConnectorBatchSentinel {
			calls, _ := underlyingArgs["calls"].([]any)
			return emit(dispatchConnectorBatch(SuppressPostToolCallHook(ctx, false), d, resolved, calls, opts), "", "", "", elapsedMS())
		}
		inner := *resolved
		inner.Name, inner.Args = underlying, underlyingArgs
		return d.DispatchWith(ctx, &inner, opts)
	}
	if name == "manage_connections" || IsConnectorName(name) {
		selected, _ := d.Registry.SelectToolNames(ctx, env, enabled, disabled, definitionSelectionContext(ctx, env))
		if !selected["manage_connections"] {
			return emit(toolErrorJSON("Connectors are not available in this session.", nil), "", "", "", 0)
		}
		if IsConnectorName(name) && !connectorNameValid(name) {
			return emit(toolErrorJSON("Malformed connector tool name; expected connectors__<connector>__{tool}.", nil), "", "", "", 0)
		}
	}
	originalArgs := make(map[string]any, len(args))
	for k, v := range args {
		originalArgs[k] = v
	}
	if !opts.SkipToolRequestMiddleware {
		args, originalArgs, trace = d.applyRequestMiddleware(ctx, env, name, args, ids, trace)
		resolved.Args = args
	}
	if AgentLoopTools[name] {
		return toolErrorJSON(name+" must be handled by the agent loop", nil)
	}
	var guard *dispatchBlock
	args, guard = d.preDispatchGuards(ctx, env, name, args, ids, trace, opts)
	resolved.Args = args
	if guard != nil {
		return emit(guard.result, "blocked", guard.errorType, guard.errorMessage, 0)
	}
	if !readSearchTools[name] {
		taskID := call.TaskID
		if taskID == "" {
			taskID = "default"
		}
		NotifyOtherToolCall(taskID)
	}
	start = time.Now()
	execCtx := WithDispatcher(ctx, d)
	if suppressed {
		execCtx = SuppressPostToolCallHook(execCtx, true)
	}
	result, err := d.executeTool(execCtx, resolved, originalArgs, ids, opts)
	if err != nil {
		errorMsg := fmt.Sprintf("Error executing %s: %s", name, err.Error())
		toolsLogger(env).Error(errorMsg)
		return emit(toolErrorJSON(SanitizeToolError(errorMsg), nil), "error", exceptionTypeName(err), err.Error(), elapsedMS())
	}
	durationMS := elapsedMS()
	emit(result, "", "", "", durationMS)
	return d.ApplyTransformToolResult(ctx, env, PostToolCallEvent{ToolName: name, Args: args, Result: result, IDs: ids, DurationMS: durationMS})
}

// Result and observer fields of a call refused before dispatch
type dispatchBlock struct {
	result, errorType, errorMessage string
}

// Plugin pre tool call hooks then the ACP edit approval, the args possibly rewritten and the block when one refuses
func (d *Dispatcher) preDispatchGuards(ctx context.Context, env *Env, name string, args map[string]any, ids ToolHookIDs, trace []map[string]any, opts DispatchOptions) (map[string]any, *dispatchBlock) {
	if !opts.SkipPreToolCallHook {
		block, modified := d.dispatchPreToolCallHooks(ctx, env, name, args, ids, trace, opts)
		if modified != nil {
			args = modified
		}
		if block != "" {
			return args, &dispatchBlock{result: toolErrorJSON(block, nil), errorType: "plugin_block", errorMessage: block}
		}
	}
	if d.EditApproval != nil {
		block, err := func() (block string, err error) {
			defer func() {
				if rec := recover(); rec != nil {
					err = fmt.Errorf("edit approval guard panicked: %v", rec)
				}
			}()
			return d.EditApproval(ctx, env, name, args)
		}()
		if err != nil {
			toolsLogger(env).Debug("ACP edit approval guard error", "error", err)
			if name == "write_file" || name == "patch" {
				return args, &dispatchBlock{result: toolErrorJSON("Edit approval denied: approval guard failed", nil), errorType: "edit_approval_error"}
			}
		} else if block != "" {
			return args, &dispatchBlock{result: block, errorType: "edit_approval_denied"}
		}
	}
	return args, nil
}

// Runs the registry handler through execution middleware with the approval observability context bound
func (d *Dispatcher) executeTool(ctx context.Context, call *Call, originalArgs map[string]any, ids ToolHookIDs, opts DispatchOptions) (string, error) {
	env := WithObservabilityContext(call.Env, ids.TurnID, ids.ToolCallID, ids.SessionID)
	bound := *call
	bound.Env = env
	call = &bound
	if call.Name == "execute_code" {
		names := opts.EnabledTools
		if names == nil {
			names = LastResolvedToolNames()
		}
		ctx = WithEnabledTools(ctx, names)
	}
	dispatch := func(nextArgs map[string]any) (string, error) {
		inner := *call
		inner.Args = nextArgs
		if IsConnectorName(call.Name) {
			return dispatchConnectorCall(ctx, env, &inner)
		}
		return d.RegistryDispatch(ctx, &inner), nil
	}
	if opts.SkipToolExecutionMiddleware {
		return dispatch(call.Args)
	}
	return d.RunToolExecutionMiddleware(ctx, env, call.Name, call.Args, originalArgs, ids, dispatch)
}
