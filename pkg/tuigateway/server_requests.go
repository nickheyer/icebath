package tuigateway

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const (
	// Error code a client answers when none of its windows shows the session of the request
	NotShownCode = 4404
	// Refusal the tool reports once every attached client declined
	NotShownMessage = "No Icebath Desktop window is showing this chat, so its preview, terminal and tour are out " +
		"of reach. Ask the user to open this chat in the Desktop app, then retry."
)

var batchOutcomes = []string{"submitted", "timed_out", "cancelled"}

// Answers and outcome of a batch clarify from its locks, the closing frame result or the way its wait ended
func settleBatch(locked *pyjson.Object, frameResult any, ended string) *pyjson.Object {
	if ended != "" {
		outcome := "cancelled"
		if slices.Contains(batchOutcomes, ended) {
			outcome = ended
		}
		return pyjson.ObjectOf("answers", locked.Copy(), "outcome", outcome)
	}
	if frame, ok := frameResult.(*pyjson.Object); ok {
		if answers, ok := frame.GetOr("answers", nil).(*pyjson.Object); ok {
			merged := locked.Copy()
			merged.Update(answers)
			out := frame.Copy()
			out.Set("answers", merged)
			outcome := "submitted"
			if named, ok := frame.GetOr("outcome", nil).(string); ok && slices.Contains(batchOutcomes, named) {
				outcome = named
			}
			out.Set("outcome", outcome)
			return out
		}
	}
	return pyjson.ObjectOf("answers", locked.Copy(), "outcome", "cancelled")
}

// One question the backend asked the renderer, open until a response, a cancel or its deadline settles it
type ServerRequest struct {
	ID        string
	SID       string
	Method    string
	Params    *pyjson.Object
	done      chan struct{}
	doneOnce  sync.Once
	result    *pyjson.Object
	answered  bool
	createdAt time.Time
	qids      []string
	locked    *pyjson.Object
	onResult  func(*pyjson.Object)
	declined  []Transport
	writing   bool
	withdrawn *string
}

func newServerRequest(sid, method string, params *pyjson.Object, qids []string, onResult func(*pyjson.Object)) *ServerRequest {
	req := &ServerRequest{ID: "srq-" + uuidHex()[:12], SID: sid, Method: method, Params: pyjson.NewObject(), done: make(chan struct{}),
		createdAt: time.Now(), locked: pyjson.NewObject(), onResult: onResult}
	if params != nil {
		req.Params = params.Copy()
	}
	if len(qids) > 0 {
		req.qids = slices.Clone(qids)
	}
	return req
}

func (r *ServerRequest) set() { r.doneOnce.Do(func() { close(r.done) }) }

// Params of the request led by its session id
func (r *ServerRequest) wireParams() *pyjson.Object {
	params := pyjson.ObjectOf("session_id", r.SID)
	params.Update(r.Params)
	return params
}

// JSON RPC request frame as sent to the client
func (r *ServerRequest) Frame() *pyjson.Object {
	return pyjson.ObjectOf("jsonrpc", "2.0", "id", r.ID, "method", r.Method, "params", r.wireParams())
}

// Open request entry for a reconnecting client, with the batch answers locked so far
func (r *ServerRequest) Snapshot() *pyjson.Object {
	params := r.wireParams()
	if r.locked.Len() > 0 {
		params.Set("answers", r.locked.Copy())
	}
	return pyjson.ObjectOf("id", r.ID, "method", r.Method, "params", params)
}

// Open requests in send order and the frame sinks the server binds
var (
	requestsMu       sync.Mutex
	openRequests     []*ServerRequest
	answeringClients []Transport
	sinkWrite        = func(frame *pyjson.Object) {}
	sinkEmit         = func(event, sid string, payload map[string]any) {}
	sinkAnswerable   = func(sid string) bool { return true }
	sinkClients      = func(sid string) []Transport { return nil }
)

func requestsLog() *slog.Logger {
	return slog.Default().With("logger", "tui_gateway.server_requests")
}

// Binds the frame writer, the event emitter, the answerable check and, when given, the answering client lookup
func BindSinks(write func(frame *pyjson.Object), emit func(event, sid string, payload map[string]any), answerable func(sid string) bool,
	clients func(sid string) []Transport) {
	requestsMu.Lock()
	defer requestsMu.Unlock()
	sinkWrite, sinkEmit, sinkAnswerable = write, emit, answerable
	if clients != nil {
		sinkClients = clients
	}
}

// Records whether the client of a transport answers server to client requests
func Advertise(transport Transport, serverRequests bool) {
	requestsMu.Lock()
	defer requestsMu.Unlock()
	answeringClients = slices.DeleteFunc(answeringClients, func(t Transport) bool { return t == transport })
	if serverRequests {
		answeringClients = append(answeringClients, transport)
	}
}

// Drops the advertisement of a disconnected transport
func Forget(transport Transport) {
	requestsMu.Lock()
	defer requestsMu.Unlock()
	answeringClients = slices.DeleteFunc(answeringClients, func(t Transport) bool { return t == transport })
}

// True when the client of the transport advertised that it answers server to client requests
func AnswersRequests(transport Transport) bool {
	requestsMu.Lock()
	defer requestsMu.Unlock()
	return slices.Contains(answeringClients, transport)
}

func currentSinks() (func(*pyjson.Object), func(string, string, map[string]any), func(string) bool, func(string) []Transport) {
	requestsMu.Lock()
	defer requestsMu.Unlock()
	return sinkWrite, sinkEmit, sinkAnswerable, sinkClients
}

// True when no attached client of the session answers server requests, logged once per refused send
func unanswerable(method, sid string) bool {
	_, _, answerable, _ := currentSinks()
	if answerable(sid) {
		return false
	}
	requestsLog().Info(fmt.Sprintf("server request %s for %s not sent: the attached client predates server→client requests (update the Icebath app)", method, sid))
	return true
}

func emitCancel(req *ServerRequest, reason string) {
	_, emit, _, _ := currentSinks()
	emit("request.cancel", req.SID, map[string]any{"id": req.ID, "method": req.Method, "reason": reason})
}

// Removes req from the open list by identity, true when it was still open
func popOpenLocked(req *ServerRequest) bool {
	for i, open := range openRequests {
		if open == req {
			openRequests = slices.Delete(openRequests, i, i+1)
			return true
		}
	}
	return false
}

func findOpenLocked(id string) *ServerRequest {
	for _, open := range openRequests {
		if open.ID == id {
			return open
		}
	}
	return nil
}

// Validates the request against its contract, publishes it and writes its frame, a cancel in between announced after
func register(req *ServerRequest) error {
	contractsMu.RLock()
	contract := ServerRequests[req.Method]
	contractsMu.RUnlock()
	if contract == nil {
		return fmt.Errorf("server request %s has no contract in tui_gateway/contracts", pyrepr.Repr(req.Method))
	}
	if _, problem := ValidateParams(contract, req.wireParams()); problem != "" {
		return &RequestParamsError{problem}
	}
	requestsMu.Lock()
	req.writing = true
	openRequests = append(openRequests, req)
	write := sinkWrite
	requestsMu.Unlock()
	var withdrawn *string
	func() {
		defer func() {
			requestsMu.Lock()
			req.writing = false
			withdrawn = req.withdrawn
			requestsMu.Unlock()
		}()
		write(req.Frame())
	}()
	if withdrawn != nil {
		emitCancel(req, *withdrawn)
	}
	return nil
}

// Params of a server request that its contract rejects, the ValueError of the sender
type RequestParamsError struct{ Problem string }

func (e *RequestParamsError) Error() string { return e.Problem }

// Blocks until the request settles or its deadline passes, true when the context ended the wait first
var requestWait = func(ctx context.Context, done <-chan struct{}, deadline <-chan time.Time) bool {
	select {
	case <-done:
	case <-deadline:
	case <-ctx.Done():
		return true
	}
	return false
}

// Sends one request and blocks for the result, nil when unanswered, a nil timeout waiting until settled
func Send(ctx context.Context, method, sid string, params *pyjson.Object, timeout *float64, qids []string) (*pyjson.Object, error) {
	if unanswerable(method, sid) {
		return nil, nil
	}
	req := newServerRequest(sid, method, params, qids, nil)
	if err := register(req); err != nil {
		return nil, err
	}
	var deadline <-chan time.Time
	if timeout != nil {
		timer := time.NewTimer(time.Duration(max(*timeout, 0) * float64(time.Second)))
		defer timer.Stop()
		deadline = timer.C
	}
	if requestWait(ctx, req.done, deadline) {
		requestsMu.Lock()
		stillOpen := popOpenLocked(req)
		requestsMu.Unlock()
		if stillOpen {
			emitCancel(req, "interrupted")
		}
		return nil, ctx.Err()
	}
	requestsMu.Lock()
	timedOut := popOpenLocked(req)
	answered, result, locked := req.answered, req.result, req.locked.Copy()
	requestsMu.Unlock()
	if answered {
		return result, nil
	}
	if timedOut {
		emitCancel(req, "timeout")
		if req.qids != nil {
			return settleBatch(locked, nil, "timed_out"), nil
		}
	}
	return nil, nil
}

// Sends one request whose wait is owned elsewhere and returns the settle call that withdraws it if still open
func SendAsync(method, sid string, params *pyjson.Object, onResult func(*pyjson.Object)) (func(reason string), error) {
	if unanswerable(method, sid) {
		onResult(nil)
		return func(string) {}, nil
	}
	req := newServerRequest(sid, method, params, nil, onResult)
	if err := register(req); err != nil {
		return nil, err
	}
	return func(reason string) {
		requestsMu.Lock()
		stillOpen := popOpenLocked(req)
		requestsMu.Unlock()
		if stillOpen {
			emitCancel(req, reason)
		}
	}, nil
}

// Python equality of a JSON number with an integer code
func jsonIsInt(v any, n int) bool {
	switch x := v.(type) {
	case int:
		return x == n
	case int64:
		return x == int64(n)
	case float64:
		return x == float64(n)
	case bool:
		return (x && n == 1) || (!x && n == 0)
	}
	if s, ok := v.(fmt.Stringer); ok {
		return s.String() == fmt.Sprint(n)
	}
	return false
}

func isNotShown(frame *pyjson.Object) bool {
	errObj, ok := frame.GetOr("error", nil).(*pyjson.Object)
	return ok && jsonIsInt(errObj.GetOr("code", nil), NotShownCode)
}

// Records one client declining and settles with the not shown refusal once every answering client declined
func decline(rid string, transport Transport) bool {
	requestsMu.Lock()
	req := findOpenLocked(rid)
	lookup := sinkClients
	requestsMu.Unlock()
	if req == nil {
		requestsLog().Debug(fmt.Sprintf("server request %s: decline dropped, request no longer open", rid))
		return false
	}
	var clients []Transport
	if transport != nil {
		clients = lookup(req.SID)
	}
	requestsMu.Lock()
	if findOpenLocked(rid) != req {
		requestsMu.Unlock()
		return true
	}
	if transport != nil && !slices.Contains(req.declined, transport) {
		req.declined = append(req.declined, transport)
	}
	unanimous := len(clients) > 0
	for _, client := range clients {
		unanimous = unanimous && slices.Contains(req.declined, client)
	}
	if !unanimous {
		requestsMu.Unlock()
		return true
	}
	popOpenLocked(req)
	value, _ := pyjson.Dumps(pyjson.ObjectOf("success", false, "error", NotShownMessage))
	req.result = pyjson.ObjectOf("value", value)
	req.answered = true
	result := req.result
	requestsMu.Unlock()
	if req.onResult != nil {
		req.onResult(result)
	}
	req.set()
	return true
}

// Routes one client response frame to its open request, false when nothing waits for that id
func ResolveResponse(frame *pyjson.Object, transport Transport) bool {
	rid, ok := frame.GetOr("id", nil).(string)
	if !ok {
		return false
	}
	if isNotShown(frame) {
		return decline(rid, transport)
	}
	requestsMu.Lock()
	req := findOpenLocked(rid)
	if req == nil {
		requestsMu.Unlock()
		requestsLog().Debug(fmt.Sprintf("server request %s: response dropped, request no longer open", rid))
		return false
	}
	popOpenLocked(req)
	if errValue, isError := frame.Get("error"); isError {
		requestsLog().Debug(fmt.Sprintf("server request %s (%s) answered with error: %s", rid, req.Method, pyrepr.Str(errValue)))
		req.result, req.answered = nil, false
	} else {
		result, isObject := frame.GetOr("result", nil).(*pyjson.Object)
		if !isObject {
			result = pyjson.NewObject()
		}
		req.result = result
		if len(req.qids) > 0 {
			req.result = settleBatch(req.locked, req.result, "")
		}
		req.answered = true
	}
	result := req.result
	requestsMu.Unlock()
	if req.onResult != nil {
		req.onResult(result)
	}
	req.set()
	return true
}

// Question id a batch does not hold, the ValueError of a lock on it
type UnknownQuestionError struct{ QuestionID string }

func (e *UnknownQuestionError) Error() string {
	return "unknown question_id " + pyrepr.Repr(e.QuestionID)
}

// Locks one batch answer and returns the ids still open, not ok when no open batch has that request id
func LockAnswer(requestID, questionID string, answer any) (remaining []string, ok bool, err error) {
	requestsMu.Lock()
	req := findOpenLocked(requestID)
	if req == nil || req.qids == nil {
		requestsMu.Unlock()
		return nil, false, nil
	}
	if !slices.Contains(req.qids, questionID) {
		requestsMu.Unlock()
		return nil, false, &UnknownQuestionError{questionID}
	}
	req.locked.Set(questionID, answer)
	remaining = []string{}
	for _, qid := range req.qids {
		if _, locked := req.locked.Get(qid); !locked {
			remaining = append(remaining, qid)
		}
	}
	if len(remaining) == 0 {
		req.result, req.answered = settleBatch(req.locked, nil, "submitted"), true
		popOpenLocked(req)
	}
	requestsMu.Unlock()
	if len(remaining) == 0 {
		req.set()
	}
	return remaining, true, nil
}

// Withdraws the open requests of one session or of all when sid is nil, narrowed to one method when given
func Cancel(sid *string, reason string, method *string) int {
	requestsMu.Lock()
	targets := []*ServerRequest{}
	for _, req := range openRequests {
		if (sid == nil || req.SID == *sid) && (method == nil || req.Method == *method) {
			targets = append(targets, req)
		}
	}
	announced := map[*ServerRequest]bool{}
	for _, req := range targets {
		popOpenLocked(req)
		if req.qids != nil {
			req.result, req.answered = settleBatch(req.locked, nil, "cancelled"), true
		} else {
			req.result, req.answered = nil, false
		}
		if req.writing {
			withdrawn := reason
			req.withdrawn = &withdrawn
		} else {
			announced[req] = true
		}
	}
	requestsMu.Unlock()
	for _, req := range targets {
		if req.onResult != nil {
			req.onResult(nil)
		}
		req.set()
		if announced[req] {
			emitCancel(req, reason)
		}
	}
	return len(targets)
}

// Open requests of a session oldest first, the caller holding the lock
func sessionRequestsLocked(sid string) []*ServerRequest {
	reqs := []*ServerRequest{}
	for _, req := range openRequests {
		if req.SID == sid {
			reqs = append(reqs, req)
		}
	}
	sort.SliceStable(reqs, func(i, j int) bool { return reqs[i].createdAt.Before(reqs[j].createdAt) })
	return reqs
}

// Unanswered requests of a session as snapshots, oldest first
func OpenRequests(sid string) []*pyjson.Object {
	requestsMu.Lock()
	defer requestsMu.Unlock()
	out := []*pyjson.Object{}
	for _, req := range sessionRequestsLocked(sid) {
		out = append(out, req.Snapshot())
	}
	return out
}

// Unanswered server requests across every session
func OpenRequestCount() int {
	requestsMu.Lock()
	defer requestsMu.Unlock()
	return len(openRequests)
}

// Method of the oldest open request of a session, empty when it waits on nothing
func PendingKind(sid string) string {
	requestsMu.Lock()
	defer requestsMu.Unlock()
	if reqs := sessionRequestsLocked(sid); len(reqs) > 0 {
		return reqs[0].Method
	}
	return ""
}

// True for a client response, an object with an id and a result or error member and no method
func IsResponseFrame(obj any) bool {
	frame, ok := obj.(*pyjson.Object)
	if !ok {
		return false
	}
	_, hasMethod := frame.Get("method")
	_, hasID := frame.Get("id")
	_, hasResult := frame.Get("result")
	_, hasError := frame.Get("error")
	return !hasMethod && hasID && (hasResult || hasError)
}

// Clears the open requests and the client advertisements between tests
func ResetForTests() {
	requestsMu.Lock()
	defer requestsMu.Unlock()
	openRequests = nil
	answeringClients = nil
}
