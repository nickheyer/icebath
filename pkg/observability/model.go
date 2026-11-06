package observability

import (
	"context"
	"log/slog"
	"maps"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// MetricsAgent is the agent state model and efficiency metrics read, implemented by pkg/agent
type MetricsAgent interface {
	ToolAvailabilityAgent
	Provider() string
	Model() string
	SessionID() string
	ValidToolNames() map[string]bool
	ToolDefinitions() []map[string]any
	MemoryWriteOrigin() string
	PersistDisabled() bool
	MetricsScratch() *sync.Map
}

// Session age after a failed turn within which ending reads as the user giving up on the model
const QuickAbandonNS int64 = 60 * 1_000_000_000

var windowThresholds = []threshold{{32_000, "lt_32k"}, {128_000, "32k_to_128k"}, {256_000, "128k_to_256k"},
	{1_000_000, "256k_to_1m"}}

var (
	limitErrorClasses     = Set("context_overflow", "payload_too_large")
	unattendedEntrypoints = Set("background", "batch", "delegated", "scheduled_task")
)

// Catalog bounded provider and model dimensions of one route
func ModelRoute(provider, model any) map[string]string {
	return ModelCallFields(map[string]any{"provider": provider, "model": model})
}

// Context window bucket, unknown for a missing or zero window
func WindowBucket(contextLength any) string {
	length, ok := nonNegativeNumber(contextLength)
	if !ok || length == 0 {
		return "unknown"
	}
	return bucket(length, windowThresholds, "gte_1m")
}

// Friction row dimensions for a known signal on a route, nil for an unknown signal
func FrictionFields(signal string, route map[string]string) map[string]string {
	if !FrictionSignals.Has(signal) {
		return nil
	}
	if route == nil {
		route = ModelRoute(nil, nil)
	}
	fields := maps.Clone(route)
	fields["signal"] = signal
	return fields
}

// Prompt token count of a usage dict, input tokens when prompt tokens are absent
func promptTokens(usage any) (float64, bool) {
	d, ok := asDict(usage)
	if !ok {
		return 0, false
	}
	if tokens, ok := nonNegativeNumber(d["prompt_tokens"]); ok {
		return tokens, true
	}
	return nonNegativeNumber(d["input_tokens"])
}

// ModelSessionState is the per session model attribution the relay runtime keeps under the session lock
type ModelSessionState struct {
	LastRoute    map[string]string
	PeakRoute    map[string]string
	PeakTokens   float64
	PeakWindow   *float64
	LimitHit     bool
	FailedTurnNS int64
	FailedRoute  map[string]string
}

// Fresh attribution state with no peak seen
func NewModelSessionState() *ModelSessionState { return &ModelSessionState{PeakTokens: -1} }

// Remembers a finished primary call's model and the fullest context seen
func (s *ModelSessionState) ObserveCall(route map[string]string, usage, contextLength any) {
	s.LastRoute = route
	tokens, hasTokens := promptTokens(usage)
	window, hasWindow := nonNegativeNumber(contextLength)
	if s.PeakRoute == nil {
		s.PeakRoute = route
	}
	if !hasTokens || !hasWindow || window == 0 {
		return
	}
	if s.PeakWindow == nil || tokens/window > s.PeakTokens / *s.PeakWindow {
		s.PeakRoute, s.PeakTokens, s.PeakWindow = route, tokens, &window
	}
}

// Remembers a failed primary call's model, a limit error marking the session as hitting its window
func (s *ModelSessionState) ObserveError(route map[string]string, errorClass string) {
	s.LastRoute = route
	if s.PeakRoute == nil {
		s.PeakRoute = route
	}
	if limitErrorClasses.Has(errorClass) {
		s.LimitHit = true
	}
}

// Tracks only the trailing turn for abandonment, a later success clearing it
func (s *ModelSessionState) ObserveTurn(outcome string, route map[string]string, nowNS int64) {
	if outcome == "failed" {
		s.FailedTurnNS = nowNS
		s.FailedRoute = route
		if len(route) == 0 {
			s.FailedRoute = s.LastRoute
		}
		return
	}
	s.FailedTurnNS, s.FailedRoute = 0, nil
}

// Route of a failed trailing turn the session ended soon after, nil otherwise
func (s *ModelSessionState) QuickAbandonRoute(nowNS int64) map[string]string {
	if s.FailedTurnNS != 0 && nowNS-s.FailedTurnNS <= QuickAbandonNS {
		if len(s.FailedRoute) > 0 {
			return s.FailedRoute
		}
		return s.LastRoute
	}
	return nil
}

// Context peak row of a session that reached a primary model, nil when no call was made
func (s *ModelSessionState) ContextPeakFields() map[string]string {
	if s.PeakRoute == nil {
		return nil
	}
	fields := maps.Clone(s.PeakRoute)
	fields["limit_hit"] = "no"
	if s.LimitHit {
		fields["limit_hit"] = "yes"
	}
	fields["peak_fill_bucket"], fields["window_bucket"] = "unknown", "unknown"
	if s.PeakWindow != nil {
		fields["peak_fill_bucket"] = ContextFillBucket(s.PeakTokens, *s.PeakWindow)
		fields["window_bucket"] = WindowBucket(*s.PeakWindow)
	}
	return fields
}

// Reports whether a task started from a surface a user watches
func Attended(startFields map[string]string) bool {
	return len(startFields) > 0 && !unattendedEntrypoints.Has(startFields["entrypoint"])
}

// Required parameter names of each named tool definition
func requiredParams(defs []map[string]any) map[string][]string {
	required := map[string][]string{}
	for _, tool := range defs {
		fn, ok := asDict(tool["function"])
		if !ok {
			continue
		}
		name, ok := fn["name"].(string)
		if !ok {
			continue
		}
		var names []string
		var list []any
		if params, ok := asDict(fn["parameters"]); ok {
			list, _ = params["required"].([]any)
			if l, ok := params["required"].([]string); ok {
				for _, n := range l {
					list = append(list, n)
				}
			}
		}
		for _, n := range list {
			if s, ok := n.(string); ok {
				names = append(names, s)
			}
		}
		required[name] = names
	}
	return required
}

// First problem of one emitted call, most severe first, none when clean
func ToolCallIssue(name, rawArgs any, validNames map[string]bool, required map[string][]string, repaired bool) string {
	clean := "none"
	if repaired {
		clean = "repaired"
	}
	toolName, isName := name.(string)
	if !isName || !validNames[toolName] {
		return "unknown_tool"
	}
	needed := required[toolName]
	var parsed any
	switch raw := rawArgs.(type) {
	case map[string]any, *pyjson.Object, []any:
		parsed = raw
	default:
		if rawArgs == nil || pystr.Strip(pyrepr.Str(rawArgs)) == "" {
			if len(needed) > 0 {
				return "empty_arguments"
			}
			return clean
		}
		value, err := pyjson.Loads(pyrepr.Str(rawArgs))
		if err != nil {
			return "invalid_json"
		}
		parsed = value
	}
	d, ok := asDict(parsed)
	if !ok {
		return "schema_mismatch"
	}
	for _, key := range needed {
		if _, has := d[key]; !has {
			return "schema_mismatch"
		}
	}
	return clean
}

// EmittedToolCall is one tool call a primary response emitted, before icebath normalizes it
type EmittedToolCall struct {
	Name, Arguments any
	NameRepaired    bool
	ArgsRepaired    bool
}

// Agent provider and model, nil for a missing agent like getattr with a None default
func agentProviderModel(agent MetricsAgent) (any, any) {
	if agent == nil {
		return nil, nil
	}
	return agent.Provider(), agent.Model()
}

// Counts every tool call one primary response emitted with its quality issue, nothing unless enabled
func RecordToolCallQuality(ctx context.Context, agent MetricsAgent, toolCalls []EmittedToolCall) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Shared-metrics tool-call quality not recorded", "error", r)
		}
	}()
	if len(toolCalls) == 0 || !relayEnabled(ctx) {
		return
	}
	route := ModelRoute(agentProviderModel(agent))
	var validNames map[string]bool
	var defs []map[string]any
	if agent != nil {
		validNames, defs = agent.ValidToolNames(), agent.ToolDefinitions()
	}
	required := requiredParams(defs)
	calls := make([]ToolCallFunction, len(toolCalls))
	for i, tc := range toolCalls {
		calls[i] = ToolCallFunction{Name: tc.Name, Arguments: tc.Arguments}
	}
	if batch := ToolBatchFields(route, calls); batch != nil {
		recordProcessMark(ctx, ToolBatchMark, batch)
	}
	for _, tc := range toolCalls {
		issue := ToolCallIssue(tc.Name, tc.Arguments, validNames, required, tc.NameRepaired || tc.ArgsRepaired)
		row := maps.Clone(route)
		row["call_role"], row["issue"] = "primary", issue
		recordProcessMark(ctx, ModelToolQualityMark, row)
		if unavailable := ToolUnavailableFields(agent, tc.Name, issue, route); unavailable != nil {
			recordProcessMark(ctx, ToolUnavailableMark, unavailable)
		}
	}
}

// Counts the calls of one validated response, a call whose name or streamed argument JSON was repaired reads repaired
func RecordValidatedToolCalls(ctx context.Context, agent MetricsAgent, toolCalls []state.ToolCall, repaired map[int]bool) {
	calls := make([]EmittedToolCall, len(toolCalls))
	for i, tc := range toolCalls {
		argsRepaired, _ := tc.Function.Extra["args_repaired"].(bool)
		calls[i] = EmittedToolCall{Name: tc.Function.Name, Arguments: tc.Function.Arguments, NameRepaired: repaired[i], ArgsRepaired: argsRepaired}
	}
	RecordToolCallQuality(ctx, agent, calls)
}

// FrictionOptions names the session and model one friction action counts against
type FrictionOptions struct {
	SessionID       any
	Agent           MetricsAgent
	Provider, Model any
	HomeDir         string
	Turns           int
}

// Counts one user friction action against the model of the session's last turn, zero turns reading one
func RecordModelFriction(ctx context.Context, signal string, opts FrictionOptions) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Shared-metrics friction not recorded", "signal", signal, "error", r)
		}
	}()
	if opts.HomeDir != "" {
		ctx = home.WithOverride(ctx, opts.HomeDir)
	}
	if !FrictionSignals.Has(signal) || !relayEnabled(ctx) {
		return
	}
	provider, model, sessionID := opts.Provider, opts.Model, opts.SessionID
	if opts.Agent != nil {
		provider, model = opts.Agent.Provider(), opts.Agent.Model()
		if id := opts.Agent.SessionID(); id != "" {
			sessionID = id
		}
	}
	turns := opts.Turns
	if turns == 0 {
		turns = 1
	}
	RecordSessionFriction(ctx, signal, pyText(sessionID), ModelRoute(provider, model), turns)
}
