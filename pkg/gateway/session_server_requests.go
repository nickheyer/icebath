package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Seconds an owner request waits for a viewer, main's sudo prompt timeout
const RequestTimeoutS = 120.0

const serverRequestPoll = 250 * time.Millisecond

// One open owner request, the stream it went out on and what a viewer answered
type pendingServerRequest struct {
	done      chan struct{}
	once      sync.Once
	sessionID string
	events    *SessionEvents
	method    string
	params    *pyjson.Object
	replay    bool
	value     string
	result    *pyjson.Object
	qids      []string
	locked    *pyjson.Object
}

// Wakes the asker once
func (p *pendingServerRequest) set() { p.once.Do(func() { close(p.done) }) }

// Open owner requests process wide, oldest first
var serverRequests struct {
	sync.Mutex
	order []string
	open  map[string]*pendingServerRequest
}

// Removes an open request, the pending entry when it was open
func popServerRequest(id string) *pendingServerRequest {
	pending, ok := serverRequests.open[id]
	if !ok {
		return nil
	}
	delete(serverRequests.open, id)
	serverRequests.order = slices.DeleteFunc(serverRequests.order, func(k string) bool { return k == id })
	return pending
}

// True when the authority's live session is the very stream a request was sent on
func streamOwnedBy(a *SessionAuthority, sessionID string, events *SessionEvents) bool {
	live, ok := a.live(sessionID)
	return ok && events != nil && live.EventStream == events
}

// Whether an actor attached to the authority's own stream with the respond capability may settle a request
func maySettle(ctx context.Context, a *SessionAuthority, actor Principal, sessionID string, events *SessionEvents) (bool, error) {
	if !streamOwnedBy(a, sessionID, events) {
		return false, nil
	}
	if err := a.Authorize(ctx, actor, SessionRef{ProfileID: a.ProfileID, SessionID: sessionID}, "session:respond"); err != nil {
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			return false, nil
		}
		return false, err
	}
	live, ok := a.live(sessionID)
	if !ok {
		return false, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, subscriber := range live.Subscribers {
		if samePrincipal(subscriber, actor) {
			return true, nil
		}
	}
	return false, nil
}

// A viewer's answer to an owner request, a string srq id with a result or error and no method
func IsResponseFrame(frame any) bool {
	obj, ok := frame.(*pyjson.Object)
	if !ok {
		return false
	}
	if _, has := obj.Get("method"); has {
		return false
	}
	id, _ := obj.GetOr("id", nil).(string)
	_, hasResult := obj.Get("result")
	_, hasError := obj.Get("error")
	return strings.HasPrefix(id, "srq-") && (hasResult || hasError)
}

// Settles the open request a response frame answers, false when nothing here waits for its id
func ResolveServerRequest(frame *pyjson.Object, sessionOK func(sessionID string, events *SessionEvents) bool) bool {
	id, _ := frame.GetOr("id", nil).(string)
	serverRequests.Lock()
	pending, ok := serverRequests.open[id]
	if !ok || !sessionOK(pending.sessionID, pending.events) {
		serverRequests.Unlock()
		return false
	}
	popServerRequest(id)
	var locked *pyjson.Object
	if pending.qids != nil {
		locked = pending.locked.Copy()
	}
	serverRequests.Unlock()
	var result any
	if _, hasError := frame.Get("error"); !hasError {
		result, _ = frame.Get("result")
	}
	if locked != nil {
		result = tuigateway.SettleBatch(locked, result, "")
	}
	pending.value = ""
	pending.result = nil
	if obj, ok := result.(*pyjson.Object); ok {
		if value, isStr := obj.GetOr("value", nil).(string); isStr {
			pending.value = value
		}
		pending.result = obj
	}
	pending.set()
	return true
}

// Sends a one string request to the session's viewers and waits for its value, empty when unanswered
func AskViewer(events *SessionEvents, sessionID, method string, params *pyjson.Object, cancelled func() bool, timeout *float64, replay bool) string {
	pending := askServerRequest(events, sessionID, method, params, cancelled, timeout, nil, replay)
	if pending == nil {
		return ""
	}
	return pending.value
}

// The same round trip for a structured answer, a batch when qids are given, nil when unanswered
func AskViewerResult(events *SessionEvents, sessionID, method string, params *pyjson.Object, cancelled func() bool, timeout *float64, qids []string) *pyjson.Object {
	pending := askServerRequest(events, sessionID, method, params, cancelled, timeout, qids, true)
	if pending == nil {
		return nil
	}
	return pending.result
}

// Whether a request of the method is open on this live session's stream
func ServerRequestWaiting(events *SessionEvents, sessionID, method string) bool {
	serverRequests.Lock()
	defer serverRequests.Unlock()
	for _, p := range serverRequests.open {
		if p.sessionID == sessionID && p.events == events && p.method == method {
			return true
		}
	}
	return false
}

// Requests still waiting on this live session's stream, oldest first, as their frames carried them
func OpenServerRequests(events *SessionEvents, sessionID string) []*pyjson.Object {
	serverRequests.Lock()
	defer serverRequests.Unlock()
	out := []*pyjson.Object{}
	for _, id := range serverRequests.order {
		p := serverRequests.open[id]
		if p.sessionID != sessionID || p.events != events || !p.replay {
			continue
		}
		params := p.params.Copy()
		if p.locked != nil && p.locked.Len() > 0 {
			params.Set("answers", p.locked.Copy())
		}
		out = append(out, pyjson.ObjectOf("id", id, "method", p.method, "params", params))
	}
	return out
}

// Settles every open request of the method on this stream with no answer, the count returned
func WithdrawServerRequests(events *SessionEvents, sessionID, method string) int {
	serverRequests.Lock()
	var settled []*pendingServerRequest
	for _, id := range slices.Clone(serverRequests.order) {
		p := serverRequests.open[id]
		if p.sessionID == sessionID && p.events == events && p.method == method {
			settled = append(settled, popServerRequest(id))
		}
	}
	serverRequests.Unlock()
	for _, p := range settled {
		p.set()
	}
	return len(settled)
}

// Error for a clarify.lock naming a question the batch never asked
type unknownQuestionError struct{ questionID string }

func (e *unknownQuestionError) Error() string { return "ValueError: " + e.questionID }

// Locks one answer of an open batch, the question ids still unanswered, nil when no batch the caller may answer is open
func LockAnswer(requestID, questionID string, answer any, sessionOK func(sessionID string, events *SessionEvents) bool) ([]string, error) {
	serverRequests.Lock()
	pending, ok := serverRequests.open[requestID]
	if !ok || pending.qids == nil || !sessionOK(pending.sessionID, pending.events) {
		serverRequests.Unlock()
		return nil, nil
	}
	if !slices.Contains(pending.qids, questionID) {
		serverRequests.Unlock()
		return nil, &unknownQuestionError{questionID}
	}
	pending.locked.Set(questionID, answer)
	remaining := []string{}
	for _, qid := range pending.qids {
		if _, done := pending.locked.Get(qid); !done {
			remaining = append(remaining, qid)
		}
	}
	if len(remaining) == 0 {
		popServerRequest(requestID)
		pending.result = tuigateway.SettleBatch(pending.locked, nil, "submitted")
	}
	serverRequests.Unlock()
	if len(remaining) == 0 {
		pending.set()
	}
	return remaining, nil
}

// Sends the request frame and waits for an answer, an interrupt or the timeout, with the cancel teardown
func askServerRequest(events *SessionEvents, sessionID, method string, params *pyjson.Object, cancelled func() bool, timeout *float64, qids []string, replay bool) *pendingServerRequest {
	if len(events.Fanout.Transports()) == 0 {
		return nil
	}
	if cancelled == nil {
		cancelled = func() bool { return false }
	}
	buf := make([]byte, 6)
	_, _ = rand.Read(buf)
	requestID := "srq-" + hex.EncodeToString(buf)
	framed := pyjson.ObjectOf("session_id", sessionID)
	if params != nil {
		for _, key := range params.Keys {
			value, _ := params.Get(key)
			framed.Set(key, value)
		}
	}
	pending := &pendingServerRequest{done: make(chan struct{}), sessionID: sessionID, events: events, method: method,
		params: framed, replay: replay, locked: pyjson.NewObject()}
	if len(qids) > 0 {
		pending.qids = slices.Clone(qids)
	}
	serverRequests.Lock()
	if serverRequests.open == nil {
		serverRequests.open = map[string]*pendingServerRequest{}
	}
	serverRequests.open[requestID] = pending
	serverRequests.order = append(serverRequests.order, requestID)
	serverRequests.Unlock()
	events.Fanout.Write(pyjson.ObjectOf("jsonrpc", "2.0", "id", requestID, "method", method, "params", pending.params))
	waited, reason := 0.0, "timeout"
	ticker := time.NewTicker(serverRequestPoll)
	defer ticker.Stop()
wait:
	for {
		select {
		case <-pending.done:
			break wait
		case <-ticker.C:
		}
		waited += serverRequestPoll.Seconds()
		if cancelled() {
			reason = "interrupted"
			break
		}
		if timeout != nil && waited >= *timeout {
			break
		}
	}
	serverRequests.Lock()
	stillOpen := popServerRequest(requestID) == pending
	if stillOpen && pending.qids != nil {
		ended := "timed_out"
		if reason == "interrupted" {
			ended = "cancelled"
		}
		pending.result = tuigateway.SettleBatch(pending.locked, nil, ended)
	}
	serverRequests.Unlock()
	if stillOpen {
		publishRequestCancel(events, sessionID, requestID, method, reason)
		if pending.qids != nil {
			return pending
		}
		return nil
	}
	publishRequestCancel(events, sessionID, requestID, method, "resolved")
	return pending
}

// Withdraws a request card from every renderer, a publish failure swallowed
func publishRequestCancel(events *SessionEvents, sessionID, requestID, method, reason string) {
	defer func() { _ = recover() }()
	events.Publish(sessionID, pyjson.ObjectOf("id", requestID, "method", method, "reason", reason), "request.cancel", nil)
}
