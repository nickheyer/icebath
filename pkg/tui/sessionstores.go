package tui

import (
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	petFlashDefault = 1600 * time.Millisecond
	operationMemory = 64
)

// Delegation caps of the last delegation.status answer, nil until fetched
type delegationState struct {
	MaxConcurrentChildren *float64
	MaxSpawnDepth         *float64
	Paused                bool
	UpdatedAt             *float64
}

// Merges a delegation.status answer, fields of the wrong type left as they were
func (s *tuiStore) applyDelegationStatus(r map[string]any) {
	if r == nil {
		return
	}
	now := s.nowMillis()
	s.delegation.UpdatedAt = &now
	if n, isNum := r["max_spawn_depth"].(float64); isNum {
		s.delegation.MaxSpawnDepth = &n
	}
	if n, isNum := r["max_concurrent_children"].(float64); isNum {
		s.delegation.MaxConcurrentChildren = &n
	}
	if b, isBool := r["paused"].(bool); isBool {
		s.delegation.Paused = b
	}
}

// Reaction beat the pet shows until its deadline
type petFlash struct {
	State string
	Until time.Time
}

// Shows a pet reaction for a moment
func (s *tuiStore) flashPet(state string, d time.Duration) {
	s.petFlash = &petFlash{State: state, Until: s.now().Add(d)}
}

// Bumps the affection heart tick of the status bar
func (s *tuiStore) flashGoodVibes() { s.goodVibesTick++ }

// Inputs whose transcript bubble this client painted at submit time
func (s *tuiStore) markBubbleShown(inputID string) {
	if inputID == "" {
		return
	}
	if s.bubblesShown == nil {
		s.bubblesShown = map[string]bool{}
	}
	s.bubblesShown[inputID] = true
}

// Forgets every painted bubble
func (s *tuiStore) resetBubbleLedger() { s.bubblesShown = nil }

// Rows of a pending submissions list, nil for anything that is no list
func pendingRows(v any) []map[string]any {
	list, isList := v.([]any)
	if !isList {
		return nil
	}
	rows := make([]map[string]any, 0, len(list))
	for _, item := range list {
		row, _ := item.(map[string]any)
		rows = append(rows, row)
	}
	return rows
}

// Rows the owner just moved from queued to started whose bubble nobody painted
func (s *tuiStore) newlyStartedRows(previous, incoming any) []map[string]any {
	before, after := pendingRows(previous), pendingRows(incoming)
	if before == nil || after == nil {
		return nil
	}
	wasQueued := []any{}
	for _, row := range before {
		if row["status"] == "queued" {
			wasQueued = append(wasQueued, row["admission_id"])
		}
	}
	out := []map[string]any{}
	for _, row := range after {
		inputID, _ := row["input_id"].(string)
		queued := slices.ContainsFunc(wasQueued, func(id any) bool { return jsStrictEqual(id, row["admission_id"]) })
		if row["status"] == "started" && queued && !(inputID != "" && s.bubblesShown[inputID]) {
			out = append(out, row)
		}
	}
	return out
}

// Whether a pending row still waits on the owner's queue
func isServerQueued(row map[string]any) bool {
	return row["status"] == "queued" || row["status"] == "unknown"
}

// Voice setters the capture events drive
type voiceSetters interface {
	setVoiceRecording(on bool)
	setVoiceProcessing(on bool)
}

// Handles the capture state events, true when fully handled and false for a transcript the handler keeps
func (s *tuiStore) handleVoiceCapture(ev gatewayEventMsg, voice voiceSetters) bool {
	switch ev.Type {
	case "voice.partial":
		s.voicePartial = stringOr(ev.Payload["text"], "")
		return true
	case "voice.transcript":
		s.voicePartial = ""
		return false
	case "voice.status":
		state := stringOr(ev.Payload["state"], "")
		voice.setVoiceRecording(state == "listening")
		voice.setVoiceProcessing(state == "transcribing")
		if state != "listening" && state != "transcribing" {
			s.voicePartial = ""
		}
		return true
	}
	return false
}

var (
	processStart = time.Now()
	launchID     = uuid.NewString()
)

// Launch latency report, sent once per process and never for a dashboard chat tab
type startupLatency struct {
	reported bool
}

var startupReport startupLatency

// Whether this TUI runs inside a dashboard chat tab
func dashboardTUIMode() bool {
	return truthyToggle.MatchString(strings.TrimFunc(os.Getenv("ICEBATH_TUI_DASHBOARD"), isJSSpace))
}

// Params of the launch latency report on the first gateway.ready, false when it was sent or is not wanted
func (r *startupLatency) take() (map[string]any, bool) {
	if r.reported || dashboardTUIMode() {
		return nil, false
	}
	r.reported = true
	return map[string]any{"elapsed_ms": float64(time.Since(processStart).Milliseconds()), "launch_id": launchID, "surface": "tui"}, true
}

// Live connection operation the card shows
type connectionOperation struct {
	DeadlineAt float64
	OpID       string
	Seq        float64
	Targets    []map[string]any
	ToolCallID string
}

// Remembers an operation id, the oldest forgotten past the memory
func rememberOperation(ids []string, opID string) []string {
	if slices.Contains(ids, opID) {
		return ids
	}
	ids = append(ids, opID)
	if len(ids) > operationMemory {
		ids = ids[1:]
	}
	return ids
}

// Outcome word of a settled target
func outcomeWord(target map[string]any) string {
	switch target["state"] {
	case "connected":
		return "connected"
	case "skipped":
		return "skipped"
	}
	return "not connected"
}

// Transcript lines of a settled target, its outcome and the named account it connected
func settledLines(target map[string]any) []string {
	name, alias := jsString(target["name"]), ""
	if a, has := target["alias"]; has && truthyJS(a) {
		alias = jsString(a)
	}
	outcome := name
	if alias != "" {
		outcome += " (" + alias + ")"
	}
	outcome += ": " + outcomeWord(target)
	if target["state"] == "connected" && alias != "" {
		return []string{outcome, tr("connection.namedAccount", name, alias)}
	}
	return []string{outcome}
}

// Snapshot fields of a connection payload
func connectionTargets(v any) []map[string]any {
	rows := pendingRows(v)
	if rows == nil {
		return []map[string]any{}
	}
	return rows
}

// Shows the card of a connection request unless its operation already settled or was dismissed
func (s *tuiStore) applyConnectionRequest(p map[string]any) {
	opID := jsString(p["op_id"])
	if slices.Contains(s.settledOperations, opID) || slices.Contains(s.dismissedOperations, opID) {
		return
	}
	seq := jsNumberOf(p["seq"])
	current := s.connectionOp
	if !(current != nil && current.OpID == opID && seq <= current.Seq) {
		toolCallID, _ := p["tool_call_id"].(string)
		s.connectionOp = &connectionOperation{DeadlineAt: jsNumberOf(p["deadline_at"]), OpID: opID, Seq: seq, Targets: connectionTargets(p["targets"]), ToolCallID: toolCallID}
	}
	s.overlay.Connection = &connectionOverlay{OpID: opID}
}

// Applies a connection update, the settled outcome lines when it closes the card
func (s *tuiStore) applyConnectionUpdate(p map[string]any) []string {
	opID := jsString(p["op_id"])
	current := s.connectionOp
	shown := current != nil && current.OpID == opID
	if truthyJS(p["settled"]) {
		s.settledOperations = rememberOperation(s.settledOperations, opID)
		if !shown && !slices.Contains(s.dismissedOperations, opID) {
			return nil
		}
		if shown {
			s.clearConnectionOperation()
		}
		lines := []string{}
		for _, target := range connectionTargets(p["targets"]) {
			lines = append(lines, settledLines(target)...)
		}
		return lines
	}
	seq := jsNumberOf(p["seq"])
	if !shown || seq <= current.Seq {
		return nil
	}
	next := *current
	next.DeadlineAt, next.Seq, next.Targets = jsNumberOf(p["deadline_at"]), seq, connectionTargets(p["targets"])
	s.connectionOp = &next
	s.overlay.Connection = &connectionOverlay{OpID: opID}
	return nil
}

// Closes the connection card
func (s *tuiStore) clearConnectionOperation() {
	s.connectionOp = nil
	s.overlay.Connection = nil
}

// Closes the card for good while its outcome still lands in the transcript
func (s *tuiStore) dismissConnectionOperation(opID string) {
	s.dismissedOperations = rememberOperation(s.dismissedOperations, opID)
	s.clearConnectionOperation()
}

// Goal or loop snapshot of the session it belongs to
type controlSnapshot struct {
	Value map[string]any
	Sid   string
}

// Replaces a bar snapshot only when its session or content changed
func applyBarSnapshot(prev *controlSnapshot, sid string, value map[string]any) {
	if prev.Sid != sid || !reflect.DeepEqual(prev.Value, value) {
		*prev = controlSnapshot{Value: value, Sid: sid}
	}
}

// Goal and loop bar state from one session.control payload
func (s *tuiStore) applyControlSnapshot(sid string, control map[string]any) {
	goal, _ := control["goal"].(map[string]any)
	loop, _ := control["loop"].(map[string]any)
	applyBarSnapshot(&s.goalSnapshot, sid, goal)
	applyBarSnapshot(&s.loopSnapshot, sid, loop)
}
