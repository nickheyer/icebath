package raft

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	activityEventSchema   = "raft-activity.v1"
	activityDrainSchema   = "raft-activity-drain.v1"
	activityContentCap    = 4096
	defaultActivityCap    = 500
	defaultDrainMax       = 200
	maxSafeScalarLength   = 120
	activityStatusOK      = "ok"
	activityStatusError   = "error"
	unknownActivitySessID = "unknown"
)

var safeScalarRE = regexp.MustCompile(`^[a-zA-Z0-9._:@/ -]+$`)

// Fields an activity event may carry
var activityFields = map[string]bool{"schema": true, "eventId": true, "sessionId": true, "hookEventName": true, "status": true,
	"occurredAt": true, "toolName": true, "toolInput": true, "toolOutput": true, "toolInputTruncated": true,
	"toolOutputTruncated": true, "truncated": true, "errorClass": true, "durationMs": true}

// Short identifier text of a value, false for anything outside the safe character set or length
func safeScalar(v any) (string, bool) {
	s, ok := v.(string)
	if !ok || s == "" || utf8.RuneCountInString(s) > maxSafeScalarLength || !safeScalarRE.MatchString(s) {
		return "", false
	}
	return s, true
}

// Content text of a value capped at the content limit and whether it was cut, false for empty content
func contentString(v any) (string, bool, bool) {
	if v == nil {
		return "", false, false
	}
	text, isString := v.(string)
	if !isString {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			return "", false, false
		}
		text = string(bytes.TrimRight(buf.Bytes(), "\n"))
	}
	if text == "" {
		return "", false, false
	}
	capped, cut := capContent(text)
	return capped, cut, true
}

// Text cut to the content cap in characters and whether it was cut
func capContent(text string) (string, bool) {
	if utf8.RuneCountInString(text) <= activityContentCap {
		return text, false
	}
	return string([]rune(text)[:activityContentCap]), true
}

// Non negative whole milliseconds of a numeric value, false for booleans, negatives and non numbers
func durationMs(v any) (int64, bool) {
	var f float64
	switch n := v.(type) {
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case int32:
		f = float64(n)
	case uint64:
		f = float64(n)
	case float64:
		f = n
	case float32:
		f = float64(n)
	case time.Duration:
		f = float64(n.Milliseconds())
	case json.Number:
		if i, err := n.Int64(); err == nil {
			if i < 0 {
				return 0, false
			}
			return i, true
		}
		parsed, err := n.Float64()
		if err != nil {
			return 0, false
		}
		f = parsed
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	whole := math.Trunc(f)
	if whole < 0 || whole >= math.MaxInt64 {
		return 0, false
	}
	return int64(whole), true
}

// Fields of one hook emitted activity event beyond its name and session
type activityFieldsIn struct {
	status     string
	toolName   any
	toolInput  any
	toolOutput any
	errorClass any
	durationMs any
}

// Activity event of a hook, every field reduced to safe scalars and capped content
func makeActivityEvent(hookEventName string, sessionID any, f activityFieldsIn) map[string]any {
	session, ok := safeScalar(sessionID)
	if !ok {
		session = unknownActivitySessID
	}
	status := activityStatusOK
	if f.status == activityStatusError {
		status = activityStatusError
	}
	event := map[string]any{"schema": activityEventSchema, "eventId": "icebath-" + uuid.NewString(), "sessionId": session,
		"hookEventName": hookEventName, "status": status, "occurredAt": time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")}
	if s, ok := safeScalar(f.toolName); ok {
		event["toolName"] = s
	}
	if s, ok := safeScalar(f.errorClass); ok {
		event["errorClass"] = s
	}
	if ms, ok := durationMs(f.durationMs); ok {
		event["durationMs"] = ms
	}
	for _, field := range []struct {
		key string
		raw any
	}{{"toolInput", f.toolInput}, {"toolOutput", f.toolOutput}} {
		if text, cut, ok := contentString(field.raw); ok {
			event[field.key] = text
			if cut {
				event[field.key+"Truncated"], event["truncated"] = true, true
			}
		}
	}
	return event
}

// Checked copy of an activity event, the error naming the first rule it breaks
func validateActivityEvent(value any) (map[string]any, error) {
	in, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("activity event must be an object")
	}
	if in["schema"] != activityEventSchema {
		return nil, errors.New("unsupported activity event schema")
	}
	var unknown []string
	for k := range in {
		if !activityFields[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("activity event field %s is not allowed", unknown[0])
	}
	for _, key := range []string{"eventId", "sessionId", "hookEventName", "occurredAt"} {
		if _, ok := safeScalar(in[key]); !ok {
			return nil, fmt.Errorf("activity event %s must be a safe non-empty string", key)
		}
	}
	if s := in["status"]; s != activityStatusOK && s != activityStatusError {
		return nil, errors.New("activity event status must be ok|error")
	}
	for _, key := range []string{"toolName", "errorClass"} {
		if v := in[key]; v != nil {
			if _, ok := safeScalar(v); !ok {
				return nil, fmt.Errorf("activity event %s must be a safe string", key)
			}
		}
	}
	if v := in["durationMs"]; v != nil {
		if _, ok := durationMs(v); !ok {
			return nil, errors.New("activity event durationMs must be a non-negative number")
		}
	}
	for _, key := range []string{"truncated", "toolInputTruncated", "toolOutputTruncated"} {
		if v := in[key]; v != nil {
			if _, ok := v.(bool); !ok {
				return nil, fmt.Errorf("activity event %s must be a boolean", key)
			}
		}
	}
	event := make(map[string]any, len(in))
	for k, v := range in {
		event[k] = v
	}
	if v := in["durationMs"]; v != nil {
		ms, _ := durationMs(v)
		event["durationMs"] = ms
	}
	for _, key := range []string{"toolInput", "toolOutput"} {
		v := in[key]
		if v == nil {
			continue
		}
		text, isString := v.(string)
		if !isString {
			return nil, fmt.Errorf("activity event %s must be a string", key)
		}
		if capped, cut := capContent(text); cut {
			event[key], event["truncated"], event[key+"Truncated"] = capped, true, true
		}
	}
	return event, nil
}

// Bounded at most once queue of activity telemetry, the oldest dropped past the cap
type activityQueue struct {
	mu      sync.Mutex
	cap     int
	events  []map[string]any
	dropped int
}

func newActivityQueue(capacity int) *activityQueue {
	if capacity < 1 {
		capacity = defaultActivityCap
	}
	return &activityQueue{cap: capacity}
}

// Validates and appends an event, counting what the cap pushes out
func (q *activityQueue) push(value any) error {
	event, err := validateActivityEvent(value)
	if err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.events = append(q.events, event)
	for len(q.events) > q.cap {
		q.events[0] = nil
		q.events = q.events[1:]
		q.dropped++
	}
	return nil
}

// Removes up to maxEvents events and reports how many were dropped since the last drain
func (q *activityQueue) drain(maxEvents int) map[string]any {
	if maxEvents < 1 {
		maxEvents = 1
	}
	q.mu.Lock()
	n := min(maxEvents, len(q.events))
	events := make([]any, 0, n)
	for _, e := range q.events[:n] {
		events = append(events, e)
	}
	q.events = append([]map[string]any(nil), q.events[n:]...)
	dropped := q.dropped
	q.dropped = 0
	q.mu.Unlock()
	return map[string]any{"schema": activityDrainSchema, "events": events, "dropped": dropped}
}

// Number of queued events
func (q *activityQueue) size() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.events)
}

// Session and turn ids learned from Raft hook payloads, consulted by hooks that carry no platform
var raftContext = struct {
	sync.Mutex
	sessions    map[string]bool
	turns       map[string]bool
	promptTurns map[string]bool
}{sessions: map[string]bool{}, turns: map[string]bool{}, promptTurns: map[string]bool{}}

// Whether a hook payload belongs to a Raft session, a raft platform payload teaching its ids
func isRaftContext(kwargs map[string]any) bool {
	session, hasSession := safeScalar(kwargs["session_id"])
	turn, hasTurn := safeScalar(kwargs["turn_id"])
	platform := ""
	if p := kwargs["platform"]; p != nil {
		platform = fmt.Sprint(p)
	}
	raftContext.Lock()
	defer raftContext.Unlock()
	if platform == string(platformName) {
		if hasSession {
			raftContext.sessions[session] = true
		}
		if hasTurn {
			raftContext.turns[turn] = true
		}
		return true
	}
	return (hasTurn && raftContext.turns[turn]) || (hasSession && raftContext.sessions[session])
}

// Forgets a turn and optionally its session once Raft hooks no longer need them
func forgetRaftContext(sessionID, turnID any, forgetSession bool) {
	session, hasSession := safeScalar(sessionID)
	turn, hasTurn := safeScalar(turnID)
	raftContext.Lock()
	defer raftContext.Unlock()
	if hasTurn {
		delete(raftContext.turns, turn)
		delete(raftContext.promptTurns, turn)
	}
	if forgetSession && hasSession {
		delete(raftContext.sessions, session)
	}
}

// Builds the hook's activity event and queues it on every connected Raft adapter
func emitActivity(hookEventName string, kwargs map[string]any, f activityFieldsIn) {
	event := makeActivityEvent(hookEventName, kwargs["session_id"], f)
	active.Lock()
	adapters := make([]*Adapter, 0, len(active.set))
	for a := range active.set {
		adapters = append(adapters, a)
	}
	active.Unlock()
	for _, a := range adapters {
		a.reportActivity(event)
	}
}

// Hook that runs body only for Raft sessions
func raftHook(body func(ctx context.Context, kwargs map[string]any)) func(context.Context, map[string]any) (any, error) {
	return func(ctx context.Context, kwargs map[string]any) (any, error) {
		if isRaftContext(kwargs) {
			body(ctx, kwargs)
		}
		return nil, nil
	}
}

// Whether a hook value is set, non empty text and anything but false or nil
func present(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case bool:
		return x
	}
	return true
}

// Raft lifecycle hooks by hook name
var raftHooks = map[string]func(context.Context, map[string]any) (any, error){
	"on_session_start": raftHook(func(ctx context.Context, kwargs map[string]any) {
		tools.RegisterEnvPassthrough(&tools.Env{Home: home.Dir(ctx)}, []string{"RAFT_PROFILE"})
		emitActivity("SessionStart", kwargs, activityFieldsIn{})
	}),
	"pre_llm_call": raftHook(func(_ context.Context, kwargs map[string]any) {
		if turn, ok := safeScalar(kwargs["turn_id"]); ok {
			raftContext.Lock()
			seen := raftContext.promptTurns[turn]
			raftContext.promptTurns[turn] = true
			raftContext.Unlock()
			if seen {
				return
			}
		}
		emitActivity("UserPromptSubmit", kwargs, activityFieldsIn{})
	}),
	"pre_tool_call": raftHook(func(_ context.Context, kwargs map[string]any) {
		emitActivity("PreToolUse", kwargs, activityFieldsIn{toolName: kwargs["tool_name"], toolInput: kwargs["args"]})
	}),
	"post_tool_call": raftHook(func(_ context.Context, kwargs map[string]any) {
		status := kwargs["status"]
		failed := status == "error" || status == "blocked" || present(kwargs["error_type"])
		f := activityFieldsIn{status: activityStatusOK, toolName: kwargs["tool_name"], toolInput: kwargs["args"],
			toolOutput: kwargs["result"], errorClass: kwargs["error_type"], durationMs: kwargs["duration_ms"]}
		if present(kwargs["error_message"]) {
			f.toolOutput = kwargs["error_message"]
		}
		name := "PostToolUse"
		if failed {
			f.status, name = activityStatusError, "PostToolUseFailure"
			if !present(f.errorClass) {
				f.errorClass = "tool_failure"
			}
		}
		emitActivity(name, kwargs, f)
	}),
	"post_llm_call": raftHook(func(_ context.Context, kwargs map[string]any) {
		emitActivity("Stop", kwargs, activityFieldsIn{})
	}),
	"on_session_end": raftHook(func(_ context.Context, kwargs map[string]any) {
		interrupted := present(kwargs["interrupted"])
		if completed, isBool := kwargs["completed"].(bool); interrupted || (isBool && !completed) {
			class := "incomplete"
			if interrupted {
				class = "interrupted"
			}
			emitActivity("Stop", kwargs, activityFieldsIn{status: activityStatusError, errorClass: class})
		}
		forgetRaftContext(kwargs["session_id"], kwargs["turn_id"], false)
	}),
	"on_session_finalize": raftHook(func(_ context.Context, kwargs map[string]any) {
		emitActivity("SessionEnd", kwargs, activityFieldsIn{})
		forgetRaftContext(kwargs["session_id"], kwargs["turn_id"], true)
	}),
}

// Hook names in registration order
var raftHookOrder = []string{"on_session_start", "pre_llm_call", "pre_tool_call", "post_tool_call", "post_llm_call",
	"on_session_end", "on_session_finalize"}

// Queues a hook event, a validation failure only logged
func (a *Adapter) reportActivity(event map[string]any) {
	if err := a.activity.push(event); err != nil {
		slog.Debug("[raft] activity event dropped during validation: " + err.Error())
	}
}

// Drain size of a max query value, the default for anything that is not an integer
func drainLimit(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return defaultDrainMax
	}
	if n == 0 {
		return defaultDrainMax
	}
	return n
}
