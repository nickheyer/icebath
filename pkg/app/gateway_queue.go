package app

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// How long the input loop holds a local input behind this viewer's own queued admission at most
const ownQueueHoldSeconds = 2.0

// Refusals of a queued admission the owner already started
var queueStartedRefusals = map[string]bool{"stale_generation": true, "admission_started": true}

// Raised when a queued submit was sent but no answer came back, the owner possibly holding it
type QueueOutcomeUnknown struct{ Msg string }

func (e *QueueOutcomeUnknown) Error() string { return e.Msg }

func (e *QueueOutcomeUnknown) As(target any) bool {
	if t, ok := target.(**gateway.GatewayClientError); ok {
		*t = &gateway.GatewayClientError{Msg: e.Msg}
		return true
	}
	return false
}

func queueState(c *CLI, store *gatewaycli.GatewaySessionStore) *gatewaycli.SessionState {
	return store.Connection.State(c.sessionID)
}

// Admits text behind the running turn in the owner's FIFO under one submission id, returning its admission id
func submitQueued(c *CLI, store *gatewaycli.GatewaySessionStore, text string) (string, error) {
	submission := uuidHex()
	var raw any
	for attempt := 1; attempt <= 2; attempt++ {
		var err error
		raw, err = store.Call(c.ctx, "prompt.submit", map[string]any{"session_id": c.sessionID, "submission_id": submission, "text": text, "queued": true})
		if err == nil {
			break
		}
		var refused *gateway.GatewayRPCError
		if errors.As(err, &refused) || !isGatewayClientError(err) {
			return "", err
		}
		if err.Error() == gateway.GatewayNotSent && attempt == 1 {
			return "", err
		}
		if attempt == 2 {
			return "", &QueueOutcomeUnknown{Msg: err.Error()}
		}
		slog.Debug(fmt.Sprintf("prompt.submit answer lost (%s); asking again under the same identity", err.Error()))
	}
	receipt, _ := pyjson.Plain(raw).(map[string]any)
	admission := strOrEmpty(receipt["admission_id"])
	if admission == "" {
		return "", &gateway.GatewayClientError{Msg: "the gateway did not admit the queued prompt"}
	}
	state := queueState(c, store)
	state.Lock()
	state.QueuedInputs.Set(admission, text)
	state.QueuedAt[admission] = state.PendingSeq
	state.Unlock()
	return admission, nil
}

// Admission id and text of one queued row
type queuedRow struct {
	admission string
	text      string
}

// Rows still queued in the owner's FIFO in run order, this viewer's own read back by receipt
func queuedRows(c *CLI, store *gatewaycli.GatewaySessionStore) []queuedRow {
	state := queueState(c, store)
	type ordered struct {
		sequence int
		text     string
	}
	rows := map[string]ordered{}
	var order []string
	put := func(admission string, row ordered) {
		if _, has := rows[admission]; !has {
			order = append(order, admission)
		}
		rows[admission] = row
	}
	state.Lock()
	pending := append([]any{}, state.Pending...)
	inputs := state.QueuedInputs.Copy()
	state.Unlock()
	for _, raw := range pending {
		row, isDict := raw.(map[string]any)
		if isDict && row["status"] == "queued" && pyrepr.Truthy(row["admission_id"]) {
			seq, _ := pyIntValue(orZero(row["sequence"]))
			put(pyrepr.Str(row["admission_id"]), ordered{seq, strOrEmpty(row["text"])})
		}
	}
	for _, admission := range inputs.Keys {
		text, _ := inputs.Get(admission)
		raw, err := store.Call(c.ctx, "prompt.receipt", map[string]any{"session_id": c.sessionID, "admission_id": admission})
		if err != nil {
			if !isGatewayClientError(err) {
				panic(err)
			}
			slog.Debug(fmt.Sprintf("prompt.receipt %s failed: %s", admission, err.Error()))
			if _, has := rows[admission]; !has {
				put(admission, ordered{1 << 62, pyrepr.Str(text)})
			}
			continue
		}
		receipt, _ := pyjson.Plain(raw).(map[string]any)
		if receipt["status"] == "queued" {
			seq, _ := pyIntValue(orZero(receipt["sequence"]))
			put(admission, ordered{seq, pyrepr.Str(text)})
			continue
		}
		if _, has := rows[admission]; has {
			delete(rows, admission)
			order = slices.DeleteFunc(order, func(listed string) bool { return listed == admission })
		}
		state.Lock()
		state.Missed[admission] = true
		state.Unlock()
	}
	out := make([]queuedRow, 0, len(rows))
	for _, admission := range order {
		out = append(out, queuedRow{admission, rows[admission].text})
	}
	sort.SliceStable(out, func(i, j int) bool { return rows[out[i].admission].sequence < rows[out[j].admission].sequence })
	return out
}

// Texts the owner last published as queued plus this viewer's admissions submitted after that list
func publishedQueue(c *CLI, store *gatewaycli.GatewaySessionStore) []string {
	state := queueState(c, store)
	state.Lock()
	defer state.Unlock()
	for admission := range state.QueuedAt {
		if _, kept := state.QueuedInputs.Get(admission); !kept {
			delete(state.QueuedAt, admission)
		}
	}
	known := map[any]bool{}
	var listed []string
	for _, raw := range state.Pending {
		row, isDict := raw.(map[string]any)
		if !isDict {
			continue
		}
		known[row["admission_id"]] = true
		if row["status"] == "queued" {
			listed = append(listed, strOrEmpty(row["text"]))
		}
	}
	var fresh []string
	for _, admission := range state.QueuedInputs.Keys {
		text, _ := state.QueuedInputs.Get(admission)
		at, has := state.QueuedAt[admission]
		if !known[admission] && has && at >= state.PendingSeq {
			fresh = append(fresh, pyrepr.Str(text))
		}
	}
	return append(listed, fresh...)
}

// This viewer's queued admission at the head of the owner's queue not yet adopted, empty otherwise
func ownQueueHead(state *gatewaycli.SessionState) string {
	state.Lock()
	defer state.Unlock()
	var queued []map[string]any
	listed := map[any]bool{}
	for _, raw := range state.Pending {
		row, isDict := raw.(map[string]any)
		if !isDict {
			continue
		}
		if row["status"] == "unknown" {
			return ""
		}
		listed[row["admission_id"]] = true
		if row["status"] == "queued" && pyrepr.Truthy(row["admission_id"]) {
			queued = append(queued, row)
		}
	}
	head := ""
	if len(queued) > 0 {
		sort.SliceStable(queued, func(i, j int) bool {
			a, _ := pyIntValue(orZero(queued[i]["sequence"]))
			b, _ := pyIntValue(orZero(queued[j]["sequence"]))
			return a < b
		})
		head = pyrepr.Str(queued[0]["admission_id"])
	} else {
		for _, admission := range state.QueuedInputs.Keys {
			if at, has := state.QueuedAt[admission]; !listed[admission] && has && at >= state.PendingSeq {
				head = admission
				break
			}
		}
	}
	if _, queuedHere := state.QueuedInputs.Get(head); !queuedHere {
		return ""
	}
	if _, adopted := state.Adopted[head]; adopted {
		return ""
	}
	return head
}

// Local input held while this viewer's own queued admission is the owner's next turn
type ownQueueHold struct {
	item             any
	state            *gatewaycli.SessionState
	admission        string
	seq              int
	allowedIdleLists int
	idleLists        int
	deadline         time.Time
	taken            bool
}

// Hold for an input taken while this viewer's own queued admission is next, nil when nothing holds it
func beginOwnQueueHold(c *CLI, item any) *ownQueueHold {
	switch item.(type) {
	case *seededQueryMessage, tools.TimelineNotification:
		return nil
	}
	c.uiMu.Lock()
	running := c.agentRunning
	c.uiMu.Unlock()
	if running || c.sessionID == "" {
		return nil
	}
	store := c.gatewayStore()
	if store == nil {
		return nil
	}
	state := queueState(c, store)
	admission := ownQueueHead(state)
	if admission == "" {
		return nil
	}
	return newOwnQueueHold(item, state, admission)
}

// Hold of item on the queued admission, one settling idle list allowed while the owner runs a turn
func newOwnQueueHold(item any, state *gatewaycli.SessionState, admission string) *ownQueueHold {
	state.Lock()
	defer state.Unlock()
	h := &ownQueueHold{item: item, state: state, admission: admission, seq: state.PendingSeq,
		deadline: time.Now().Add(time.Duration(ownQueueHoldSeconds * float64(time.Second)))}
	if pyrepr.Truthy(state.Info["running"]) {
		h.allowedIdleLists = 1
	}
	return h
}

// Pops a turn the owner runs now from the head of the pending input, typed items staying behind the hold
func (h *ownQueueHold) takeGatewayTurn(pending *inputQueue) any {
	pending.mu.Lock()
	if len(pending.items) == 0 {
		pending.mu.Unlock()
		return nil
	}
	head := pending.items[0]
	seeded, isSeeded := head.(*seededQueryMessage)
	_, isNotification := head.(tools.TimelineNotification)
	if !(isSeeded && seeded.AdmissionID != "") && !isNotification {
		pending.mu.Unlock()
		return nil
	}
	pending.items = pending.items[1:]
	pending.mu.Unlock()
	if isSeeded && seeded.AdmissionID == h.admission {
		h.taken = true
	}
	return head
}

// Whether the hold is over, taken, timed out, no longer next or the owner idles without claiming it
func (h *ownQueueHold) done() bool {
	if h.taken || !time.Now().Before(h.deadline) {
		return true
	}
	if ownQueueHead(h.state) != h.admission {
		h.state.Lock()
		_, adopted := h.state.Adopted[h.admission]
		h.state.Unlock()
		return !adopted
	}
	h.state.Lock()
	defer h.state.Unlock()
	if h.state.PendingSeq != h.seq {
		h.seq = h.state.PendingSeq
		if !pyrepr.Truthy(h.state.Info["running"]) {
			h.idleLists++
		}
	}
	return h.idleLists > h.allowedIdleLists
}

// Retires one queued admission, cancelled when it never ran, stopped when its run was interrupted, started otherwise
func cancelQueued(c *CLI, store *gatewaycli.GatewaySessionStore, admission string) (string, error) {
	raw, err := store.Call(c.ctx, "prompt.cancel", map[string]any{"session_id": c.sessionID, "admission_id": admission})
	if err != nil {
		return "", err
	}
	receipt, _ := pyjson.Plain(raw).(map[string]any)
	state := queueState(c, store)
	state.Lock()
	defer state.Unlock()
	if receipt["status"] == "terminal" && receipt["outcome"] == "cancelled" {
		state.QueuedInputs.Delete(admission)
		delete(state.Missed, admission)
		return "cancelled", nil
	}
	state.Missed[admission] = true
	if receipt["status"] == "started" || receipt["outcome"] == "interrupted" {
		return "stopped", nil
	}
	return "started", nil
}

// Replaces a queued prompt with an edited one at the back of the FIFO, returning the new admission id
func replaceQueued(c *CLI, store *gatewaycli.GatewaySessionStore, admission, text string) (string, error) {
	raw, err := store.Call(c.ctx, "prompt.replace_queued", map[string]any{"session_id": c.sessionID, "admission_id": admission,
		"submission_id": uuidHex(), "text": text})
	if err != nil {
		return "", err
	}
	receipt, _ := pyjson.Plain(raw).(map[string]any)
	state := queueState(c, store)
	state.Lock()
	defer state.Unlock()
	state.QueuedInputs.Delete(admission)
	added := strOrEmpty(receipt["admission_id"])
	if added != "" {
		state.QueuedInputs.Set(added, text)
		state.QueuedAt[added] = state.PendingSeq
	}
	return added, nil
}
