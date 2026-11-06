package agent

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Replays the newest paired todo tool result into the todo store when its revision is newer
func (a *AIAgent) hydrateTodoStore(ctx context.Context, history []map[string]any) {
	if todos, revision, found := a.latestTodoResponse(history); found {
		historyRevision, ok := pyIntCoerce(pyOr(revision, 0))
		if !ok {
			historyRevision = 1
		}
		historyRevision = max(0, historyRevision)
		current, _ := pyIntCoerce(pyOr(a.todoStore.Snapshot()["revision"], 0))
		if historyRevision > current {
			a.todoStore.Restore(todos, historyRevision)
			if !a.quietMode {
				a.vprint(fmt.Sprintf("%s📋 Restored %d todo item(s) from history", a.logPrefix, len(todos)), false, false)
			}
		}
	}
	tools.SetInterrupt(tools.WorkerOf(ctx), false, "")
}

// Python x or y over JSON values
func pyOr(v, def any) any {
	if pyTruthy(v) {
		return v
	}
	return def
}

// Newest paired size bounded todo result as its todos and revision
func (a *AIAgent) latestTodoResponse(history []map[string]any) ([]any, any, bool) {
	for idx := len(history) - 1; idx >= 0; idx-- {
		msg := history[idx]
		content, isStr := msg["content"].(string)
		if _, has := msg["content"]; !has {
			content, isStr = "", true
		}
		if msg["role"] != "tool" || !isStr || !toolResponseMatchesTodoCall(history, idx) {
			continue
		}
		if utf8.RuneCountInString(content) > tools.MaxTodoResultChars {
			sid := a.sessionID
			if sid == "" {
				sid = "none"
			}
			runAgentLog().Warn(fmt.Sprintf("Skipping oversized todo tool response during hydration: session=%s chars=%d", sid, utf8.RuneCountInString(content)))
			continue
		}
		if !strings.Contains(content, `"todos"`) {
			continue
		}
		data, err := pyjson.Loads(content)
		if err != nil {
			continue
		}
		switch d := data.(type) {
		case *pyjson.Object:
			if v, ok := d.Get("todos"); ok {
				if list, ok := v.([]any); ok {
					rev, has := d.Get("revision")
					if !has {
						rev = 1
					}
					return list, rev, true
				}
			}
		case []any:
			for _, item := range d {
				if s, ok := item.(string); ok && s == "todos" {
					panic(fmt.Errorf("TypeError: list indices must be integers or slices, not str"))
				}
			}
		case string:
			if strings.Contains(d, "todos") {
				panic(fmt.Errorf("TypeError: string indices must be integers, not 'str'"))
			}
		}
	}
	return nil, nil, false
}

// True when the nearest prior assistant message issued a todo tool call with this tool_call_id
func toolResponseMatchesTodoCall(history []map[string]any, toolIndex int) bool {
	if toolIndex < 0 || toolIndex >= len(history) {
		return false
	}
	toolCallID, _ := history[toolIndex]["tool_call_id"].(string)
	if toolCallID == "" {
		return false
	}
	for i := toolIndex - 1; i >= 0; i-- {
		switch history[i]["role"] {
		case "assistant":
			return assistantHasTodoToolCall(history[i], toolCallID)
		case "user", "system":
			return false
		}
	}
	return false
}

// True when the paired call resolves to the registered todo tool
func assistantHasTodoToolCall(assistantMsg map[string]any, toolCallID string) bool {
	calls, ok := assistantMsg["tool_calls"].([]any)
	if !ok {
		return false
	}
	for _, tc := range calls {
		if state.CoalesceToolCallID(tc) == toolCallID && tools.IsTodoToolCall(getToolCallNameStatic(tc), toolCallArguments(tc)) {
			return true
		}
	}
	return false
}

// Raw arguments of a tool call entry in dict or struct form
func toolCallArguments(tc any) any {
	switch t := tc.(type) {
	case map[string]any:
		if fn, ok := t["function"].(map[string]any); ok {
			return fn["arguments"]
		}
	case state.ToolCall:
		return t.Function.Arguments
	case *state.ToolCall:
		if t != nil {
			return t.Function.Arguments
		}
	}
	return nil
}

// Function name of a tool call entry in dict or struct form
func getToolCallNameStatic(tc any) string {
	switch t := tc.(type) {
	case map[string]any:
		if fn, ok := t["function"].(map[string]any); ok {
			name, _ := fn["name"].(string)
			return name
		}
		return ""
	case state.ToolCall:
		return t.Function.Name
	case *state.ToolCall:
		if t != nil {
			return t.Function.Name
		}
	}
	return ""
}

// True for an assistant turn whose only payload is reasoning with no text and no tool calls
func isThinkingOnlyAssistant(msg map[string]any, dropCodexReasoningItems bool) bool {
	if msg == nil || msg["role"] != "assistant" || pyTruthy(msg["tool_calls"]) {
		return false
	}
	if pyTruthy(msg["_thinking_prefill"]) {
		return true
	}
	if contentHasRealPayload(msg["content"]) {
		return false
	}
	if provider.HasCompactionCheckpoint(dictItems(msg["codex_reasoning_items"])) {
		return false
	}
	reasoning := pyOr(msg["reasoning_content"], msg["reasoning"])
	if s, ok := reasoning.(string); ok && strings.TrimSpace(s) != "" {
		return true
	}
	if rd, ok := msg["reasoning_details"].([]any); ok && len(rd) > 0 {
		return true
	}
	if items, ok := msg["codex_reasoning_items"].([]any); ok && dropCodexReasoningItems {
		for _, item := range items {
			if d, ok := item.(map[string]any); ok && d["type"] == "reasoning" {
				return true
			}
		}
	}
	return false
}

// True when assistant content carries anything beyond thinking blocks or whitespace
func contentHasRealPayload(content any) bool {
	switch c := content.(type) {
	case string:
		return strings.TrimSpace(c) != ""
	case []any:
		for _, block := range c {
			b, ok := block.(map[string]any)
			if !ok {
				if pyTruthy(block) {
					return true
				}
				continue
			}
			switch b["type"] {
			case "text":
				if text, ok := b["text"].(string); ok && strings.TrimSpace(text) != "" {
					return true
				}
			case "thinking", "redacted_thinking":
			default:
				return true
			}
		}
		return false
	}
	return content != nil
}

// Caps delegate_task calls in one turn at the concurrent children limit, other calls all kept
func capDelegateTaskCalls(ctx context.Context, toolCalls []state.ToolCall) []state.ToolCall {
	maxChildren := maxConcurrentChildren(ctx)
	delegateCount := 0
	for _, tc := range toolCalls {
		if tc.Function.Name == "delegate_task" {
			delegateCount++
		}
	}
	if delegateCount <= maxChildren {
		return toolCalls
	}
	kept := 0
	truncated := make([]state.ToolCall, 0, len(toolCalls))
	for _, tc := range toolCalls {
		if tc.Function.Name == "delegate_task" {
			if kept >= maxChildren {
				continue
			}
			kept++
		}
		truncated = append(truncated, tc)
	}
	runAgentLog().Warn(fmt.Sprintf("Truncated %d excess delegate_task call(s) to enforce max_concurrent_children=%d limit", delegateCount-maxChildren, maxChildren))
	return truncated
}

// Drops duplicate name and canonical argument pairs in one turn, first occurrence kept
func deduplicateToolCalls(toolCalls []state.ToolCall) []state.ToolCall {
	type key struct{ name, args string }
	seen := map[key]bool{}
	unique := make([]state.ToolCall, 0, len(toolCalls))
	for _, tc := range toolCalls {
		args := tc.Function.Arguments
		if v, err := pyjson.Loads(args); err == nil {
			if canon, err := pyjson.Dumps(v, pyjson.Separators(",", ":"), pyjson.SortKeys(true)); err == nil {
				args = canon
			}
		}
		k := key{tc.Function.Name, args}
		if seen[k] {
			runAgentLog().Warn("Removed duplicate tool call: " + tc.Function.Name)
			continue
		}
		seen[k] = true
		unique = append(unique, tc)
	}
	if len(unique) < len(toolCalls) {
		return unique
	}
	return toolCalls
}

// True when a fallback provider remains in the chain
func (a *AIAgent) hasPendingFallback() bool { return a.fallbackIndex < len(a.fallbackChain) }

// Records the first guardrail decision that should stop the turn and counts every halt
func (a *AIAgent) setToolGuardrailHalt(d tools.ToolGuardrailDecision) {
	if d.ShouldHalt() && a.toolGuardrailHaltDecision == nil {
		dc := d
		a.toolGuardrailHaltDecision = &dc
	}
	if d.ShouldHalt() {
		recordGuardrailDecision(a, d.Action, d.Code)
	}
}

// Final response text for a turn the tool guardrail halted
func toolguardControlledHaltResponse(d tools.ToolGuardrailDecision) string {
	name := d.ToolName
	if name == "" {
		name = "the same tool"
	}
	return fmt.Sprintf("I stopped retrying because I kept running %s %d times without making progress. The last result above shows what "+
		"blocked it. Tell me how you'd like to proceed, or send `continue` and I'll try a "+
		"different approach.", name, d.Count)
}

// Guardrails returns the per turn tool call guardrail controller
func (a *AIAgent) Guardrails() *tools.ToolCallGuardrailController { return a.toolGuardrails }

// Folds guardrail and stall guard observations into a tool result and records any halt
func (a *AIAgent) AppendGuardrailObservation(name string, args map[string]any, result string, failed bool, toolCallID string) string {
	res, fail := result, failed
	decision := a.toolGuardrails.AfterCall(name, args, &res, &fail)
	var stallNotice, resultStub, stallKind string
	if a.stallGuardsEnabled() {
		func() {
			defer func() {
				if r := recover(); r != nil {
					runAgentLog().Debug(fmt.Sprintf("stall-guard identical-call observation failed: %v", r))
				}
			}()
			obs := a.toolGuardrails.ObserveCall(name, args, result, toolCallID, failed)
			stallNotice, resultStub, stallKind = obs.Notice, obs.Stub, obs.Kind
		}()
	}
	if resultStub != "" {
		result = resultStub
	}
	if decision.Action == "warn" || decision.Action == "halt" {
		result = tools.AppendToolguardGuidance(result, decision)
	}
	kind := ""
	if stallNotice != "" {
		kind = stallKind
	}
	recordGuardrailWarnings(a, decision, kind)
	if decision.ShouldHalt() {
		a.setToolGuardrailHalt(decision)
	} else if streak := a.toolGuardrails.HaltDecision(); streak != nil && (streak.Code == "identical_call_streak_halt" || streak.Code == "identical_cycle_halt") {
		result = tools.AppendToolguardGuidance(result, *streak)
		a.setToolGuardrailHalt(*streak)
	}
	if stallNotice != "" {
		result = result + "\n\n" + stallNotice
	}
	return result
}

// Config gate for the runtime anti stall guards
func (a *AIAgent) stallGuardsEnabled() bool { return a.stallGuards }

// Records the halt and returns the synthetic tool result for a blocked call
func (a *AIAgent) GuardrailBlockResult(d tools.ToolGuardrailDecision) string {
	a.setToolGuardrailHalt(d)
	return tools.ToolguardSyntheticResult(d)
}

// Runs the assistant tool calls through the segment planner and appends their results to messages
func (a *AIAgent) executeToolCalls(ctx context.Context, assistantMessage *state.Message, messages *[]*state.Message, effectiveTaskID string) {
	toolCalls := assistantMessage.ToolCalls
	a.executingTools = true
	func() {
		defer func() { a.executingTools = false }()
		ctx := tools.WithConnectionSurface(ctx, agentConnectionSurface(a))
		exec := a.toolExecutor()
		if len(toolCalls) <= 1 {
			exec.ExecuteToolCallsSequential(ctx, toolCalls, messages, effectiveTaskID, true)
			return
		}
		execCwd := ""
		if env, err := tools.GetActiveEnv(a.env, effectiveTaskID); err == nil && env != nil {
			execCwd = env.Cwd()
		}
		segments := tools.PlanToolBatchSegments(ctx, a.env, toolCalls, execCwd)
		if len(segments) == 1 {
			if segments[0].Mode == "parallel" {
				exec.ExecuteToolCallsConcurrent(ctx, toolCalls, messages, effectiveTaskID, true)
			} else {
				exec.ExecuteToolCallsSequential(ctx, toolCalls, messages, effectiveTaskID, true)
			}
			return
		}
		exec.ExecuteToolCallsSegmented(ctx, toolCalls, messages, effectiveTaskID, segments)
	}()
	if a.trimAfterToolBatch {
		a.trimAfterToolBatch = false
		memory.TrimMemory(ctx, a.env, memory.TrimOptions{Reason: "large tool result"})
	}
}

// Single delegate_task dispatch site mapping the schema fields onto the delegate tool keywords
func (a *AIAgent) DispatchDelegateTask(ctx context.Context, args map[string]any) (string, error) {
	return delegateTask(ctx, a, map[string]any{
		"goal": args["goal"], "context": args["context"], "tasks": stripModelHiddenTaskFields(args["tasks"]),
		"max_iterations": args["max_iterations"], "role": args["role"], "background": !(a.delegateDepth > 0),
		"images": args["images"], "action": args["action"], "subagent_id": args["subagent_id"], "message": args["message"],
	})
}

// Word wraps verbose tool output to the terminal width with continuation lines indented
func wrapVerbose(label, text, indent string) string {
	cols, _ := pyshutil.GetTerminalSize(120, 24)
	wrapWidth := max(40, cols-utf8.RuneCountInString(indent))
	var out []string
	for _, raw := range strings.Split(text, "\n") {
		if utf8.RuneCountInString(raw) <= wrapWidth {
			out = append(out, raw)
			continue
		}
		wrapped := pystr.TextwrapWrapNoHyphenBreak(raw, wrapWidth, true)
		if len(wrapped) == 0 {
			wrapped = []string{raw}
		}
		out = append(out, wrapped...)
	}
	return indent + label + strings.Join(out, "\n"+indent)
}

// WrapVerbose wraps verbose tool output with the default five space indent
func (a *AIAgent) WrapVerbose(prefix, text string) string { return wrapVerbose(prefix, text, "     ") }
