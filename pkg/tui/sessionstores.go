package tui

import (
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"reflect"
	"slices"
	"strings"
	"time"
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

// Rows the owner just moved from queued to started whose bubble nobody painted
func (s *tuiStore) newlyStartedRows(previous, incoming *tuigateway.SessionInfo) []pendingRow {
	before, after := pendingRowsOf(previous), pendingRowsOf(incoming)
	if before == nil || after == nil {
		return nil
	}
	wasQueued := map[string]bool{}
	for _, row := range before {
		if row.Status == "queued" {
			wasQueued[row.AdmissionID] = true
		}
	}
	out := []pendingRow{}
	for _, row := range after {
		if row.Status == "started" && wasQueued[row.AdmissionID] && !(row.InputID != "" && s.bubblesShown[row.InputID]) {
			out = append(out, row)
		}
	}
	return out
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
		partial, _ := tuigateway.VoicePartialEvent.Payload(ev.Payload)
		s.voicePartial = partial.Text
		return true
	case "voice.transcript":
		s.voicePartial = ""
		return false
	case "voice.status":
		status, _ := tuigateway.VoiceStatusEvent.Payload(ev.Payload)
		state := status.State
		voice.setVoiceRecording(state == "listening")
		voice.setVoiceProcessing(state == "transcribing")
		if state != "listening" && state != "transcribing" {
			s.voicePartial = ""
		}
		return true
	}
	return false
}

// Live connection operation the card shows
type connectionOperation struct {
	OpID    string
	Seq     int64
	Targets []tuigateway.ConnectionOperationTarget
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

// Word of a target state on the card and in the settled lines
func connectionStateWord(state tuigateway.ConnectionTargetState) string {
	switch state {
	case "initiated":
		return tr("connection.state.initiated")
	case "connected":
		return tr("connection.state.connected")
	case "skipped":
		return tr("connection.state.skipped")
	case "failed":
		return tr("connection.state.failed")
	case "not_connected":
		return tr("connection.state.notConnected")
	}
	return tr("connection.state.pending")
}

// Transcript line of a settled target and its outcome
func settledLine(target tuigateway.ConnectionOperationTarget) string {
	state := target.State
	if state != "connected" && state != "skipped" {
		state = "not_connected"
	}
	return connectionTargetName(target) + ": " + connectionStateWord(state)
}

// Name of a target as the card shows it
func connectionTargetName(target tuigateway.ConnectionOperationTarget) string {
	return firstText(strings.TrimFunc(val(target.Display), isJSSpace), target.Name)
}

// Shows the card of a connection request unless its operation already settled
func (s *tuiStore) applyConnectionRequest(p tuigateway.ConnectionRequestPayload) {
	if slices.Contains(s.settledOperations, p.OpID) {
		return
	}
	current := s.connectionOp
	if !(current != nil && current.OpID == p.OpID && p.Seq <= current.Seq) {
		s.connectionOp = &connectionOperation{OpID: p.OpID, Seq: p.Seq, Targets: p.Targets}
	}
	s.overlay.Connection = &connectionOverlay{OpID: p.OpID}
}

// Applies an operation snapshot, the settled outcome lines when it closes the card
func (s *tuiStore) applyConnectionSnapshot(p tuigateway.ConnectionOperationSnapshot) []string {
	current := s.connectionOp
	shown := current != nil && current.OpID == p.OpID
	if p.Settled {
		s.settledOperations = rememberOperation(s.settledOperations, p.OpID)
		if !shown {
			return nil
		}
		s.clearConnectionOperation()
		lines := make([]string, 0, len(p.Targets))
		for _, target := range p.Targets {
			lines = append(lines, settledLine(target))
		}
		return lines
	}
	if !shown || p.Seq <= current.Seq {
		return nil
	}
	s.connectionOp = &connectionOperation{OpID: p.OpID, Seq: p.Seq, Targets: p.Targets}
	s.overlay.Connection = &connectionOverlay{OpID: p.OpID}
	return nil
}

// Applies a connection update, the settled outcome lines when it closes the card
func (s *tuiStore) applyConnectionUpdate(p tuigateway.ConnectionUpdatePayload) []string {
	return s.applyConnectionSnapshot(p.ConnectionOperationSnapshot)
}

// Closes the connection card
func (s *tuiStore) clearConnectionOperation() {
	s.connectionOp = nil
	s.overlay.Connection = nil
}

// Goal and loop state of the session it belongs to
type controlState struct {
	Goal *tuigateway.GoalSnapshot
	Loop *tuigateway.LoopSnapshot
	Sid  string
}

// Goal and loop bar state from one session control snapshot, replaced only when its session or content changed
func (s *tuiStore) applyControlSnapshot(sid string, control tuigateway.SessionControlSnapshot) {
	next := controlState{Goal: control.Goal, Loop: control.Loop, Sid: sid}
	if !reflect.DeepEqual(s.controls, next) {
		s.controls = next
	}
}
