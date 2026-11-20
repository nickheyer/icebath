package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
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
		raw, _ := frame.Get("result")
		if fitted, refused := tuihost.FittedAnswer(pending.method, raw); refused != nil {
			pkgLog().Warn(fmt.Sprintf("server request %s (%s) answer refused: %s", id, pending.method, refused))
		} else {
			result = fitted
		}
	}
	if locked != nil {
		result = tuihost.SettleBatch(locked, result, "")
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
func AskViewer(events *SessionEvents, sessionID, method string, params *pyjson.Object, cancelled func() bool, timeout *float64) string {
	pending := askServerRequest(events, sessionID, method, params, cancelled, timeout, nil)
	if pending == nil {
		return ""
	}
	return pending.value
}

// The same round trip for a structured answer, a batch when qids are given, nil when unanswered
func AskViewerResult(events *SessionEvents, sessionID, method string, params *pyjson.Object, cancelled func() bool, timeout *float64, qids []string) *pyjson.Object {
	pending := askServerRequest(events, sessionID, method, params, cancelled, timeout, qids)
	if pending == nil {
		return nil
	}
	return pending.result
}

// Requests still waiting on this live session's stream, oldest first, as their frames carried them
func OpenServerRequests(events *SessionEvents, sessionID string) []*pyjson.Object {
	serverRequests.Lock()
	defer serverRequests.Unlock()
	out := []*pyjson.Object{}
	for _, id := range serverRequests.order {
		p := serverRequests.open[id]
		if p.sessionID != sessionID || p.events != events {
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

// Error for a clarify.lock naming a question the batch never asked
type unknownQuestionError struct{ questionID string }

func (e *unknownQuestionError) Error() string {
	return "the open clarify batch never asked question " + e.questionID
}

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
		pending.result = tuihost.SettleBatch(pending.locked, nil, "submitted")
	}
	serverRequests.Unlock()
	if len(remaining) == 0 {
		pending.set()
	}
	return remaining, nil
}

// Sends the request frame and waits for an answer, an interrupt or the timeout, with the cancel teardown
func askServerRequest(events *SessionEvents, sessionID, method string, params *pyjson.Object, cancelled func() bool, timeout *float64, qids []string) *pendingServerRequest {
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
	if err := tuihost.CheckRequestParams(method, framed); err != nil {
		tuigateway.ReportMisfit("params of request", method, err)
	}
	pending := &pendingServerRequest{done: make(chan struct{}), sessionID: sessionID, events: events, method: method,
		params: framed, locked: pyjson.NewObject()}
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
		pending.result = tuihost.SettleBatch(pending.locked, nil, ended)
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
