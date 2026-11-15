package langfuse

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Ceiling on live trace state, the least recently updated turns evicted past it
var maxTraceState = 256

// Insertion ordered string keyed map with dict and OrderedDict operations
type omap[V any] struct {
	keys []string
	vals map[string]V
}

// Empty ordered map
func newOmap[V any]() *omap[V] { return &omap[V]{vals: map[string]V{}} }

// Value under key and whether it is present
func (m *omap[V]) get(k string) (V, bool) {
	v, ok := m.vals[k]
	return v, ok
}

// Stores a value, a present key keeping its position
func (m *omap[V]) set(k string, v V) {
	if _, ok := m.vals[k]; !ok {
		m.keys = append(m.keys, k)
	}
	m.vals[k] = v
}

// Removes a key returning its value and whether it was present
func (m *omap[V]) pop(k string) (V, bool) {
	v, ok := m.vals[k]
	if !ok {
		return v, false
	}
	delete(m.vals, k)
	for i, key := range m.keys {
		if key == k {
			m.keys = append(m.keys[:i:i], m.keys[i+1:]...)
			break
		}
	}
	return v, true
}

// Moves a present key to the end
func (m *omap[V]) moveToEnd(k string) {
	v, ok := m.pop(k)
	if ok {
		m.set(k, v)
	}
}

// Number of entries
func (m *omap[V]) len() int { return len(m.keys) }

// Values in order
func (m *omap[V]) values() []V {
	out := make([]V, len(m.keys))
	for i, k := range m.keys {
		out[i] = m.vals[k]
	}
	return out
}

// Keys in order
func (m *omap[V]) keyList() []string { return append([]string(nil), m.keys...) }

// Removes every entry
func (m *omap[V]) clear() {
	m.keys = nil
	m.vals = map[string]V{}
}

// Live Langfuse state of one turn
type traceState struct {
	traceID          string
	root             *observation
	generations      *omap[*observation]
	generationModels *omap[string]
	tools            *omap[*observation]
	pendingTools     *omap[[]*observation]
	turnToolCalls    []any
	subagents        *omap[*observation]
	moaEmitted       map[string]bool
	lastUpdated      time.Time
	propagated       *pyjson.Object
}

// Fresh state around an opened root
func newTraceState(traceID string, root *observation, propagated *pyjson.Object) *traceState {
	return &traceState{traceID: traceID, root: root, generations: newOmap[*observation](), generationModels: newOmap[string](),
		tools: newOmap[*observation](), pendingTools: newOmap[[]*observation](), subagents: newOmap[*observation](),
		moaEmitted: map[string]bool{}, lastUpdated: time.Now(), propagated: propagated}
}

var (
	stateMu            sync.Mutex
	traceStates        = newOmap[*traceState]()
	senderByTurn       = newOmap[string]()
	userByChildSession = newOmap[string]()
)

// Runs fn holding the state lock, released even when fn raises
func withState(fn func()) {
	stateMu.Lock()
	defer stateMu.Unlock()
	fn()
}

// Identifier of the calling goroutine, the thread identity of a scope with no task or session
func goroutineID() string {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	fields := strings.Fields(strings.TrimPrefix(string(buf[:n]), "goroutine "))
	if len(fields) == 0 {
		return "0"
	}
	if _, err := strconv.ParseUint(fields[0], 10, 64); err != nil {
		return "0"
	}
	return fields[0]
}

// In process key of one turn's trace, the turn id winning over the request id so turn and request hooks meet
func traceKey(taskID, sessionID, turnID, apiRequestID string) string {
	scope := "thread:" + goroutineID()
	if taskID != "" {
		scope = "task:" + taskID
	} else if sessionID != "" {
		scope = "session:" + sessionID
	}
	if turnID != "" {
		return scope + ":turn:" + turnID
	}
	if apiRequestID != "" {
		return scope + ":api:" + apiRequestID
	}
	if taskID != "" {
		return taskID
	}
	return scope
}

// Stores or refreshes a value dropping the oldest entries over the cap, the caller holding the state lock
func rememberLocked(m *omap[string], key, value string) {
	m.set(key, value)
	m.moveToEnd(key)
	for m.len() > maxTraceState {
		m.pop(m.keys[0])
	}
}

// Messaging user behind a turn, its own sender else the user a subagent session inherited, the caller holding the lock
func turnUserLocked(turnID, sessionID string) string {
	if turnID != "" {
		if v, ok := senderByTurn.get(turnID); ok && v != "" {
			return v
		}
	}
	v, _ := userByChildSession.get(sessionID)
	return v
}

// Live state of a turn id alone matched on its turn suffix, the caller holding the lock
func stateForTurn(turnID string) *traceState {
	if turnID == "" {
		return nil
	}
	suffix := ":turn:" + turnID
	for _, k := range traceStates.keys {
		if strings.HasSuffix(k, suffix) {
			return traceStates.vals[k]
		}
	}
	return nil
}

// Identity and routing of the turn a root trace opens for
type rootArgs struct {
	taskID, sessionID, platform, provider, model, apiMode string
	messages                                              any
	turnID, apiRequestID, userID                          string
}

// Content free root tags naming the platform, the trigger and a cron run's stable job id
func traceTags(ctx context.Context, platform string) []any {
	p := pystr.Lower(pystr.Strip(platform))
	var cron any
	if pc := pluginContext(); pc != nil {
		exec, err := pc.CurrentCronExecution(ctx)
		if err != nil {
			panic(err)
		}
		cron = exec
	}
	tags := []any{"icebath", "langfuse"}
	if p != "" {
		tags = append(tags, "platform:"+p)
	}
	if p == "cron" {
		tags = append(tags, "trigger:cron")
	} else {
		tags = append(tags, "trigger:interactive")
	}
	if truthy(cron) {
		tags = append(tags, "cron-job:"+pyStr(getattr(cron, "JobID")))
	}
	return tags
}

// Opens the turn's root chain with its trace attributes propagated, retried bare when the propagated open raises
func startRootTrace(ctx context.Context, c *lfClient, key string, a rootArgs) *traceState {
	scope := a.turnID
	for _, s := range []string{a.apiRequestID, a.taskID, key} {
		if scope == "" {
			scope = s
		}
	}
	seed := a.sessionID
	if seed == "" {
		seed = "sessionless"
	}
	traceID := createTraceID(seed + "::" + scope)
	var lastUser any
	if elems, ok := listItems(a.messages); ok {
		for i := len(elems) - 1; i >= 0; i-- {
			if isDict(elems[i]) && strEq(dget(elems[i], "role"), "user") {
				lastUser = elems[i]
				break
			}
		}
	}
	var traceInput any
	if lastUser != nil {
		traceInput = obj("role", "user", "content", captureContent(dget(lastUser, "content"), false, nil))
	}
	profile := ""
	if pc := pluginContext(); pc != nil {
		profile = pc.ProfileName(ctx)
	}
	kanban := pyjson.NewObject()
	if v := envStr("ICEBATH_KANBAN_TASK", ""); v != "" {
		kanban.Set("kanban_task_id", v)
	}
	if v := envStr("ICEBATH_KANBAN_RUN_ID", ""); v != "" {
		kanban.Set("kanban_run_id", v)
	}
	metadata := obj("source", "icebath", "task_id", a.taskID, "turn_id", a.turnID, "api_request_id", a.apiRequestID,
		"platform", a.platform, "provider", a.provider, "model", a.model, "api_mode", a.apiMode,
		"capture_mode", captureMode(), "profile", profile)
	metadata.Update(kanban)
	sessionAttr := a.sessionID
	if sessionAttr == "" {
		sessionAttr = key
	}
	propagated := obj("session_id", sessionAttr, "trace_name", "Icebath turn", "tags", traceTags(ctx, a.platform))
	if a.userID != "" {
		propagated.Set("user_id", a.userID)
	}
	if profile != "" || kanban.Len() > 0 {
		md := pyjson.NewObject()
		if profile != "" {
			md.Set("profile", profile)
		}
		md.Update(kanban)
		propagated.Set("metadata", md)
	}
	var root *observation
	func() {
		defer func() {
			if recover() != nil {
				root = nil
			}
		}()
		root = c.startRoot(traceID, "Icebath turn", "chain", traceInput, metadata, propagated)
	}()
	if root == nil {
		root = c.startRoot(traceID, "Icebath turn", "chain", traceInput, metadata, nil)
	}
	failsafe("set_trace_io(input)", func() { root.setTraceIO(traceInput, nil) })
	debugLog(fmt.Sprintf("started trace %s for %s", traceID, key))
	return newTraceState(traceID, root, propagated)
}

// Child observation opened under the root's propagated trace attributes
func startChildObservation(state *traceState, name, asType string, input, metadata, model, modelParameters any) *observation {
	var propagated *pyjson.Object
	if state.propagated != nil && state.propagated.Len() > 0 {
		propagated = state.propagated
	}
	if !truthy(metadata) {
		metadata = pyjson.NewObject()
	}
	return state.root.startChild(name, asType, obsFields{input: input, metadata: metadata, model: model, modelParameters: modelParameters}, propagated)
}

// Final fields of an observation, empty values left out
type endArgs struct {
	output, metadata, usage, cost any
	level, statusMessage          string
	model                         any
}

// Applies the final fields and ends an observation, never raising
func endObservation(o *observation, a endArgs) {
	if o == nil {
		return
	}
	failsafe("end observation", func() {
		f := obsFields{}
		given := false
		if a.output != nil {
			f.output, given = a.output, true
		}
		if truthy(a.metadata) {
			f.metadata, given = a.metadata, true
		}
		if truthy(a.usage) {
			f.usageDetails, given = a.usage, true
		}
		if truthy(a.cost) {
			f.costDetails, given = a.cost, true
		}
		if a.level != "" {
			f.level, given = a.level, true
		}
		if a.statusMessage != "" {
			f.statusMessage, given = a.statusMessage, true
		}
		if truthy(a.model) {
			f.model, given = a.model, true
		}
		if given {
			o.update(f)
		}
		o.end()
	})
}

// Ends every still open child, unanswered calls marked as errors, subagents included when asked
func endChildren(state *traceState, includeSubagents bool) {
	var pending []*observation
	for _, queue := range state.pendingTools.values() {
		pending = append(pending, queue...)
	}
	open := append(append(state.generations.values(), state.tools.values()...), pending...)
	for _, o := range open {
		endObservation(o, endArgs{level: "ERROR", statusMessage: "Turn ended before this call's response/result was recorded."})
	}
	if includeSubagents {
		for _, o := range state.subagents.values() {
			endObservation(o, endArgs{})
		}
	}
}

// Ends the root span, never raising
func endRoot(state *traceState, label string) {
	failsafe(label, func() { state.root.end() })
}

// Ends every open root and flushes each settled client, the process exit finalizer
func finalizeAllTraces() {
	var keys []string
	var states []*traceState
	withState(func() {
		keys, states = traceStates.keyList(), traceStates.values()
		traceStates.clear()
	})
	for i, state := range states {
		label := "atexit finalize for " + keys[i]
		failsafe(label, func() {
			endChildren(state, true)
			endRoot(state, label)
		})
	}
	if len(states) > 0 {
		for _, c := range settledClients() {
			flushClient(c)
		}
	}
}

// Flushes a client, swallowing any failure
func flushClient(c *lfClient) {
	if c == nil {
		return
	}
	func() {
		defer func() { _ = recover() }()
		c.flush()
	}()
}

// Ends a turn's spans locally and lets the exporter deliver, the final output carrying the turn's tool calls
func finishTrace(ctx context.Context, key string, output any) {
	client := getLangfuse(ctx)
	var state *traceState
	withState(func() {
		if client != nil {
			state, _ = traceStates.pop(key)
		}
		if i := strings.LastIndex(key, ":turn:"); i >= 0 {
			senderByTurn.pop(key[i+len(":turn:"):])
		}
	})
	if state == nil {
		return
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				debugLog(fmt.Sprintf("finish trace failed: %v", r))
				func() {
					defer func() { _ = recover() }()
					state.root.end()
				}()
			}
		}()
		endChildren(state, false)
		finalOutput := output
		var calls []any
		withState(func() { calls = append([]any(nil), state.turnToolCalls...) })
		if len(calls) > 0 {
			var out *pyjson.Object
			if items, ok := dictItems(output); ok {
				out = pyjson.NewObject()
				for _, it := range items {
					out.Set(it.key, it.val)
				}
			} else {
				out = obj("content", output)
			}
			out.Set("tool_calls", calls)
			finalOutput = out
		}
		if finalOutput != nil {
			failsafe("set_trace_io(output)", func() { state.root.setTraceIO(nil, finalOutput) })
			failsafe("root update(output)", func() { state.root.update(obsFields{output: finalOutput}) })
		}
		endRoot(state, "root end()")
	}()
}

// Request scoped generation key of an API call count
func requestKey(apiCallCount any) string {
	if !truthy(apiCallCount) {
		return "0"
	}
	return pyStr(apiCallCount)
}

// Client and trace key of a hook, a nil client when tracing is unavailable
func clientAndKey(ctx context.Context, taskID, sessionID, turnID, apiRequestID string) (*lfClient, string) {
	c := getLangfuse(ctx)
	if c == nil {
		return nil, ""
	}
	return c, traceKey(taskID, sessionID, turnID, apiRequestID)
}

// API duration metadata rounded to milliseconds, empty without a positive duration
func durationMeta(apiDuration any) *pyjson.Object {
	f, ok := toFloat(apiDuration)
	if !truthy(apiDuration) || !ok || f <= 0 {
		return pyjson.NewObject()
	}
	if b, isBool := apiDuration.(bool); isBool && b {
		return obj("api_duration_s", 1)
	}
	if isStrictInt(apiDuration) {
		return obj("api_duration_s", apiDuration)
	}
	return obj("api_duration_s", pyint.Round(f, 3))
}

// Detaches the open generation of one API call with the model it started with
func popGeneration(key string, apiCallCount any) (*traceState, *observation, string) {
	var state *traceState
	var gen *observation
	var model string
	withState(func() {
		s, ok := traceStates.get(key)
		if !ok {
			return
		}
		state = s
		rk := requestKey(apiCallCount)
		gen, _ = s.generations.pop(rk)
		model, _ = s.generationModels.pop(rk)
	})
	return state, gen, model
}

// Live state of a key, a new root opened first when absent after evicting the least recently updated, the caller holding the lock
func getOrStartStateLocked(ctx context.Context, c *lfClient, key string, a rootArgs) *traceState {
	state, ok := traceStates.get(key)
	if !ok {
		state = startRootTrace(ctx, c, key, a)
		if over := traceStates.len() - (maxTraceState - 1); over > 0 {
			keys, states := traceStates.keyList(), traceStates.values()
			order := make([]int, len(keys))
			for i := range order {
				order[i] = i
			}
			sort.SliceStable(order, func(i, j int) bool { return states[order[i]].lastUpdated.Before(states[order[j]].lastUpdated) })
			for _, idx := range order[:over] {
				traceStates.pop(keys[idx])
				endRoot(states[idx], "evict stale trace")
			}
		}
		traceStates.set(key, state)
	}
	state.lastUpdated = time.Now()
	return state
}
