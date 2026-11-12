package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

const recentSubagentsCap = 200

// Insertion ordered subagent records keyed by subagent id, a reassigned id keeping its place
type subagentRecords struct {
	keys []string
	m    map[string]map[string]any
}

func (s *subagentRecords) get(id string) map[string]any { return s.m[id] }

func (s *subagentRecords) set(id string, record map[string]any) {
	if s.m == nil {
		s.m = map[string]map[string]any{}
	}
	if _, ok := s.m[id]; !ok {
		s.keys = append(s.keys, id)
	}
	s.m[id] = record
}

func (s *subagentRecords) pop(id string) {
	if _, ok := s.m[id]; !ok {
		return
	}
	delete(s.m, id)
	for i, k := range s.keys {
		if k == id {
			s.keys = append(s.keys[:i:i], s.keys[i+1:]...)
			return
		}
	}
}

func (s *subagentRecords) values() []map[string]any {
	out := make([]map[string]any, 0, len(s.keys))
	for _, k := range s.keys {
		out = append(out, s.m[k])
	}
	return out
}

var (
	spawnPauseMu      sync.Mutex
	spawnPaused       bool
	activeSubagentsMu sync.Mutex
	activeSubagents   subagentRecords
	recentSubagents   subagentRecords
)

// SessionSteerAuthority returns the request transport and live session record owning a gateway session, installed by the binary
var SessionSteerAuthority func(ctx context.Context, sessionID string) (transport, record any)

// Hard interrupt of a running child, swapped by tests that observe the stop path
var subagentHardInterrupt = func(agent any, reason string) bool { return RequestHardInterruptOf(agent, &reason, nil) }

// Live parent chain link of a delegated agent, nil when the parent is gone or never stamped
type delegateParented interface{ delegateParentAgent() any }

func (a *AIAgent) delegateParentAgent() any {
	if p := a.delegateParentRef.Value(); p != nil {
		return p
	}
	return nil
}

// Live transcript path of an agent, nil when none was attached
type liveTranscriptHolder interface{ liveTranscript() any }

func (a *AIAgent) liveTranscript() any {
	if a.liveTranscriptPath == "" {
		return nil
	}
	return a.liveTranscriptPath
}

// Registry key of a subagent id value, empty for a falsy id
func subagentKey(v any) string {
	if !pyTruthy(v) {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return pyrepr.Str(v)
}

// GetSubagentAttribution returns the subagent id, goal and delegation id of a live or recently finished child task
func GetSubagentAttribution(taskID string) map[string]any {
	if taskID == "" {
		return nil
	}
	activeSubagentsMu.Lock()
	defer activeSubagentsMu.Unlock()
	record := activeSubagents.get(taskID)
	if len(record) == 0 {
		record = recentSubagents.get(taskID)
	}
	if record == nil {
		return nil
	}
	return map[string]any{"subagent_id": taskID, "goal": record["goal"], "delegation_id": record["delegation_id"]}
}

// SetSpawnPaused blocks or unblocks new delegate_task spawns globally and returns the new state
func SetSpawnPaused(paused bool) bool {
	spawnPauseMu.Lock()
	defer spawnPauseMu.Unlock()
	spawnPaused = paused
	return spawnPaused
}

func isSpawnPaused() bool {
	spawnPauseMu.Lock()
	defer spawnPauseMu.Unlock()
	return spawnPaused
}

// IsSpawnPaused reports whether new delegate_task spawns are globally blocked
func IsSpawnPaused() bool { return isSpawnPaused() }

// Adds a live child record that accepts steering unless the record says otherwise
func registerSubagent(record map[string]any) {
	sid := subagentKey(record["subagent_id"])
	if sid == "" {
		return
	}
	if _, ok := record["accepting_steer"]; !ok {
		record["accepting_steer"] = true
	}
	activeSubagentsMu.Lock()
	defer activeSubagentsMu.Unlock()
	activeSubagents.set(sid, record)
}

// Drops the live record of the exact agent when given and keeps a bounded attribution entry
func unregisterSubagent(subagentID string, agent any) {
	activeSubagentsMu.Lock()
	defer activeSubagentsMu.Unlock()
	record := activeSubagents.get(subagentID)
	if record == nil || !(agent == nil || pyrepr.Is(record["agent"], agent)) {
		return
	}
	activeSubagents.pop(subagentID)
	sid := subagentKey(record["subagent_id"])
	if sid == "" {
		return
	}
	recentSubagents.set(sid, map[string]any{
		"goal": record["goal"], "delegation_id": record["delegation_id"], "owner_agent_session_id": record["owner_agent_session_id"],
	})
	for len(recentSubagents.keys) > recentSubagentsCap {
		recentSubagents.pop(recentSubagents.keys[0])
	}
}

// Closes steer acceptance of the exact agent and returns its undelivered steer text, empty when none
func closeSubagentSteering(subagentID string, agent any) string {
	activeSubagentsMu.Lock()
	defer activeSubagentsMu.Unlock()
	record := activeSubagents.get(subagentID)
	if record == nil || !pyrepr.Is(record["agent"], agent) {
		return ""
	}
	record["accepting_steer"] = false
	drainer, ok := agent.(interface{ drainPendingSteer() string })
	if !ok {
		return ""
	}
	pending := drainer.drainPendingSteer()
	if pystr.Strip(pending) == "" {
		return ""
	}
	return pending
}

// InterruptSubagent asks one running child to stop at its next iteration boundary, true when it was found
func InterruptSubagent(subagentID string) bool {
	activeSubagentsMu.Lock()
	record := activeSubagents.get(subagentID)
	var agent any
	if len(record) > 0 {
		agent = record["agent"]
	}
	activeSubagentsMu.Unlock()
	if agent == nil {
		return false
	}
	return subagentHardInterrupt(agent, fmt.Sprintf("Interrupted via TUI (%s)", subagentID))
}

// Whether a transport is attached to the live slot of the session that commissioned the child
func subagentTransportMatches(record map[string]any, transport any) bool {
	if record["owner_transport"] == nil {
		return false
	}
	var bound any
	switch owner := record["owner_session_record"].(type) {
	case map[string]any:
		bound = owner["transport"]
	case *tuigateway.Session:
		bound = owner.Get("transport")
	default:
		bound = record["owner_transport"]
	}
	if pyrepr.Is(bound, transport) {
		return true
	}
	fanout, ok := bound.(*tuigateway.FanoutTransport)
	if !ok {
		return false
	}
	member, ok := transport.(tuigateway.Transport)
	return ok && fanout.Contains(member)
}

// SteerSubagent queues steering text into a running child, an owner session id demanding exact gateway authority
func SteerSubagent(subagentID, text string, ownerSessionID *string, ownerTransport, ownerSessionRecord any) bool {
	if pystr.Strip(text) == "" {
		return false
	}
	activeSubagentsMu.Lock()
	defer activeSubagentsMu.Unlock()
	record := activeSubagents.get(subagentID)
	if len(record) == 0 || !pyTruthy(record["accepting_steer"]) {
		return false
	}
	if ownerSessionID != nil && (!pyrepr.Equal(record["owner_session_id"], *ownerSessionID) || ownerTransport == nil ||
		!subagentTransportMatches(record, ownerTransport) || ownerSessionRecord == nil ||
		!pyrepr.Is(record["owner_session_record"], ownerSessionRecord)) {
		return false
	}
	agent := record["agent"]
	if agent == nil {
		return false
	}
	steerable, ok := agent.(interface{ Steer(text string) bool })
	if !ok {
		delegateLog().Debug(fmt.Sprintf("steer_subagent(%s) failed: '%s' object has no attribute 'steer'", subagentID, pyrepr.TypeName(agent)))
		return false
	}
	return steerQuietly(subagentID, steerable, text)
}

// Steer call of a live child, a panic inside the child's steer degrading to false like the hermes except branch
func steerQuietly(subagentID string, steerable interface{ Steer(text string) bool }, text string) (accepted bool) {
	defer func() {
		if r := recover(); r != nil {
			if isUserInterruptPanic(r) {
				panic(r)
			}
			delegateLog().Debug(fmt.Sprintf("steer_subagent(%s) failed: %v", subagentID, r))
			accepted = false
		}
	}()
	return steerable.Steer(text)
}

// Request transport and live session record of a gateway session, both nil outside a gateway session
func captureGatewaySteerAuthority(ctx context.Context, ownerSessionID string) (any, any) {
	if ownerSessionID == "" {
		return nil, nil
	}
	return SessionSteerAuthority(ctx, ownerSessionID)
}

var privateRecordKeys = map[string]bool{
	"agent": true, "owner_session_id": true, "owner_transport": true, "owner_session_record": true, "accepting_steer": true,
}

// ListActiveSubagents copies the running subagent tree without its private authority fields
func ListActiveSubagents() []map[string]any {
	activeSubagentsMu.Lock()
	defer activeSubagentsMu.Unlock()
	out := []map[string]any{}
	for _, r := range activeSubagents.values() {
		entry := map[string]any{}
		for k, v := range r {
			if !privateRecordKeys[k] {
				entry[k] = v
			}
		}
		out = append(out, entry)
	}
	return out
}

// Whether the child sits below the parent in the spawn tree within the hop budget
func isDescendantOf(child, parent any, maxHops int) bool {
	if isNilAgent(child) || isNilAgent(parent) {
		return false
	}
	cur := child
	for range maxHops {
		var ancestor any
		if p, ok := cur.(delegateParented); ok {
			ancestor = p.delegateParentAgent()
		}
		if ancestor == nil {
			return false
		}
		if pyrepr.Is(ancestor, parent) {
			return true
		}
		cur = ancestor
	}
	return false
}

// Nil interface or nil agent pointer, the Python None of an agent slot
func isNilAgent(v any) bool {
	if v == nil {
		return true
	}
	a, ok := v.(*AIAgent)
	return ok && a == nil
}

// Compression lineage tip of a session id through the parent's session db, the id itself when unresolvable
func resolveSessionLineage(sessionID string, parent *AIAgent) string {
	if sessionID == "" || parent == nil || parent.sessionDB == nil {
		return sessionID
	}
	resolved, err := parent.sessionDB.ResolveResumeSessionID(sessionID)
	if err != nil || resolved == "" {
		return sessionID
	}
	return resolved
}

// Whether the parent's conversation owns the child record by identity chain or durable session lineage
func ownsSubagentRecord(record map[string]any, parent *AIAgent) bool {
	var parentAny any
	if parent != nil {
		parentAny = parent
	}
	if isDescendantOf(record["agent"], parentAny, 8) {
		return true
	}
	ownerSID := subagentKey(record["owner_agent_session_id"])
	parentSID := ""
	if parent != nil {
		parentSID = parent.sessionID
	}
	if ownerSID == "" || parentSID == "" {
		return false
	}
	if ownerSID == parentSID {
		return true
	}
	tip := resolveSessionLineage(ownerSID, parent)
	return tip == parentSID || tip == resolveSessionLineage(parentSID, parent)
}

// Seconds since a numeric start stamp rounded to a tenth, false for a non numeric stamp
func runningSeconds(started any) (float64, bool) {
	var start float64
	switch v := started.(type) {
	case float64:
		start = v
	case int:
		start = float64(v)
	case int64:
		start = float64(v)
	case bool:
		if v {
			start = 1
		}
	default:
		return 0, false
	}
	now := float64(time.Now().UnixNano()) / 1e9
	return pyint.Round(now-start, 1), true
}

// The list action payload of the children this parent owns
func listSubagentsPayload(parent *AIAgent) *pyjson.Object {
	activeSubagentsMu.Lock()
	records := activeSubagents.values()
	snapshots := make([]map[string]any, len(records))
	for i, r := range records {
		snapshot := make(map[string]any, len(r))
		for k, v := range r {
			snapshot[k] = v
		}
		snapshots[i] = snapshot
	}
	activeSubagentsMu.Unlock()
	entries := []any{}
	for _, r := range snapshots {
		if !ownsSubagentRecord(r, parent) {
			continue
		}
		entry := pyjson.NewObject()
		entry.Set("subagent_id", r["subagent_id"])
		entry.Set("parent_id", r["parent_id"])
		entry.Set("goal", r["goal"])
		entry.Set("model", r["model"])
		entry.Set("status", r["status"])
		if secs, ok := runningSeconds(r["started_at"]); ok {
			entry.Set("running_seconds", secs)
		} else {
			entry.Set("running_seconds", nil)
		}
		entry.Set("accepting_steer", pyTruthy(r["accepting_steer"]))
		var transcript any
		if h, ok := r["agent"].(liveTranscriptHolder); ok && !isNilAgent(r["agent"]) {
			transcript = h.liveTranscript()
		}
		entry.Set("live_transcript", transcript)
		entries = append(entries, entry)
	}
	payload := pyjson.NewObject()
	payload.Set("action", "list")
	payload.Set("count", len(entries))
	payload.Set("subagents", entries)
	if len(entries) == 0 {
		payload.Set("note", "No live subagents right now. Children that already finished "+
			"have delivered (or will deliver) their results as normal "+
			"completion messages — there is nothing to steer or stop.")
	}
	return payload
}

// Success status, success note and failure template of each targeted control action
var controlOutcomes = map[string][3]string{
	"stop": {
		"interrupt_requested",
		"The subagent stops at its next iteration boundary (in-flight tool calls are asked to cancel). Its " +
			"partial result still re-enters the conversation as a completion message — do not wait or poll.",
		"Could not interrupt '%s' — it likely finished in the last " +
			"moment. Its result arrives as a normal completion message.",
	},
	"steer": {
		"queued",
		"Steering text queued. The subagent sees it appended to its next tool result — the current tool call is " +
			"never cut. If the child finishes before a delivery boundary remains, the text is reported back as " +
			"missed_steer in its completion entry.",
		"Subagent '%s' is no longer accepting steering (finishing or " +
			"already finished). Its result arrives as a normal completion " +
			"message; re-delegate a follow-up task if more work is needed.",
	},
}

// Text of an optional string argument, an error for a value without a strip method
func controlText(v any) (string, error) {
	if !pyTruthy(v) {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("'%s' object has no attribute 'strip'", pyrepr.TypeName(v))
	}
	return s, nil
}

// Runs the list, steer or stop control action over the parent's own spawn tree
func handleDelegateControlAction(action string, subagentID, message any, parent *AIAgent) (string, error) {
	if action == "list" {
		return pyjson.Dumps(listSubagentsPayload(parent), pyjson.EnsureASCII(false))
	}
	rawID, err := controlText(subagentID)
	if err != nil {
		return "", err
	}
	sid := pystr.Strip(rawID)
	if sid == "" {
		return delegateToolError(fmt.Sprintf("action='%s' requires subagent_id (from the spawn dispatch response or action='list').", action)), nil
	}
	activeSubagentsMu.Lock()
	record := activeSubagents.get(sid)
	var snapshot map[string]any
	if record != nil {
		snapshot = make(map[string]any, len(record))
		for k, v := range record {
			snapshot[k] = v
		}
	}
	activeSubagentsMu.Unlock()
	if snapshot == nil || !ownsSubagentRecord(snapshot, parent) {
		return delegateToolError(fmt.Sprintf("No live subagent '%s' in this conversation's spawn tree. It "+
			"may have already finished (its result arrives as a normal "+
			"completion message). Use action='list' to see live children.", sid)), nil
	}
	text, err := controlText(message)
	if err != nil && action == "steer" {
		return "", err
	}
	if action == "steer" && pystr.Strip(text) == "" {
		return delegateToolError("action='steer' requires a non-empty 'message' describing the course correction."), nil
	}
	outcome, ok := controlOutcomes[action]
	if !ok {
		return delegateToolError(fmt.Sprintf("Unknown action '%s'. Use spawn, list, steer, or stop.", action)), nil
	}
	var done bool
	if action == "stop" {
		done = InterruptSubagent(sid)
	} else {
		done = SteerSubagent(sid, pystr.Strip(text), nil, nil, nil)
	}
	if done {
		payload := pyjson.NewObject()
		payload.Set("action", action)
		payload.Set("subagent_id", sid)
		payload.Set("status", outcome[0])
		payload.Set("note", outcome[1])
		return pyjson.Dumps(payload, pyjson.EnsureASCII(false))
	}
	return delegateToolError(fmt.Sprintf(outcome[2], sid)), nil
}

// Process handoff view of a live child and the parent it reports to
type subagentHandoff struct {
	child  *AIAgent
	parent *AIAgent
}

var handoffMu sync.Mutex

func (h *subagentHandoff) ParentOwner() string {
	if h.parent.currentTaskID != "" {
		return h.parent.currentTaskID
	}
	return h.parent.sessionID
}

func (h *subagentHandoff) ParentSessionID() string { return h.parent.sessionID }

func (h *subagentHandoff) HandedOff() []map[string]any {
	handoffMu.Lock()
	defer handoffMu.Unlock()
	return append([]map[string]any(nil), h.child.handedOffProcesses...)
}

func (h *subagentHandoff) AppendHandoff(entry map[string]any) {
	handoffMu.Lock()
	defer handoffMu.Unlock()
	h.child.handedOffProcesses = append(h.child.handedOffProcesses, entry)
}

// Live child of a task with a live parent for process handoff, nil otherwise
func resolveSubagentHandoff(taskID string) tools.SubagentHandoff {
	activeSubagentsMu.Lock()
	record := activeSubagents.get(taskID)
	var agent any
	if record != nil {
		agent = record["agent"]
	}
	activeSubagentsMu.Unlock()
	child, _ := agent.(*AIAgent)
	if child == nil {
		return nil
	}
	parent := child.delegateParentRef.Value()
	if parent == nil {
		return nil
	}
	return &subagentHandoff{child: child, parent: parent}
}

// Installs the registry lookups the process registry reads for attribution and handoff
func init() {
	tools.SubagentAttribution = GetSubagentAttribution
	tools.SubagentHandoffResolver = resolveSubagentHandoff
}
