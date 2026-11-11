package gatewayapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Subagent lifecycle fields forwarded on the run stream, the free text ones redacted
var subagentEventKeys = []string{"goal", "task_count", "task_index", "subagent_id", "child_session_id", "delegation_id", "parent_id",
	"depth", "model", "tool_count", "status", "summary", "duration_seconds", "input_tokens",
	"output_tokens", "reasoning_tokens", "api_calls", "cost_usd", "files_read", "files_written", "output_tail"}

var subagentTextKeys = map[string]bool{"goal": true, "summary": true, "output_tail": true}

// Terminal usage wire keys and the agent counters they read
var usageFields = [][2]string{{"input_tokens", "session_prompt_tokens"}, {"output_tokens", "session_completion_tokens"},
	{"total_tokens", "session_total_tokens"}, {"cache_read_tokens", "session_cache_read_tokens"},
	{"cache_write_tokens", "session_cache_write_tokens"}}

const (
	toolCompletedPreviewMaxChars = 500
	runStreamWriteTimeout        = 5.0
	runStreamBacklogLimit        = 1000
	runStreamSubscriberLimit     = 256
	// Seconds before orphaned run streams are swept
	runStreamTTL = 300
	// Seconds terminal run status stays pollable
	runStatusTTL = 3600
)

// Tool call id field of a progress event when the agent supplied one
func callIDField(kw map[string]any) []any {
	if pyrepr.Truthy(kw["tool_call_id"]) {
		return []any{"tool_call_id", kw["tool_call_id"]}
	}
	return nil
}

// Wire fields of the fixed tool progress events
func fixedEventFields(eventType, tool, preview string, kw map[string]any) ([]any, bool) {
	switch eventType {
	case "tool.started":
		return append([]any{"tool", tool, "preview", nullableString(preview)}, callIDField(kw)...), true
	case "tool.completed":
		duration := 0.0
		if d, ok := kw["duration"]; ok {
			duration = toFloat(d)
		}
		isErr := any(false)
		if v, ok := kw["is_error"]; ok {
			isErr = v
		}
		return append([]any{"tool", tool, "duration", math.Round(duration*1000) / 1000, "error", isErr}, callIDField(kw)...), true
	case "reasoning.available":
		return []any{"text", preview}, true
	}
	return nil, false
}

// Nil for an empty string
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Float of a JSON number
func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	}
	return 0
}

// Bounded secret redacted result summary for the public run stream, redacted before truncation
func toolCompletedPreview(result any) string {
	if result == nil {
		return ""
	}
	text, ok := result.(string)
	if !ok {
		encoded, err := pyjson.Dumps(result, pyjson.EnsureASCII(false), pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil }))
		if err != nil {
			text = pyrepr.Str(result)
		} else {
			text = encoded
		}
	}
	preview := tools.RedactSensitiveTextWith(nil, text, tools.RedactOptions{Force: true})
	runes := []rune(preview)
	if len(runes) <= toolCompletedPreviewMaxChars {
		return preview
	}
	return string(runes[:toolCompletedPreviewMaxChars-3]) + "..."
}

// Sequenced event of a run stream, a nil event being the terminal marker
type runStreamItem struct {
	seq      int64
	event    *pyjson.Object
	overflow bool
}

// One run's retained events fanned out to SSE subscribers
type runStream struct {
	mu          sync.Mutex
	subscribers map[*runSubscriber]struct{}
	backlog     []runStreamItem
	nextSeq     int64
	terminal    bool
}

// Bounded subscriber queue of a run stream
type runSubscriber struct {
	items chan runStreamItem
}

func newRunStream() *runStream { return &runStream{subscribers: map[*runSubscriber]struct{}{}} }

// Retains and fans out an event, an overflowing subscriber detached with an overflow marker
func (s *runStream) putNowait(event *pyjson.Object) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminal {
		return
	}
	seq := s.nextSeq
	s.nextSeq++
	s.backlog = append(s.backlog, runStreamItem{seq: seq, event: event})
	if len(s.backlog) > runStreamBacklogLimit {
		s.backlog = s.backlog[len(s.backlog)-runStreamBacklogLimit:]
	}
	if event == nil {
		s.terminal = true
	}
	for sub := range s.subscribers {
		select {
		case sub.items <- runStreamItem{seq: seq, event: event}:
		default:
			delete(s.subscribers, sub)
			for len(sub.items) > 0 {
				<-sub.items
			}
			sub.items <- runStreamItem{seq: seq, overflow: true}
		}
	}
}

// Subscriber with the retained events after a sequence for replay
func (s *runStream) attach(lastSeq int64) (*runSubscriber, []runStreamItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var replay []runStreamItem
	for _, it := range s.backlog {
		if it.seq > lastSeq {
			replay = append(replay, it)
		}
	}
	sub := &runSubscriber{items: make(chan runStreamItem, runStreamSubscriberLimit+len(replay))}
	s.subscribers[sub] = struct{}{}
	return sub, replay
}

// Detaches a subscriber
func (s *runStream) detach(sub *runSubscriber) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.subscribers, sub)
}

type requestStateKey struct{}

// Mutable per request state, the Go form of values handlers store on the aiohttp request mapping
type requestState struct {
	mu                 sync.Mutex
	roomRetentionUntil float64
}

// Middleware binding a fresh per request state into the request context
func requestStateMiddleware(ctx context.Context, request *gateway.WebRequest, handler gateway.WebHandler) (*gateway.WebResponse, error) {
	return handler(context.WithValue(ctx, requestStateKey{}, &requestState{}), request)
}

// Per request state bound by the middleware, a detached one when none is bound
func requestStateOf(ctx context.Context) *requestState {
	if st, ok := ctx.Value(requestStateKey{}).(*requestState); ok {
		return st
	}
	return &requestState{}
}

// Records the grant's status or expiry horizon on the request
func rememberRoomRetention(ctx context.Context, claims *pyjson.Object) {
	st := requestStateOf(ctx)
	st.mu.Lock()
	st.roomRetentionUntil = toFloat(firstTruthy(claims.GetOr("status_expires_at", nil), claims.GetOr("expires_at", nil), int64(0)))
	st.mu.Unlock()
}

// Retention horizon remembered on the request, zero when none
func roomRetentionUntil(ctx context.Context) float64 {
	st := requestStateOf(ctx)
	st.mu.Lock()
	defer st.mu.Unlock()
	return max(0.0, st.roomRetentionUntil)
}

// One SSE run event payload with the envelope keys first
func runEvent(runID, name string, fields ...any) *pyjson.Object {
	out := pyjson.ObjectOf("event", name, "run_id", runID, "timestamp", apiNow())
	for i := 0; i+1 < len(fields); i += 2 {
		out.Set(fields[i].(string), fields[i+1])
	}
	return out
}

// Terminal run status of a turn result and the wire fields every terminal event carries
func terminalRunStatus(result map[string]any) (string, *pyjson.Object) {
	interrupted := pyrepr.Truthy(result["interrupted"])
	finished := !interrupted && !pyrepr.Truthy(result["failed"]) && !pyrepr.Truthy(result["partial"]) && result["completed"] != false
	status := "failed"
	switch {
	case interrupted:
		status = "cancelled"
	case finished:
		status = "completed"
	}
	fields := pyjson.ObjectOf("completed", finished, "partial", pyrepr.Truthy(result["partial"]), "interrupted", interrupted)
	if !finished && pyrepr.Truthy(result["turn_exit_reason"]) {
		fields.Set("turn_exit_reason", pyrepr.Str(result["turn_exit_reason"]))
	}
	if pyrepr.Truthy(result["pending_steer"]) {
		fields.Set("pending_steer", result["pending_steer"])
	}
	return status, fields
}

// Adapter owned durable and live structured run state, every map keyed by run id
type runState struct {
	runIdempotencyStore       *gateway.RunIdempotencyStore
	runOwnerPID               int
	runOwnerStarted           int64
	runIdempotencyIDs         map[string]bool
	stoppingRunIDs            map[string]bool
	shutdownInterruptedRunIDs map[string]bool
	runShutdownRequestedAt    *float64
	runAdmissionAliases       map[string]string
	runOwners                 map[string]string
	runStreams                map[string]*runStream
	runStreamsCreated         map[string]float64
	activeRunAgents           map[string]*agent.AIAgent
	activeRunAgentOrder       []string
	activeRunTasks            map[string]*agentTask
	runStatuses               map[string]*pyjson.Object
	runApprovalSessions       map[string]string
}

// Opens the durable run store and resets the live run maps
func (a *APIServerAdapter) initializeRunState(ctx context.Context) error {
	store, err := gateway.NewRunIdempotencyStore(ctx, "")
	if err != nil {
		return err
	}
	a.runIdempotencyStore = store
	a.runOwnerPID = os.Getpid()
	a.runOwnerStarted = 0
	if started, ok := pyint64(gateway.GetProcessStartTime(a.runOwnerPID)); ok {
		a.runOwnerStarted = started
	}
	a.runIdempotencyIDs, a.stoppingRunIDs, a.shutdownInterruptedRunIDs = map[string]bool{}, map[string]bool{}, map[string]bool{}
	a.runAdmissionAliases, a.runOwners = map[string]string{}, map[string]string{}
	a.runStreams, a.runStreamsCreated = map[string]*runStream{}, map[string]float64{}
	a.activeRunAgents, a.activeRunTasks = map[string]*agent.AIAgent{}, map[string]*agentTask{}
	a.runStatuses, a.runApprovalSessions = map[string]*pyjson.Object{}, map[string]string{}
	return nil
}

// Integer of a value when it is a whole number
func pyint64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	}
	return 0, false
}

// Registers a run's live agent, caller holds the adapter lock
func (a *APIServerAdapter) setActiveRunAgentLocked(runID string, ag *agent.AIAgent) {
	if _, exists := a.activeRunAgents[runID]; !exists {
		a.activeRunAgentOrder = append(a.activeRunAgentOrder, runID)
	}
	a.activeRunAgents[runID] = ag
}

// Drops a run's live agent, caller holds the adapter lock
func (a *APIServerAdapter) forgetActiveRunAgentLocked(runID string) {
	if _, exists := a.activeRunAgents[runID]; !exists {
		return
	}
	delete(a.activeRunAgents, runID)
	for i, id := range a.activeRunAgentOrder {
		if id == runID {
			a.activeRunAgentOrder = append(a.activeRunAgentOrder[:i], a.activeRunAgentOrder[i+1:]...)
			break
		}
	}
}

// Idempotency capability block of the capabilities payload
func (a *APIServerAdapter) idempotencyCapabilities() *pyjson.Object {
	return pyjson.ObjectOf("supported", true, "durable", a.runIdempotencyStore.Durable(),
		"retention_seconds", int64(gateway.RunIdempotencyRetentionSeconds))
}

// Retires the turn executor and closes the durable run store
func (a *APIServerAdapter) closeRunState(ctx context.Context) {
	if a.runIdempotencyStore == nil {
		return
	}
	if err := a.runIdempotencyStore.Close(); err != nil {
		apiLog().Debug(fmt.Sprintf("Failed to close run idempotency store for %s", a.Name()), "error", err)
	}
}

// Updates the pollable run status, persisting an idempotent run on a status, terminal or key field change
func (a *APIServerAdapter) setRunStatus(ctx context.Context, runID, status string, fields ...any) *pyjson.Object {
	a.mu.Lock()
	now := apiNow()
	current := a.runStatuses[runID]
	if current == nil {
		current = pyjson.NewObject()
	}
	previous := ""
	if v := current.GetOr("status", nil); pyrepr.Truthy(v) {
		previous = pyrepr.Str(v)
	}
	names := map[string]bool{}
	extra := pyjson.NewObject()
	for i := 0; i+1 < len(fields); i += 2 {
		k := fields[i].(string)
		names[k] = true
		extra.Set(k, fields[i+1])
	}
	current.Set("object", "hermes.run")
	current.Set("run_id", runID)
	current.Set("status", status)
	current.Set("updated_at", now)
	createdAt, hasCreated := extra.Pop("created_at")
	if !hasCreated {
		createdAt = now
	}
	setDefault(current, "created_at", createdAt)
	current.Update(extra)
	if a.runShutdownRequestedAt != nil && !gateway.APIRunTerminalStatus(status) {
		setDefault(current, "shutdown_requested_at", *a.runShutdownRequestedAt)
	}
	if status != "waiting_for_approval" {
		current.Delete("approval")
	}
	a.runStatuses[runID] = current
	persist := status != previous || gateway.APIRunTerminalStatus(status)
	for _, k := range []string{"output", "error", "usage", "pending_steer", "session_id", "shutdown_requested_at"} {
		if names[k] {
			persist = true
		}
	}
	idempotent := a.runIdempotencyIDs[runID]
	snapshot := current.Copy()
	a.mu.Unlock()
	if idempotent && persist {
		if err := a.runIdempotencyStore.UpdateStatus(ctx, runID, snapshot); err != nil {
			apiLog().Error(fmt.Sprintf("[api_server] failed to persist idempotent run status %s", runID), "error", err)
		}
	}
	return current
}

// Publishes the shutdown outcome on runs before cooperative interruption races teardown
func (a *APIServerAdapter) markShutdownInterruptedRuns(ctx context.Context, runIDs []string) {
	for _, runID := range runIDs {
		a.mu.Lock()
		a.shutdownInterruptedRunIDs[runID] = true
		a.mu.Unlock()
		a.setRunStatus(ctx, runID, "interrupted", "error", "Gateway shutdown interrupted the run.", "last_event", "run.interrupted")
	}
}

// Persists the gateway drain start on every nonterminal run and returns how many were marked
func (a *APIServerAdapter) markShutdownRequested(ctx context.Context) int {
	a.mu.Lock()
	if a.runShutdownRequestedAt == nil {
		now := apiNow()
		a.runShutdownRequestedAt = &now
	}
	at := *a.runShutdownRequestedAt
	type pending struct {
		runID, status string
	}
	var unmarked []pending
	for runID, current := range a.runStatuses {
		status := pyrepr.Str(current.GetOr("status", ""))
		if gateway.APIRunTerminalStatus(status) {
			continue
		}
		if _, ok := current.Get("shutdown_requested_at"); ok {
			continue
		}
		if !pyrepr.Truthy(current.GetOr("status", nil)) {
			status = "running"
		}
		unmarked = append(unmarked, pending{runID, status})
	}
	a.mu.Unlock()
	for _, p := range unmarked {
		a.setRunStatus(ctx, p.runID, p.status, "shutdown_requested_at", at)
	}
	return len(unmarked)
}

// Progress callback pushing structured run events to the run's SSE stream
func (a *APIServerAdapter) makeRunEventCallback(ctx context.Context, runID string) func(eventType, toolName, preview string, args map[string]any, kwargs map[string]any) {
	push := func(event *pyjson.Object) {
		a.mu.Lock()
		status := "running"
		if current := a.runStatuses[runID]; current != nil {
			if s, ok := current.Get("status"); ok {
				status = pyrepr.Str(s)
			}
		}
		stream := a.runStreams[runID]
		a.mu.Unlock()
		a.setRunStatus(ctx, runID, status, "last_event", event.GetOr("event", nil))
		if stream != nil {
			stream.putNowait(event)
		}
	}
	return func(eventType, toolName, preview string, args map[string]any, kwargs map[string]any) {
		if fields, ok := fixedEventFields(eventType, toolName, preview, kwargs); ok {
			event := runEvent(runID, eventType, fields...)
			if eventType == "tool.completed" {
				event.Set("preview", toolCompletedPreview(kwargs["result"]))
			}
			push(event)
			return
		}
		if eventType == "subagent.start" || eventType == "subagent.complete" {
			event := runEvent(runID, eventType)
			if preview != "" {
				event.Set("preview", tools.RedactSensitiveTextWith(nil, preview, tools.RedactOptions{Force: true}))
			}
			for _, key := range subagentEventKeys {
				value, ok := kwargs[key]
				if !ok || value == nil {
					continue
				}
				if text, isStr := value.(string); isStr && subagentTextKeys[key] {
					value = tools.RedactSensitiveTextWith(nil, text, tools.RedactOptions{Force: true})
				}
				event.Set(key, value)
			}
			push(event)
		}
	}
}

// Room grant permission a run control path asks for
func roomPermissionFor(request *gateway.WebRequest) string {
	path := request.RelURL.Path
	for _, pair := range [][2]string{{"/stop", "stop"}, {"/resolve-unknown", "stop"}, {"/approval", "approve"}, {"/clarify", "approve"}} {
		if strings.HasSuffix(path, pair[0]) {
			return pair[1]
		}
	}
	if request.Method == "GET" {
		return "status"
	}
	return "dispatch"
}

// Opaque auth and profile namespace of a request, never the bearer credential itself
func (a *APIServerAdapter) runIdempotencyScope(ctx context.Context, request *gateway.WebRequest) string {
	var parts []string
	if roomGrantToken(request) != "" {
		claims, err := a.roomGrantClaims(ctx, request, roomPermissionFor(request))
		if err != nil {
			panic(err)
		}
		rememberRoomRetention(ctx, claims)
		for _, k := range []string{"room_id", "home_install_id", "authority_gateway_id", "authority_epoch", "member_id", "target_install_id", "target_profile"} {
			v, _ := claims.Get(k)
			parts = append(parts, pyrepr.Str(v))
		}
	} else {
		profile := requestProfile(ctx)
		if profile == "" {
			profile = "default"
		}
		key := a.expectedAPIKey(ctx)
		if key == "" {
			key = "unauthenticated-test-listener"
		}
		parts = []string{profile, key}
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// Whether a runs dispatch carries a room grant token instead of the API key
func (a *APIServerAdapter) usesRoomRunAuth(request *gateway.WebRequest) bool {
	return strings.HasSuffix(request.RelURL.Path, "/v1/runs") && roomGrantToken(request) != ""
}

// Bearer auth, or the room grant's claims for the permission when a grant token is present
func (a *APIServerAdapter) checkRunAuth(ctx context.Context, request *gateway.WebRequest, permission string) *gateway.WebResponse {
	if roomGrantToken(request) == "" {
		return a.checkAuth(ctx, request)
	}
	if _, err := a.roomGrantClaims(ctx, request, permission); err != nil {
		return roomGrantErrorResponse(err)
	}
	return nil
}

// Whether the recorded owner pid is alive and the same process incarnation
func ownerAlive(ownerPID, ownerStarted int64) (alive bool) {
	defer func() {
		if recover() != nil {
			alive = false
		}
	}()
	if ownerPID <= 0 || !proc.Exists(int(ownerPID)) {
		return false
	}
	if ownerStarted == 0 {
		return true
	}
	current := gateway.GetProcessStartTime(int(ownerPID))
	if current == nil {
		current = int64(0)
	}
	match, err := gateway.StartTimeFingerprintsMatch(ownerStarted, current, 1)
	return err == nil && match
}
