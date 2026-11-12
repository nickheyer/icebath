package app

import (
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Outcomes of a queued admission that never ran
var gatewayNeverRan = map[string]bool{"cancelled": true, "rejected": true}

// Gateway session store of the CLI, nil when none is open
func (c *CLI) gatewayStore() *gatewaycli.GatewaySessionStore { return c.sessionDB }

// Async result card of a message.start frame, nil when the frame carries none
func notificationCard(params map[string]any) map[string]any {
	payload, _ := params["payload"].(map[string]any)
	if payload == nil {
		return nil
	}
	display, displayOK := payload["display_text"].(string)
	text, textOK := payload["text"].(string)
	if !pyrepr.Truthy(payload["display_kind"]) || !displayOK || pystr.Strip(display) == "" || !textOK || text == "" {
		return nil
	}
	return payload
}

// Idle tick queueing the notification or queued turn the gateway admitted for this session
func queueAdmittedTurn(c *CLI, store *gatewaycli.GatewaySessionStore) {
	connection, sessionID := store.Connection, c.sessionID
	state := connection.State(sessionID)
	if adoptionWaiting(c, state) {
		return
	}
	state.Lock()
	state.AdmittedTurn = nil
	state.Unlock()
	events := connection.Events
	for {
		frame, ok := events.GetNowait()
		if !ok {
			recoverQueued(c, store)
			return
		}
		if _, isErr := frame.(error); isErr {
			if gatewaycli.LostBefore(frame, connection.Epoch()) {
				continue
			}
			events.PutNowait(frame)
			return
		}
		params := frameParams(frame)
		if params == nil || params["session_id"] != sessionID {
			continue
		}
		if params["type"] == "review.summary" {
			if line := reviewSummaryLine(params["payload"]); line != "" {
				c.agentStatusPrint(line)
			}
			continue
		}
		if params["type"] != "message.start" || !pyrepr.Truthy(params["admission_id"]) {
			continue
		}
		admission := pyrepr.Str(params["admission_id"])
		state.Lock()
		queuedText, queued := state.QueuedInputs.Get(admission)
		state.Unlock()
		if queued && queuedText != nil {
			adoptQueuedTurn(c, state, map[string]any{"admission_id": admission, "execution_generation": params["execution_generation"]}, pyrepr.Str(queuedText))
			return
		}
		card := notificationCard(params)
		if card == nil {
			continue
		}
		state.Lock()
		state.AdmittedTurn = map[string]any{"admission_id": admission, "text": card["text"], "execution_generation": params["execution_generation"]}
		state.Unlock()
		category := "result"
		if pyrepr.Truthy(card["notification_category"]) {
			category = pyrepr.Str(card["notification_category"])
		}
		c.pendingInput.putFirst(tools.NewTimelineNotification(pyrepr.Str(card["text"]), pyrepr.Str(card["display_text"]),
			pyrepr.Str(card["display_kind"]), category))
		return
	}
}

// Adopts a started queued admission and puts its literal prompt ahead of the input queue
func adoptQueuedTurn(c *CLI, state *gatewaycli.SessionState, adopted map[string]any, text string) {
	admission := pyrepr.Str(adopted["admission_id"])
	state.Lock()
	delete(state.Missed, admission)
	state.Adopted[admission] = adopted
	state.Unlock()
	c.pendingInput.putFirst(newSeededQueryMessage(text, nil, admission))
}

// Whether an adopted item still waits in the input queue, forgetting adoptions a queue clear dropped
func adoptionWaiting(c *CLI, state *gatewaycli.SessionState) bool {
	state.Lock()
	empty := len(state.Adopted) == 0
	armed := state.ArmedAdmission
	state.Unlock()
	if empty || c.pendingInput == nil {
		return false
	}
	waiting := map[string]bool{armed: true}
	for _, item := range c.pendingInput.snapshot() {
		if seeded, ok := item.(*seededQueryMessage); ok {
			waiting[seeded.AdmissionID] = true
		}
	}
	state.Lock()
	defer state.Unlock()
	for admission := range state.Adopted {
		if !waiting[admission] {
			delete(state.Adopted, admission)
		}
	}
	return len(state.Adopted) > 0
}

// Recovers queued admissions the owner started or settled without this viewer painting their turn
func recoverQueued(c *CLI, store *gatewaycli.GatewaySessionStore) {
	state := store.Connection.State(c.sessionID)
	state.Lock()
	if state.QueuedInputs.Len() == 0 || (state.RecoveredSeq == state.PendingSeq && len(state.Missed) == 0) {
		state.Unlock()
		return
	}
	state.RecoveredSeq = state.PendingSeq
	listed := map[any]any{}
	for _, raw := range state.Pending {
		if row, isDict := raw.(map[string]any); isDict {
			listed[row["admission_id"]] = row["status"]
		}
	}
	type queuedInput struct {
		admission string
		text      any
	}
	var inputs []queuedInput
	for _, admission := range state.QueuedInputs.Keys {
		text, _ := state.QueuedInputs.Get(admission)
		inputs = append(inputs, queuedInput{admission, text})
	}
	state.Unlock()
	for _, input := range inputs {
		state.Lock()
		_, adopted := state.Adopted[input.admission]
		missed := state.Missed[input.admission]
		state.Unlock()
		if adopted || (!missed && listed[input.admission] == "queued") {
			continue
		}
		raw, err := store.Call(c.ctx, "prompt.receipt", map[string]any{"session_id": c.sessionID, "admission_id": input.admission, "include_result": true})
		if err != nil {
			if !isGatewayClientError(err) {
				panic(err)
			}
			state.Lock()
			state.RecoveredSeq = -1
			state.Unlock()
			continue
		}
		receipt, _ := pyjson.Plain(raw).(map[string]any)
		status := receipt["status"]
		if status == "queued" {
			state.Lock()
			delete(state.Missed, input.admission)
			state.Unlock()
			continue
		}
		if outcome, _ := receipt["outcome"].(string); status == "terminal" && gatewayNeverRan[outcome] {
			state.Lock()
			state.QueuedInputs.Delete(input.admission)
			delete(state.Missed, input.admission)
			state.Unlock()
			continue
		}
		if status != "started" && status != "terminal" {
			continue
		}
		adoptedRow := map[string]any{"admission_id": input.admission, "execution_generation": receipt["execution_generation"], "recovered": true}
		if status == "terminal" {
			adoptedRow["receipt"] = receipt
		}
		adoptQueuedTurn(c, state, adoptedRow, pyrepr.Str(input.text))
		return
	}
}

// Remembers a queued admission's start dropped outside its turn so the idle tick paints it from its receipt
func missedStart(state *gatewaycli.SessionState, params map[string]any) {
	if params["type"] != "message.start" {
		return
	}
	admission, isStr := params["admission_id"].(string)
	if !isStr {
		return
	}
	state.Lock()
	defer state.Unlock()
	if _, queued := state.QueuedInputs.Get(admission); queued {
		state.Missed[admission] = true
	}
}

// Arms the next run to pump exactly the adopted queued admission the input loop took
func armAdopted(c *CLI, item any) {
	seeded, ok := item.(*seededQueryMessage)
	if !ok || seeded.AdmissionID == "" {
		return
	}
	if store := c.gatewayStore(); store != nil {
		state := store.Connection.State(c.sessionID)
		state.Lock()
		state.ArmedAdmission = seeded.AdmissionID
		state.Unlock()
	}
}

// Admitted turn a run replays instead of submitting, nil for any other turn
func takeAdmittedTurn(a *GatewayAgent, text any) map[string]any {
	state := a.state()
	state.Lock()
	defer state.Unlock()
	armed := state.ArmedAdmission
	state.ArmedAdmission = ""
	if armed != "" {
		if adopted, ok := state.Adopted[armed]; ok {
			delete(state.Adopted, armed)
			state.QueuedInputs.Delete(armed)
			row, _ := adopted.(map[string]any)
			return row
		}
	}
	adopted := state.AdmittedTurn
	state.AdmittedTurn = nil
	if adopted == nil || !pyjson.Equal(adopted["text"], text) {
		return nil
	}
	return adopted
}

// Puts an item ahead of everything queued, the gateway running that turn now, and wakes a blocked getter
func (q *inputQueue) putFirst(item any) {
	q.mu.Lock()
	q.items = append([]any{item}, q.items...)
	notify := q.waker()
	q.mu.Unlock()
	select {
	case notify <- struct{}{}:
	default:
	}
}
