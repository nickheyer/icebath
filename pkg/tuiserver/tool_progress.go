package tuiserver

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Session event emitter of the callback sinks, swapped where a case records the emitted events
var emitEvent = tuigateway.Emit

// Guards the per session tool slots parallel tool callbacks share
var toolSlotsMu sync.Mutex

// Per call slot map of a session, created on first use, the caller holding toolSlotsMu
func toolSlot[V any](session *tuigateway.Session, key string) map[string]V {
	slot, ok := session.Get(key).(map[string]V)
	if !ok || slot == nil {
		slot = map[string]V{}
		session.Set(key, slot)
	}
	return slot
}

// Python round half to even of a float
func pyRound(x float64) float64 { return math.RoundToEven(x) }

// Short human duration of a tool run, empty when unknown
func fmtToolDuration(seconds *float64) string {
	if seconds == nil {
		return ""
	}
	s := *seconds
	if s < 10 {
		return fmt.Sprintf("%.1fs", s)
	}
	if s < 60 {
		return fmt.Sprintf("%.0fs", pyRound(s))
	}
	total := int64(pyRound(s))
	mins, secs := total/60, total%60
	if secs != 0 {
		return fmt.Sprintf("%dm %ds", mins, secs)
	}
	return fmt.Sprintf("%dm", mins)
}

// Length of the list at a key path of decoded JSON, false when any step is not a dict or the leaf no list
func countList(obj any, path ...string) (int, bool) {
	cur := obj
	for _, key := range path {
		dict, ok := cur.(*pyjson.Object)
		if !ok {
			return 0, false
		}
		cur = dict.GetOr(key, nil)
	}
	list, ok := cur.([]any)
	return len(list), ok
}

// Result counter, verb and nouns of a tool's completion summary line
type summaryCounter struct {
	count            func(data *pyjson.Object) (int, bool)
	verb, one, other string
}

// Tools whose completion line counts something in the result
var summaryCounters = map[string]summaryCounter{
	"web_search": {func(d *pyjson.Object) (int, bool) { return countList(d, "data", "web") }, "Did", "search", "searches"},
	"web_extract": {func(d *pyjson.Object) (int, bool) {
		if n, ok := countList(d, "results"); ok && n != 0 {
			return n, true
		}
		return countList(d, "data", "results")
	}, "Extracted", "page", "pages"},
}

// One line completion summary of a tool result, empty when the result has nothing to summarize
func toolSummary(name, result string, durationS *float64) string {
	decoded, err := pyjson.Loads(result)
	if err != nil {
		return ""
	}
	data, ok := decoded.(*pyjson.Object)
	if !ok {
		return ""
	}
	suffix := ""
	if dur := fmtToolDuration(durationS); dur != "" {
		suffix = " in " + dur
	}
	if warning := strings.TrimSpace(strOr(data.GetOr("fallback_warning", nil))); warning != "" {
		return warning + suffix
	}
	entry, known := summaryCounters[name]
	if !known {
		return ""
	}
	n, counted := entry.count(data)
	if !counted {
		return ""
	}
	noun := entry.other
	if n == 1 {
		noun = entry.one
	}
	return fmt.Sprintf("%s %d %s%s", entry.verb, n, noun, suffix)
}

// Revision of a normalized todo snapshot
func todoRevision(state *pyjson.Object) int64 {
	revision, _ := pyint.Int64(state.GetOr("revision", int64(0)))
	return revision
}

// Keeps the newest todo snapshot on the session, revision monotonic
func cacheTodoState(session *tuigateway.Session, state *pyjson.Object) {
	if state == nil {
		return
	}
	cached := tuigateway.NormalizeTodoState(session.Get("todo_state"))
	if cached == nil || todoRevision(state) >= todoRevision(cached) {
		session.Set("todo_state", state)
	}
}

// Newest live or cached todo snapshot of a runtime session
func sessionTodoState(session *tuigateway.Session) *pyjson.Object {
	cached := tuigateway.NormalizeTodoState(session.Get("todo_state"))
	var live *pyjson.Object
	if a := sessionAgent(session); a != nil {
		if store := a.TodoStore(); store != nil {
			func() {
				defer func() {
					if failure := recover(); failure != nil {
						logger().Debug("failed to read live todo state", "exc_info", failureText(failure))
					}
				}()
				snapshot, _ := pyjson.AsObject(store.Snapshot())
				live = tuigateway.NormalizeTodoState(snapshot)
			}()
		}
	}
	if live != nil && (cached == nil || todoRevision(live) >= todoRevision(cached)) {
		cached = live
	}
	if cached != nil {
		session.Set("todo_state", cached)
	}
	return cached
}

// Attaches the authoritative todo snapshot to a session response
func attachTodoState(payload map[string]any, session *tuigateway.Session) map[string]any {
	if state := sessionTodoState(session); state != nil {
		payload["todo_state"] = state
	}
	return payload
}

// Cold resume todo snapshot of a stored session without the page or history limits
func todoStateFromDB(ctx context.Context, db todoResultReader, sessionID string) (state *pyjson.Object) {
	if db == nil {
		return nil
	}
	defer func() {
		if failure := recover(); failure != nil {
			logger().Debug("failed to read persisted todo state", "exc_info", failureText(failure))
			state = nil
		}
	}()
	content, found, err := db.GetLatestTodoResult(ctx, sessionID, nil)
	if err != nil {
		panic(err)
	}
	if !found || len(content) > tools.MaxTodoResultChars {
		return nil
	}
	decoded, err := pyjson.Loads(content)
	if err != nil {
		return nil
	}
	return tuigateway.NormalizeTodoState(decoded)
}

// Store that serves the newest todo tool result of a session
type todoResultReader interface {
	GetLatestTodoResult(ctx context.Context, sessionID string, maxChars *int) (string, bool, error)
}

// Whether a failed tool call must reach the user whatever the display policy
func toolResultNeedsUser(result any) bool {
	text, ok := result.(string)
	if !ok || text == "" {
		return false
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return false
	}
	data, ok := decoded.(*pyjson.Object)
	if !ok {
		return false
	}
	if data.GetOr("success", nil) == false || data.GetOr("ok", nil) == false {
		return true
	}
	if message, ok := data.GetOr("error", nil).(string); ok && strings.TrimSpace(message) != "" {
		return true
	}
	if _, isBool := data.GetOr("exit_code", nil).(bool); isBool {
		return false
	}
	code, isInt := pyrepr.Int(data.GetOr("exit_code", nil))
	return isInt && code.Sign() != 0
}

// Tool environment of the context profile for labels and previews, defaults when its config cannot load
func toolEnv(ctx context.Context) *tools.Env {
	tree, _ := loadConfig(ctx)
	return &tools.Env{Home: home.Dir(ctx), Config: tree, TerminalScope: tools.GetTerminalScope(ctx)}
}

// Friendly labels of a tool call as payloads, nil when it has none
func toolLabels(ctx context.Context, name string, args map[string]any) []any {
	labels := tools.ToolLabelsForCall(toolEnv(ctx), name, args)
	if len(labels) == 0 {
		return nil
	}
	out := make([]any, len(labels))
	for i, label := range labels {
		out[i] = label.AsPayload()
	}
	return out
}

// Whether a call drives a connector, directly or through a bridged tool_call batch
func connectorToolLifecycle(name string, args map[string]any) bool {
	if name == "manage_connections" || tools.IsConnectorName(name) {
		return true
	}
	if name != "tool_call" || args == nil {
		return false
	}
	calls := []any{args}
	if list, ok := args["calls"].([]any); ok {
		calls = list
	}
	for _, raw := range calls {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		callName, _ := call["name"].(string)
		if callName == "manage_connections" || tools.IsConnectorName(callName) {
			return true
		}
	}
	return false
}

// Whether a connector lifecycle event belongs to a turn whose record was replaced or finalized
func connectorLifecycleIsStale(ctx context.Context, sid, name string, args map[string]any) bool {
	if !connectorToolLifecycle(name, args) {
		return false
	}
	owner := currentRuntimeSessionRecord(ctx)
	return owner != nil && (tuigateway.SessionRecord(sid) != owner || truthy(owner.Get("_finalized")))
}

// Emits a tool lifecycle event, a connector one projected and written to the owner's transport
func emitToolLifecycle(ctx context.Context, event, sid, name string, args map[string]any, payload map[string]any) {
	if !connectorToolLifecycle(name, args) {
		emitEvent(ctx, event, sid, payload)
		return
	}
	projected, _ := tuigateway.ConnectorUIPayload(payload).(map[string]any)
	sessionsLock.Lock()
	if connectorLifecycleIsStale(ctx, sid, name, args) {
		sessionsLock.Unlock()
		return
	}
	transport, _ := tuigateway.SessionRecord(sid).Get("transport").(tuigateway.Transport)
	if transport == nil {
		if transport = tuigateway.CurrentTransport(ctx); transport == nil {
			transport = tuigateway.ModuleStdioTransport()
		}
	}
	sessionsLock.Unlock()
	frame := tuigateway.EventFrame(event, sid, projected)
	tuigateway.StampEvent(frame)
	tuigateway.ProjectRoomMemberActivity(ctx, frame, tuigateway.SessionRecord)
	transport.Write(frame)
}

// Redacted and capped pretty JSON of tool arguments for the verbose trail
func toolArgsText(args map[string]any) string {
	var value any = map[string]any{}
	if len(args) > 0 {
		value = args
	}
	text, err := pyjson.Dumps(value, pyjson.Indent(2), pyjson.EnsureASCII(false),
		pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil }))
	if err != nil {
		text = pyrepr.Str(value)
	}
	return tuigateway.RedactVerboseText(text)
}

// Records the tool start, snapshots a local edit and emits tool.start when the session shows it
func onToolStart(ctx context.Context, sid, toolCallID, name string, args map[string]any) {
	if connectorLifecycleIsStale(ctx, sid, name, args) {
		return
	}
	if session := tuigateway.SessionRecord(sid); session != nil {
		suppress(func() {
			taskID := strOr(session.Get("session_key"))
			if taskID == "" {
				if a := sessionAgent(session); a != nil && a.SessionID() != "" {
					taskID = a.SessionID()
				} else {
					taskID = sid
				}
			}
			if snapshot := tools.CaptureLocalEditSnapshot(toolEnv(ctx), name, args, &taskID); snapshot != nil {
				toolSlotsMu.Lock()
				toolSlot[*tools.LocalEditSnapshot](session, "edit_snapshots")[toolCallID] = snapshot
				toolSlotsMu.Unlock()
			}
		})
		toolSlotsMu.Lock()
		toolSlot[float64](session, "tool_started_at")[toolCallID] = nowSeconds()
		delete(toolSlot[map[string]any](session, "tool_result_metadata"), toolCallID)
		toolSlotsMu.Unlock()
	}
	if toolProgressEnabled(sid) || toolLifecycleRequiredForUI(name) || connectorToolLifecycle(name, args) {
		payload := map[string]any{"tool_id": toolCallID, "name": name, "context": tuigateway.ToolCtx(name, args)}
		if labels := toolLabels(ctx, name, args); labels != nil {
			payload["labels"] = labels
		}
		if len(args) > 0 {
			payload["args"] = args
		}
		if sessionVerbose(sid) {
			if text := toolArgsText(args); text != "" {
				payload["args_text"] = text
			}
		}
		emitToolLifecycle(ctx, "tool.start", sid, name, args, payload)
	}
}

// Prepares the inline diff preview of a tool result without emitting, kept on the session for the completion event
func prepareToolResultMetadata(ctx context.Context, sid, toolCallID, name string, args map[string]any, result string) map[string]any {
	session := tuigateway.SessionRecord(sid)
	var snapshot *tools.LocalEditSnapshot
	if session != nil {
		toolSlotsMu.Lock()
		snapshots := toolSlot[*tools.LocalEditSnapshot](session, "edit_snapshots")
		snapshot = snapshots[toolCallID]
		delete(snapshots, toolCallID)
		toolSlotsMu.Unlock()
	}
	metadata := map[string]any{}
	suppress(func() {
		rendered := []string{}
		if tools.RenderEditDiffWithDelta(ctx, name, result, args, snapshot, func(line string) { rendered = append(rendered, line) }) {
			metadata["inline_diff"] = strings.Join(rendered, "\n")
		}
	})
	if session != nil {
		toolSlotsMu.Lock()
		toolSlot[map[string]any](session, "tool_result_metadata")[toolCallID] = metadata
		toolSlotsMu.Unlock()
	}
	if len(metadata) == 0 {
		return map[string]any{}
	}
	return map[string]any{"tool_result_metadata": metadata}
}

// Emits live MCP progress on a running tool row when the session shows tool progress
func progressToolProgress(ctx context.Context, sid string, name, preview any, kw map[string]any) {
	if toolProgressEnabled(sid) {
		emitEvent(ctx, "tool.progress", sid, tools.ToolProgressPayload(strOr(name), strOr(preview), kw))
	}
}

// Emits the risk badge of a tool row when the session shows tool progress
func progressOutputRisk(ctx context.Context, sid string, name, preview any, kw map[string]any) {
	metadata, ok := kw["risk_metadata"].(map[string]any)
	if !ok || !toolProgressEnabled(sid) {
		return
	}
	risk := "low"
	if value := metadata["risk"]; truthy(value) {
		risk = pyrepr.Str(value)
	}
	findings := []any{}
	if list, ok := metadata["findings"].([]any); ok {
		for _, item := range list {
			findings = append(findings, pyrepr.Str(item))
		}
	}
	emitEvent(ctx, "tool.output_risk", sid, map[string]any{"tool_id": strOr(kw["tool_call_id"]), "name": pyrepr.Str(name),
		"risk": risk, "findings": findings, "redacted": truthy(metadata["redacted"])})
}

// Emits reasoning.available when the session shows reasoning
func progressReasoning(ctx context.Context, sid string, name, preview any, kw map[string]any) {
	if !sessionShowReasoning(ctx, sid) {
		return
	}
	payload := map[string]any{"text": pyrepr.Str(preview)}
	if sessionVerbose(sid) {
		payload["verbose"] = true
	}
	emitEvent(ctx, "reasoning.available", sid, payload)
}

// Emits one mixture of agents reference output when the session shows reasoning
func progressMoaReference(ctx context.Context, sid string, name, preview any, kw map[string]any) {
	if !sessionShowReasoning(ctx, sid) {
		return
	}
	text := ""
	if truthy(preview) {
		text = pyrepr.Str(preview)
	}
	payload := map[string]any{"label": pyrepr.Str(name), "text": text}
	for _, pair := range [][2]string{{"moa_index", "index"}, {"moa_count", "count"}} {
		if value := kw[pair[0]]; value != nil {
			payload[pair[1]] = value
		}
	}
	emitEvent(ctx, "moa.reference", sid, payload)
}

// Python int of a progress counter, panicking like int() on junk
func progressInt(v any) int64 {
	n, err := pyint.Int64(v)
	if err != nil {
		panic(err)
	}
	return n
}

// Emits the reference completion counters of a mixture of agents turn
func progressMoaProgress(ctx context.Context, sid string, name, preview any, kw map[string]any) {
	done, total := kw["moa_refs_done"], kw["moa_refs_total"]
	if !sessionShowReasoning(ctx, sid) || done == nil || total == nil {
		return
	}
	emitEvent(ctx, "moa.progress", sid, map[string]any{"label": strOr(name), "refs_done": progressInt(done), "refs_total": progressInt(total)})
}

// Emits the mixture of agents phase transition with its counters and aggregator
func progressMoaPhase(ctx context.Context, sid string, name, preview any, kw map[string]any) {
	phase := kw["moa_phase"]
	if !truthy(phase) || !sessionShowReasoning(ctx, sid) {
		return
	}
	payload := map[string]any{"phase": pyrepr.Str(phase)}
	for _, pair := range [][2]string{{"moa_refs_done", "refs_done"}, {"moa_refs_total", "refs_total"}} {
		if value := kw[pair[0]]; value != nil {
			payload[pair[1]] = progressInt(value)
		}
	}
	if truthy(name) {
		payload["aggregator"] = pyrepr.Str(name)
	}
	emitEvent(ctx, "moa.phase", sid, payload)
}

// Emits the aggregating state transition both clients use for the busy indicator
func progressMoaAggregating(ctx context.Context, sid string, name, preview any, kw map[string]any) {
	emitEvent(ctx, "moa.aggregating", sid, map[string]any{"aggregator": strOr(name)})
}

// Emits a subagent lifecycle event and mirrors it into the child's watch window
func progressSubagent(ctx context.Context, sid string, name, preview any, kw map[string]any, eventType string) {
	payload, err := tuigateway.SubagentPayload(eventType, name, preview, kw, sessionShowReasoning(ctx, sid))
	if err != nil {
		panic(err)
	}
	if eventType != "subagent.text" {
		emitEvent(ctx, eventType, sid, objectMap(payload))
	}
	if parent := tuigateway.SessionRecord(sid); parent != nil {
		mirrorSubagentToChild(ctx, eventType, objectMap(payload), parent.Get("profile_home"))
	}
}

// One progress event handler and the argument that must be truthy for its row
type progressHandler struct {
	handle   func(ctx context.Context, sid string, name, preview any, kw map[string]any)
	requires string
}

// Progress event handlers by event type
var progressHandlers = map[string]progressHandler{
	"tool.output_risk":    {progressOutputRisk, "name"},
	"tool.progress":       {progressToolProgress, "name"},
	"reasoning.available": {progressReasoning, "preview"},
	"moa.reference":       {progressMoaReference, "name"},
	"moa.aggregating":     {progressMoaAggregating, ""},
	"moa.progress":        {progressMoaProgress, ""},
	"moa.phase":           {progressMoaPhase, ""},
}

// Routes one agent progress event to its handler, subagent lifecycle surviving display.tool_progress off
func onToolProgress(ctx context.Context, sid, eventType string, name, preview any, kw map[string]any) {
	if eventType == "tool.started" && truthy(name) {
		return
	}
	if strings.HasPrefix(eventType, "subagent.") {
		progressSubagent(ctx, sid, name, preview, kw, eventType)
		return
	}
	handler, known := progressHandlers[eventType]
	if !known {
		return
	}
	switch handler.requires {
	case "name":
		if !truthy(name) {
			return
		}
	case "preview":
		if !truthy(preview) {
			return
		}
	}
	handler.handle(ctx, sid, name, preview, kw)
}
