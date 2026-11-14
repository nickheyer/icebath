package raft

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"
	"weak"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	defaultActivityQueueCap = 500
	activityContentCap      = 4096
	activityEventSchema     = "raft-activity.v1"
	activityDrainSchema     = "raft-activity-drain.v1"
	maxScalarLength         = 120
)

var (
	// Field names that carry message content and are refused in a wake payload
	contentFieldNames = map[string]bool{"body": true, "content": true, "message": true, "messages": true, "preview": true,
		"snippet": true, "text": true}
	safeScalarRE = pyre.MustCompile(`^[a-zA-Z0-9._:@/ -]+$`)
	// Fields an activity event may carry
	activityAllowedFields = map[string]bool{"schema": true, "eventId": true, "sessionId": true, "hookEventName": true, "status": true,
		"occurredAt": true, "toolName": true, "toolInput": true, "toolOutput": true, "toolInputTruncated": true,
		"toolOutputTruncated": true, "truncated": true, "errorClass": true, "durationMs": true}

	activeAdaptersMu sync.Mutex
	activeAdapters   = map[weak.Pointer[RaftAdapter]]struct{}{}

	raftContextMu      sync.Mutex
	raftSessionIDs     = map[string]bool{}
	raftTurnIDs        = map[string]bool{}
	raftPromptTurnIDs  = map[string]bool{}
)

// Whether a decoded JSON value holds a content named key at any depth
func hasContentField(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		for k, item := range v {
			if contentFieldNames[pystr.Lower(pystr.Strip(k))] || hasContentField(item) {
				return true
			}
		}
	case []any:
		for _, item := range v {
			if hasContentField(item) {
				return true
			}
		}
	}
	return false
}

// The value when it is a short string of safe characters, false otherwise
func safeScalar(value any) (string, bool) {
	text, ok := value.(string)
	if !ok || text == "" || len([]rune(text)) > maxScalarLength {
		return "", false
	}
	matched, err := safeScalarRE.MatchString(text)
	if err != nil || !matched {
		return "", false
	}
	return text, true
}

// Text of a tool input or output cut to the content cap with whether it was cut, false for nothing to report
func contentString(value any) (string, bool, bool) {
	if value == nil {
		return "", false, false
	}
	text, isStr := value.(string)
	if !isStr {
		encoded, err := pyjson.Dumps(value, pyjson.EnsureASCII(false), pyjson.SortKeys(true))
		if err != nil {
			return "", false, false
		}
		text = encoded
	}
	if text == "" {
		return "", false, false
	}
	rs := []rune(text)
	if len(rs) > activityContentCap {
		return string(rs[:activityContentCap]), true, true
	}
	return text, false, true
}

// Truncated integer of a non negative int or float, nil for anything else, the error int raises for a non finite float
func durationMs(value any) (any, error) {
	switch v := value.(type) {
	case int:
		if v < 0 {
			return nil, nil
		}
		return int64(v), nil
	case int64:
		if v < 0 {
			return nil, nil
		}
		return v, nil
	case *big.Int:
		if v.Sign() < 0 {
			return nil, nil
		}
		return v, nil
	case float64:
		if math.IsNaN(v) {
			return nil, &gateway.PyError{Kind: "ValueError", Msg: "cannot convert float NaN to integer"}
		}
		if math.IsInf(v, 0) {
			return nil, &gateway.PyError{Kind: "OverflowError", Msg: "cannot convert float infinity to integer"}
		}
		n, err := pyint.Of(v)
		if err != nil {
			return nil, err
		}
		if n.Sign() < 0 {
			return nil, nil
		}
		if n.IsInt64() {
			return n.Int64(), nil
		}
		return n, nil
	}
	return nil, nil
}

// Fields of one activity event beyond its hook name and session id
type activityFields struct {
	status     string
	toolName   any
	toolInput  any
	toolOutput any
	errorClass any
	durationMs any
}

// Builds a validated shape activity event for a hook, content capped and unsafe scalars dropped
func makeActivityEvent(hookEventName string, sessionID any, f activityFields) (*pyjson.Object, error) {
	session, ok := safeScalar(sessionID)
	if !ok {
		session = "unknown"
	}
	status := "ok"
	if f.status == "error" {
		status = "error"
	}
	occurredAt := strings.Replace(pydatetime.ISOFormatUTC(time.Now()), "+00:00", "Z", -1)
	event := pyjson.ObjectOf("schema", activityEventSchema, "eventId", "icebath-"+uuid.NewString(), "sessionId", session,
		"hookEventName", hookEventName, "status", status, "occurredAt", occurredAt)
	for _, row := range []struct {
		key string
		raw any
	}{{"toolName", f.toolName}, {"errorClass", f.errorClass}} {
		if safe, ok := safeScalar(row.raw); ok {
			event.Set(row.key, safe)
		}
	}
	duration, err := durationMs(f.durationMs)
	if err != nil {
		return nil, err
	}
	if duration != nil {
		event.Set("durationMs", duration)
	}
	for _, row := range []struct {
		key string
		raw any
	}{{"toolInput", f.toolInput}, {"toolOutput", f.toolOutput}} {
		if text, truncated, ok := contentString(row.raw); ok {
			event.Set(row.key, text)
			if truncated {
				event.Set(row.key+"Truncated", true)
				event.Set("truncated", true)
			}
		}
	}
	return event, nil
}

// ValueError of an activity event that fails validation
func activityError(msg string) error { return &gateway.PyError{Kind: "ValueError", Msg: msg} }

// Optional field groups in check order with the description of what each must be
var optionalFieldRules = []struct {
	keys []string
	ok   func(v any) (bool, error)
	what string
}{
	{[]string{"toolName", "errorClass"}, func(v any) (bool, error) { _, ok := safeScalar(v); return ok, nil }, "a safe string"},
	{[]string{"durationMs"}, func(v any) (bool, error) { d, err := durationMs(v); return d != nil, err }, "a non-negative number"},
	{[]string{"truncated", "toolInputTruncated", "toolOutputTruncated"}, func(v any) (bool, error) { _, ok := v.(bool); return ok, nil },
		"a boolean"},
}

// Copy of a well formed activity event with its duration truncated and its content capped, ValueError otherwise
func validateActivityEvent(value any) (*pyjson.Object, error) {
	source, ok := value.(*pyjson.Object)
	if !ok {
		return nil, activityError("activity event must be an object")
	}
	if schema, _ := source.Get("schema"); !pyrepr.Equal(schema, activityEventSchema) {
		return nil, activityError("unsupported activity event schema")
	}
	var unknown []string
	for _, k := range source.Keys {
		if !activityAllowedFields[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, activityError(fmt.Sprintf("activity event field %s is not allowed", unknown[0]))
	}
	for _, key := range []string{"eventId", "sessionId", "hookEventName", "occurredAt"} {
		field, _ := source.Get(key)
		if _, ok := safeScalar(field); !ok {
			return nil, activityError(fmt.Sprintf("activity event %s must be a safe non-empty string", key))
		}
	}
	status, _ := source.Get("status")
	switch status.(type) {
	case []any, *pyjson.Object:
		return nil, &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("unhashable type: '%s'", pyrepr.TypeName(status))}
	}
	if !pyrepr.Equal(status, "ok") && !pyrepr.Equal(status, "error") {
		return nil, activityError("activity event status must be ok|error")
	}
	for _, rule := range optionalFieldRules {
		for _, key := range rule.keys {
			field, _ := source.Get(key)
			if field == nil {
				continue
			}
			passes, err := rule.ok(field)
			if err != nil {
				return nil, err
			}
			if !passes {
				return nil, activityError(fmt.Sprintf("activity event %s must be %s", key, rule.what))
			}
		}
	}
	event := source.Copy()
	if raw, _ := event.Get("durationMs"); raw != nil {
		duration, err := durationMs(raw)
		if err != nil {
			return nil, err
		}
		event.Set("durationMs", duration)
	}
	for _, key := range []string{"toolInput", "toolOutput"} {
		content, _ := event.Get(key)
		if content == nil {
			continue
		}
		text, isStr := content.(string)
		if !isStr {
			return nil, activityError(fmt.Sprintf("activity event %s must be a string", key))
		}
		if rs := []rune(text); len(rs) > activityContentCap {
			event.Set(key, string(rs[:activityContentCap]))
			event.Set("truncated", true)
			event.Set(key+"Truncated", true)
		}
	}
	return event, nil
}

// Bounded at most once queue of Raft external activity telemetry
type ActivityQueue struct {
	mu                sync.Mutex
	limit             int
	events            []*pyjson.Object
	droppedSinceDrain int
}

// Queue holding at most cap events, the default cap for zero
func NewActivityQueue(cap int) *ActivityQueue {
	if cap == 0 {
		cap = defaultActivityQueueCap
	}
	return &ActivityQueue{limit: max(1, cap)}
}

// Validates and appends an event, dropping the oldest ones past the cap and counting them
func (q *ActivityQueue) Push(event any) error {
	validated, err := validateActivityEvent(event)
	if err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.events = append(q.events, validated)
	for len(q.events) > q.limit {
		q.events = q.events[1:]
		q.droppedSinceDrain++
	}
	return nil
}

// Removes and returns up to maxEvents oldest events with the count dropped since the last drain
func (q *ActivityQueue) Drain(maxEvents int) *pyjson.Object {
	if maxEvents == 0 {
		maxEvents = 200
	}
	limit := max(1, maxEvents)
	q.mu.Lock()
	n := min(limit, len(q.events))
	events := make([]any, n)
	for i, e := range q.events[:n] {
		events[i] = e
	}
	q.events = q.events[n:]
	dropped := q.droppedSinceDrain
	q.droppedSinceDrain = 0
	q.mu.Unlock()
	return pyjson.ObjectOf("schema", activityDrainSchema, "events", events, "dropped", dropped)
}

// Number of queued events
func (q *ActivityQueue) Size() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.events)
}

// Forgets a finished turn and, when asked, its whole session
func forgetRaftContext(sessionID, turnID any, forgetSession bool) {
	safeSessionID, hasSession := safeScalar(sessionID)
	safeTurnID, hasTurn := safeScalar(turnID)
	raftContextMu.Lock()
	defer raftContextMu.Unlock()
	if hasTurn {
		delete(raftTurnIDs, safeTurnID)
		delete(raftPromptTurnIDs, safeTurnID)
	}
	if forgetSession && hasSession {
		delete(raftSessionIDs, safeSessionID)
	}
}

// Platform value of a hook payload as text, empty for a missing or falsy one
func platformText(platform any) string {
	switch p := platform.(type) {
	case nil:
		return ""
	case gateway.Platform:
		return string(p)
	case string:
		return p
	case fmt.Stringer:
		return p.String()
	}
	if !pyrepr.Truthy(platform) {
		return ""
	}
	return pyrepr.Str(platform)
}

// Whether a hook payload belongs to Raft, an explicit raft platform also learning its session and turn ids
func isRaftContext(kwargs map[string]any) bool {
	safeSessionID, hasSession := safeScalar(kwargs["session_id"])
	safeTurnID, hasTurn := safeScalar(kwargs["turn_id"])
	raftContextMu.Lock()
	defer raftContextMu.Unlock()
	if platformText(kwargs["platform"]) == "raft" {
		if hasSession {
			raftSessionIDs[safeSessionID] = true
		}
		if hasTurn {
			raftTurnIDs[safeTurnID] = true
		}
		return true
	}
	return (hasTurn && raftTurnIDs[safeTurnID]) || (hasSession && raftSessionIDs[safeSessionID])
}

// Builds the activity event of a hook and hands it to every live adapter
func emit(hookEventName string, kwargs map[string]any, f activityFields) error {
	event, err := makeActivityEvent(hookEventName, kwargs["session_id"], f)
	if err != nil {
		return err
	}
	activeAdaptersMu.Lock()
	adapters := make([]*RaftAdapter, 0, len(activeAdapters))
	for ref := range activeAdapters {
		if adapter := ref.Value(); adapter != nil {
			adapters = append(adapters, adapter)
		} else {
			delete(activeAdapters, ref)
		}
	}
	activeAdaptersMu.Unlock()
	for _, adapter := range adapters {
		adapter.ReportActivity(event)
	}
	return nil
}

// Hook that runs its body only for Raft sessions
func raftHook(fn func(ctx context.Context, kwargs map[string]any) error) func(ctx context.Context, kwargs map[string]any) (any, error) {
	return func(ctx context.Context, kwargs map[string]any) (any, error) {
		if isRaftContext(kwargs) {
			return nil, fn(ctx, kwargs)
		}
		return nil, nil
	}
}

// Lets RAFT_PROFILE through to sandboxed commands and reports the session start
var onSessionStart = raftHook(func(ctx context.Context, kwargs map[string]any) error {
	func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Debug("[raft] failed to register RAFT_PROFILE env passthrough", "error", r)
			}
		}()
		tools.RegisterEnvPassthrough(&tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}, []string{"RAFT_PROFILE"})
	}()
	return emit("SessionStart", kwargs, activityFields{})
})

// Reports the prompt submit once per turn
var onPreLLMCall = raftHook(func(ctx context.Context, kwargs map[string]any) error {
	if safeTurnID, ok := safeScalar(kwargs["turn_id"]); ok {
		raftContextMu.Lock()
		seen := raftPromptTurnIDs[safeTurnID]
		raftPromptTurnIDs[safeTurnID] = true
		raftContextMu.Unlock()
		if seen {
			return nil
		}
	}
	return emit("UserPromptSubmit", kwargs, activityFields{})
})

// Reports a tool call about to run with its input
var onPreToolCall = raftHook(func(ctx context.Context, kwargs map[string]any) error {
	return emit("PreToolUse", kwargs, activityFields{toolName: kwargs["tool_name"], toolInput: kwargs["args"]})
})

// Reports a finished tool call, a failed or blocked one as a failure with its error class
var onPostToolCall = raftHook(func(ctx context.Context, kwargs map[string]any) error {
	status := "ok"
	if pyrepr.Equal(kwargs["status"], "error") || pyrepr.Equal(kwargs["status"], "blocked") || pyrepr.Truthy(kwargs["error_type"]) {
		status = "error"
	}
	name := "PostToolUse"
	if status == "error" {
		name = "PostToolUseFailure"
	}
	output := kwargs["error_message"]
	if !pyrepr.Truthy(output) {
		output = kwargs["result"]
	}
	errorClass := kwargs["error_type"]
	if !pyrepr.Truthy(errorClass) {
		errorClass = nil
		if status == "error" {
			errorClass = "tool_failure"
		}
	}
	return emit(name, kwargs, activityFields{status: status, toolName: kwargs["tool_name"], toolInput: kwargs["args"],
		toolOutput: output, errorClass: errorClass, durationMs: kwargs["duration_ms"]})
})

// Reports the end of a model turn
var onPostLLMCall = raftHook(func(ctx context.Context, kwargs map[string]any) error {
	return emit("Stop", kwargs, activityFields{})
})

// Reports an interrupted or incomplete session end and forgets the turn
var onSessionEnd = raftHook(func(ctx context.Context, kwargs map[string]any) error {
	interrupted := pyrepr.Truthy(kwargs["interrupted"])
	if completed, isBool := kwargs["completed"].(bool); interrupted || (isBool && !completed) {
		errorClass := "incomplete"
		if interrupted {
			errorClass = "interrupted"
		}
		if err := emit("Stop", kwargs, activityFields{status: "error", errorClass: errorClass}); err != nil {
			return err
		}
	}
	forgetRaftContext(kwargs["session_id"], kwargs["turn_id"], false)
	return nil
})

// Reports the session end and forgets the session and turn
var onSessionFinalize = raftHook(func(ctx context.Context, kwargs map[string]any) error {
	if err := emit("SessionEnd", kwargs, activityFields{}); err != nil {
		return err
	}
	forgetRaftContext(kwargs["session_id"], kwargs["turn_id"], true)
	return nil
})
