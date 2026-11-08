package browser

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/vault"
)

const (
	maxPostAttachReconnectFailures = 5
	recentDialogsMax               = 20
	frameTreeMaxEntries            = 30
	frameTreeMaxOOPIFDepth         = 2
	dialogBridgeHost               = "icebath-dialog-bridge.invalid"
	dialogBridgeURLPattern         = "http://" + dialogBridgeHost + "/*"
	supervisorStartTimeoutS        = 15.0
	supervisorNoTimeout            = -1.0
	supervisorMaxMessage           = 50 * 1024 * 1024
)

const dialogBridgeScript = `
(() => {
  if (window.__icebathDialogBridgeInstalled) return;
  window.__icebathDialogBridgeInstalled = true;
  const ENDPOINT = "http://icebath-dialog-bridge.invalid/";
  function ask(kind, message, defaultPrompt) {
    try {
      const xhr = new XMLHttpRequest();
      const params = new URLSearchParams({
        kind: String(kind || ""),
        message: String(message == null ? "" : message),
        default_prompt: String(defaultPrompt == null ? "" : defaultPrompt),
      });
      xhr.open("GET", ENDPOINT + "?" + params.toString(), false);  // sync
      xhr.send(null);
      if (xhr.status !== 200) return null;
      let parsed;
      try { parsed = JSON.parse(xhr.responseText || ""); } catch (e) { return null; }
      if (kind === "alert") return undefined;
      if (kind === "confirm") return Boolean(parsed && parsed.accept);
      if (kind === "prompt") {
        if (!parsed || !parsed.accept) return null;
        return parsed.prompt_text == null ? "" : String(parsed.prompt_text);
      }
      return null;
    } catch (e) {
      return null;
    }
  }
  window.alert   = function(message) { ask("alert",   message, ""); };
  window.confirm = function(message) {
    const r = ask("confirm", message, "");
    return r === null ? false : Boolean(r);
  };
  window.prompt  = function(message, def) {
    const r = ask("prompt", message, def == null ? "" : def);
    return r === null ? null : String(r);
  };
})();
`

var autoAttachParams = pyjson.ObjectOf("autoAttach", true, "waitForDebuggerOnStart", false, "flatten", true)

// Python exception of the given class carrying msg as its str
func pyError(class, msg string) error { return &cloudError{Class: class, Msg: msg} }

func timeoutError(msg string) error { return pyError("TimeoutError", msg) }

var errLoopUnavailable = pyError("_LoopUnavailable", "Browser supervisor loop unavailable")

// Endpoint credentials masked in an error or url text
func redactCDPErrorText(text string) string { return tools.RedactCDPURL(text) }

func supervisorFail(msg string) *pyjson.Object { return pyjson.ObjectOf("ok", false, "error", msg) }

func supervisorErr(err error) *pyjson.Object {
	return supervisorFail(pyrepr.ExceptionName(err) + ": " + pyrepr.ExceptionMessage(err))
}

// Value of key in a decoded CDP object, nil when absent or the holder is not an object
func anyGet(v any, key string) any {
	o, ok := pyjson.AsObject(v)
	if !ok {
		return nil
	}
	out, _ := o.Get(key)
	return out
}

// Object under key with an empty object for a missing or non object value
func objChild(v any, key string) *pyjson.Object {
	if o, ok := pyjson.AsObject(anyGet(v, key)); ok {
		return o
	}
	return pyjson.NewObject()
}

// str of the value under key or the empty string when it is falsy
func objStr(v any, key string) string {
	got := anyGet(v, key)
	if !pyrepr.Truthy(got) {
		return ""
	}
	return pyrepr.Str(got)
}

// Subscript that raises KeyError as dict indexing does
func objIndex(v any, key string) (any, error) {
	o, ok := pyjson.AsObject(v)
	if ok {
		if out, has := o.Get(key); has {
			return out, nil
		}
	}
	return nil, pyError("KeyError", pyrepr.StrRepr(key))
}

// Websocket surface the supervisor speaks CDP over
type supervisorWS interface {
	WriteText(ctx context.Context, payload []byte) error
	ReadMessage(ctx context.Context) (byte, []byte, error)
	Close(code int, reason string) error
}

// Serial executor standing in for the supervisor thread's event loop
type supervisorLoop struct {
	mu      sync.Mutex
	queue   []func()
	wake    chan struct{}
	closed  bool
	running bool
	gid     uint64
	stopped chan struct{}
}

func newSupervisorLoop() *supervisorLoop {
	return &supervisorLoop{wake: make(chan struct{}, 1), stopped: make(chan struct{})}
}

// Goroutine id read from the runtime stack header
func goroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	field := bytes.Fields(buf[:n])
	if len(field) < 2 {
		return 0
	}
	id, _ := strconv.ParseUint(string(field[1]), 10, 64)
	return id
}

// Runs queued callbacks in order until quit closes, then refuses new work
func (l *supervisorLoop) serve(quit <-chan struct{}) {
	l.mu.Lock()
	l.running, l.gid = true, goroutineID()
	l.mu.Unlock()
	defer close(l.stopped)
	for {
		l.mu.Lock()
		if len(l.queue) > 0 {
			fn := l.queue[0]
			l.queue = l.queue[1:]
			l.mu.Unlock()
			fn()
			continue
		}
		l.mu.Unlock()
		select {
		case <-l.wake:
		case <-quit:
			l.mu.Lock()
			l.closed, l.running, l.queue = true, false, nil
			l.mu.Unlock()
			return
		}
	}
}

// Queues fn on the loop, false once the loop closed
func (l *supervisorLoop) submit(fn func()) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return false
	}
	l.queue = append(l.queue, fn)
	select {
	case l.wake <- struct{}{}:
	default:
	}
	return true
}

func (l *supervisorLoop) isRunning() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.running
}

func (l *supervisorLoop) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// True when the caller runs on the loop goroutine itself
func (l *supervisorLoop) onLoop() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.running && l.gid == goroutineID()
}

// One JS dialog currently open on some frame's session
type PendingDialog struct {
	ID, Type, Message, DefaultPrompt string
	OpenedAt                         float64
	CDPSessionID                     string
	FrameID                          any
	BridgeRequestID                  string
}

// Snapshot dict with the page originated text redacted
func (d PendingDialog) toDict(env *tools.Env) *pyjson.Object {
	return pyjson.ObjectOf("id", d.ID, "type", d.Type, "message", redactSupervisorText(env, d.Message),
		"default_prompt", redactSupervisorText(env, d.DefaultPrompt), "opened_at", d.OpenedAt, "frame_id", d.FrameID)
}

// One dialog that opened and was then handled
type DialogRecord struct {
	ID, Type, Message  string
	OpenedAt, ClosedAt float64
	ClosedBy           string
	FrameID            any
}

func (r DialogRecord) toDict(env *tools.Env) *pyjson.Object {
	return pyjson.ObjectOf("id", r.ID, "type", r.Type, "message", redactSupervisorText(env, r.Message),
		"opened_at", r.OpenedAt, "closed_at", r.ClosedAt, "closed_by", r.ClosedBy, "frame_id", r.FrameID)
}

// Page originated text redacted before it leaves a supervisor snapshot
func redactSupervisorText(env *tools.Env, value string) string {
	return tools.RedactSensitiveTextWith(env, value, tools.RedactOptions{Force: true})
}

// Ring buffer capped at keep entries once it overflows to twice that
func trimDialogRing(events []DialogRecord, keep int) []DialogRecord {
	if len(events) > keep*2 {
		return append([]DialogRecord(nil), events[len(events)-keep:]...)
	}
	return events
}

// One frame of the page's frame tree, out of process frames carrying their own session
type FrameInfo struct {
	FrameID, URL, Origin string
	ParentFrameID        string
	IsOOPIF              bool
	CDPSessionID         string
	Name                 string
}

func (f FrameInfo) toDict() *pyjson.Object {
	d := pyjson.ObjectOf("frame_id", f.FrameID, "url", f.URL, "origin", f.Origin, "is_oopif", f.IsOOPIF)
	for _, kv := range [][2]string{{"session_id", f.CDPSessionID}, {"parent_frame_id", f.ParentFrameID}, {"name", f.Name}} {
		if kv[1] != "" {
			d.Set(kv[0], kv[1])
		}
	}
	return d
}

// Insertion ordered frame table keyed by frame id
type frameTable struct {
	keys []string
	m    map[string]FrameInfo
}

func (t *frameTable) get(id string) (FrameInfo, bool) {
	f, ok := t.m[id]
	return f, ok
}

func (t *frameTable) set(f FrameInfo) {
	if t.m == nil {
		t.m = map[string]FrameInfo{}
	}
	if _, ok := t.m[f.FrameID]; !ok {
		t.keys = append(t.keys, f.FrameID)
	}
	t.m[f.FrameID] = f
}

func (t *frameTable) pop(id string) {
	if _, ok := t.m[id]; !ok {
		return
	}
	delete(t.m, id)
	for i, k := range t.keys {
		if k == id {
			t.keys = append(t.keys[:i:i], t.keys[i+1:]...)
			break
		}
	}
}

func (t *frameTable) values() []FrameInfo {
	out := make([]FrameInfo, 0, len(t.keys))
	for _, k := range t.keys {
		out = append(out, t.m[k])
	}
	return out
}

// Read only view of supervisor state for tool handlers
type SupervisorSnapshot struct {
	PendingDialogs []PendingDialog
	RecentDialogs  []DialogRecord
	FrameTree      *pyjson.Object
	Active         bool
	CDPURL, TaskID string
	env            *tools.Env
}

// Serialized snapshot for tool output, recent dialogs only when any exist
func (s SupervisorSnapshot) ToDict() *pyjson.Object {
	pending := make([]any, 0, len(s.PendingDialogs))
	for _, d := range s.PendingDialogs {
		pending = append(pending, d.toDict(s.env))
	}
	out := pyjson.ObjectOf("pending_dialogs", pending, "frame_tree", s.FrameTree)
	if len(s.RecentDialogs) > 0 {
		recent := make([]any, 0, len(s.RecentDialogs))
		for _, r := range s.RecentDialogs {
			recent = append(recent, r.toDict(s.env))
		}
		out.Set("recent_dialogs", recent)
	}
	return out
}

type supervisorReply struct {
	msg *pyjson.Object
	err error
}

// Persistent CDP connection per task watching dialogs and frames
type cdpSupervisor struct {
	taskID, cdpURL, dialogPolicy string
	dialogTimeoutS               float64
	env                          *tools.Env
	log                          *slog.Logger

	stateMu        sync.Mutex
	pendingKeys    []string
	pendingDialogs map[string]PendingDialog
	recentDialogs  []DialogRecord
	frames         frameTable
	active         bool
	pageSessionID  string

	ctlMu         sync.Mutex
	loop          *supervisorLoop
	threadDone    chan struct{}
	stopCh        chan struct{}
	startErr      error
	ready         *pythread.Event
	stopRequested atomic.Bool

	wsMu         sync.Mutex
	ws           supervisorWS
	nextCallID   int64
	pendingCalls map[int64]chan supervisorReply

	wdMu      sync.Mutex
	watchdogs map[string]*time.Timer
	dialogSeq int
}

// New supervisor for one task and endpoint, a ValueError for an unknown dialog policy
func newCDPSupervisor(env *tools.Env, taskID, cdpURL, policy string, dialogTimeoutS float64) (*cdpSupervisor, error) {
	if !validDialogPolicies[policy] {
		return nil, pyError("ValueError", fmt.Sprintf("Invalid dialog_policy %s; must be one of ['auto_accept', 'auto_dismiss', 'must_respond']",
			pyrepr.StrRepr(policy)))
	}
	return &cdpSupervisor{taskID: taskID, cdpURL: cdpURL, dialogPolicy: policy, dialogTimeoutS: dialogTimeoutS, env: env,
		log: cfgOf(env).logger(), pendingDialogs: map[string]PendingDialog{}, ready: &pythread.Event{},
		pendingCalls: map[int64]chan supervisorReply{}, watchdogs: map[string]*time.Timer{}}, nil
}

func (s *cdpSupervisor) currentLoop() *supervisorLoop {
	s.ctlMu.Lock()
	defer s.ctlMu.Unlock()
	return s.loop
}

// True while the supervisor thread runs
func (s *cdpSupervisor) threadAlive() bool {
	s.ctlMu.Lock()
	done := s.threadDone
	s.ctlMu.Unlock()
	if done == nil {
		return false
	}
	select {
	case <-done:
		return false
	default:
		return true
	}
}

// Thread alive with its loop running
func (s *cdpSupervisor) healthy() bool { return s.threadAlive() && s.currentLoop().isRunning() }

func (s *cdpSupervisor) currentWS() supervisorWS {
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	return s.ws
}

func (s *cdpSupervisor) isActive() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.active
}

func (s *cdpSupervisor) setActive(v bool) {
	s.stateMu.Lock()
	s.active = v
	s.stateMu.Unlock()
}

func (s *cdpSupervisor) pageSession() string {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.pageSessionID
}

func (s *cdpSupervisor) setPageSession(sid string) {
	s.stateMu.Lock()
	s.pageSessionID = sid
	s.stateMu.Unlock()
}

var supervisorStart = (*cdpSupervisor).start

// Launches the supervisor thread and blocks until attach completes, raising the redacted attach failure
func (s *cdpSupervisor) start(timeout float64) error {
	if s.threadAlive() {
		return nil
	}
	s.ready.Clear()
	done := make(chan struct{})
	s.ctlMu.Lock()
	s.startErr = nil
	s.stopRequested.Store(false)
	s.stopCh = make(chan struct{})
	s.threadDone = done
	s.ctlMu.Unlock()
	go s.threadMain(done)
	if !s.ready.Wait(timeout) {
		s.stop(5.0)
		redacted := []rune(redactCDPErrorText(s.cdpURL))
		if len(redacted) > 80 {
			redacted = redacted[:80]
		}
		return timeoutError(fmt.Sprintf("CDP supervisor did not attach within %ss (cdp_url=%s...)", pyrepr.Float(timeout), string(redacted)))
	}
	s.ctlMu.Lock()
	err := s.startErr
	s.ctlMu.Unlock()
	if err != nil {
		s.stop(5.0)
		return runtimeError("CDP supervisor failed to start: " + redactCDPErrorText(pyrepr.ExceptionMessage(err)))
	}
	return nil
}

// Closes the socket on the loop and joins the thread within timeout seconds
func (s *cdpSupervisor) stop(timeout float64) {
	s.stopRequested.Store(true)
	s.ctlMu.Lock()
	if s.stopCh != nil {
		select {
		case <-s.stopCh:
		default:
			close(s.stopCh)
		}
	}
	done := s.threadDone
	s.ctlMu.Unlock()
	if loop := s.currentLoop(); loop.isRunning() {
		_, _ = s.schedule(func() (any, error) { s.closeWS(); return nil, nil }, 2.0, true)
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(seconds(timeout)):
		}
	}
	s.setActive(false)
}

func seconds(v float64) time.Duration { return time.Duration(v * float64(time.Second)) }

// Runs fn as a coroutine started from the loop and waits timeout seconds for its result
func (s *cdpSupervisor) schedule(fn func() (any, error), timeout float64, onLoop bool) (any, error) {
	loop := s.currentLoop()
	if loop == nil {
		return nil, errLoopUnavailable
	}
	res := make(chan supervisorReplyAny, 1)
	ok := loop.submit(func() {
		run := func() {
			out, err := fn()
			res <- supervisorReplyAny{out, err}
		}
		if onLoop {
			run()
		} else {
			go run()
		}
	})
	if !ok {
		return nil, errLoopUnavailable
	}
	select {
	case r := <-res:
		return r.out, r.err
	case <-time.After(seconds(timeout)):
		return nil, timeoutError("")
	}
}

type supervisorReplyAny struct {
	out any
	err error
}

// Immutable copy of the current dialogs and frame tree
func (s *cdpSupervisor) snapshot() SupervisorSnapshot {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	pending := make([]PendingDialog, 0, len(s.pendingKeys))
	for _, k := range s.pendingKeys {
		pending = append(pending, s.pendingDialogs[k])
	}
	recent := s.recentDialogs
	if len(recent) > recentDialogsMax {
		recent = recent[len(recent)-recentDialogsMax:]
	}
	return SupervisorSnapshot{PendingDialogs: pending, RecentDialogs: append([]DialogRecord(nil), recent...),
		FrameTree: s.buildFrameTreeLocked(), Active: s.active, CDPURL: s.cdpURL, TaskID: s.taskID, env: s.env}
}

// Accepts or dismisses a pending dialog through the loop, an ok false dict for recoverable errors
func (s *cdpSupervisor) respondToDialog(action string, promptText *string, dialogID string, timeout float64) *pyjson.Object {
	if action != "accept" && action != "dismiss" {
		return supervisorFail("action must be 'accept' or 'dismiss', got " + pyrepr.StrRepr(action))
	}
	s.stateMu.Lock()
	if !s.active {
		s.stateMu.Unlock()
		return supervisorFail("supervisor is not active")
	}
	if len(s.pendingKeys) == 0 {
		s.stateMu.Unlock()
		return supervisorFail("no dialog is currently open")
	}
	var dialog PendingDialog
	switch {
	case dialogID != "":
		d, ok := s.pendingDialogs[dialogID]
		if !ok {
			known := append([]string(nil), s.pendingKeys...)
			sort.Strings(known)
			s.stateMu.Unlock()
			return supervisorFail(fmt.Sprintf("dialog_id %s not found (known: %s)", pyrepr.StrRepr(dialogID), pyrepr.Repr(stringsAny(known))))
		}
		dialog = d
	case len(s.pendingKeys) > 1:
		ids := append([]string(nil), s.pendingKeys...)
		s.stateMu.Unlock()
		return supervisorFail(fmt.Sprintf("%d pending dialogs; specify dialog_id. Candidates: %s", len(ids), pyrepr.Repr(stringsAny(ids))))
	default:
		dialog = s.pendingDialogs[s.pendingKeys[0]]
	}
	s.stateMu.Unlock()
	if s.currentLoop() == nil {
		return supervisorFail("supervisor loop is not running")
	}
	text := ""
	if promptText != nil {
		text = *promptText
	}
	_, err := s.schedule(func() (any, error) {
		return nil, s.handleDialogCDP(dialog, action == "accept", text)
	}, timeout, false)
	if errors.Is(err, errLoopUnavailable) {
		return supervisorFail(err.Error())
	}
	if err != nil {
		return supervisorErr(err)
	}
	return pyjson.ObjectOf("ok", true, "dialog", dialog.toDict(s.env))
}

func stringsAny(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// Evaluates expression in the attached page over the live socket, retrying by reference on a too deep value
func (s *cdpSupervisor) evaluateRuntime(expression string, returnByValue, awaitPromise bool, timeout float64) *pyjson.Object {
	if !s.currentLoop().isRunning() {
		return supervisorFail("supervisor loop is not running")
	}
	s.stateMu.Lock()
	active, sessionID := s.active, s.pageSessionID
	s.stateMu.Unlock()
	if !active {
		return supervisorFail("supervisor is not active")
	}
	if sessionID == "" {
		return supervisorFail("supervisor has no attached page session")
	}
	runEval := func(byValue bool) (any, error) {
		params := pyjson.ObjectOf("expression", expression, "returnByValue", byValue, "awaitPromise", awaitPromise, "userGesture", true)
		return s.schedule(func() (any, error) { return s.cdpCall("Runtime.evaluate", params, sessionID, timeout) }, timeout+1, false)
	}
	response, err := runEval(returnByValue)
	if err != nil {
		if !(returnByValue && strings.Contains(pystr.Lower(pyrepr.ExceptionMessage(err)), "reference chain is too long")) {
			return supervisorErr(err)
		}
		if response, err = runEval(false); err != nil {
			return supervisorErr(err)
		}
	}
	resultPayload := pyjson.NewObject()
	if o, ok := response.(*pyjson.Object); ok && o != nil {
		resultPayload = objChild(o, "result")
	}
	if details := anyGet(resultPayload, "exceptionDetails"); pyrepr.Truthy(details) {
		excText := anyGet(details, "text")
		if !pyrepr.Truthy(excText) {
			excText = "JavaScript exception"
		}
		description := anyGet(objChild(details, "exception"), "description")
		if pyrepr.Truthy(description) {
			return supervisorFail(pyrepr.Str(excText) + ": " + pyrepr.Str(description))
		}
		return supervisorFail(pyrepr.Str(excText))
	}
	resultObj := objChild(resultPayload, "result")
	resultType, ok := resultObj.Get("type")
	if !ok {
		resultType = "undefined"
	}
	var value any
	if v, has := resultObj.Get("value"); has {
		value = v
	} else if resultType != "undefined" {
		value = anyGet(resultObj, "description")
		if !pyrepr.Truthy(value) {
			value = anyGet(resultObj, "unserializableValue")
		}
	}
	return pyjson.ObjectOf("ok", true, "result", value, "result_type", resultType)
}

// Re-attaches the page session to an open page on origin, the first tab where accept is truthy winning
func (s *cdpSupervisor) focusPage(origin, accept string, timeout float64) *pyjson.Object {
	if !s.currentLoop().isRunning() {
		return supervisorFail("supervisor loop is not running")
	}
	attach := func(targetID string) (string, error) {
		reply, err := s.cdpCall("Target.attachToTarget", pyjson.ObjectOf("targetId", targetID, "flatten", true), "", timeout)
		if err != nil {
			return "", err
		}
		result, err := objIndex(reply, "result")
		if err != nil {
			return "", err
		}
		sidVal, err := objIndex(result, "sessionId")
		if err != nil {
			return "", err
		}
		sid := pyrepr.Str(sidVal)
		if err := s.enablePageDomains(sid, timeout); err != nil {
			return "", err
		}
		s.installDialogBridge(sid)
		return sid, nil
	}
	focus := func() (any, error) {
		reply, err := s.cdpCall("Target.getTargets", nil, "", timeout)
		if err != nil {
			return nil, err
		}
		targets, _ := objChild(reply, "result").Get("targetInfos")
		type candidate struct{ targetID, url string }
		var candidates []candidate
		list, _ := targets.([]any)
		for _, t := range list {
			u := objStr(t, "url")
			if anyGet(t, "type") != "page" || !(strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")) {
				continue
			}
			if origin != "" {
				norm, err := vault.NormalizeOrigin(u)
				if err != nil || norm != origin {
					continue
				}
			}
			id, err := objIndex(t, "targetId")
			if err != nil {
				continue
			}
			candidates = append(candidates, candidate{pyrepr.Str(id), u})
		}
		for _, c := range candidates {
			sid, err := attach(c.targetID)
			if err != nil {
				return nil, err
			}
			if accept != "" {
				probe, err := s.cdpCall("Runtime.evaluate", pyjson.ObjectOf("expression", accept, "returnByValue", true), sid, timeout)
				if err != nil {
					return nil, err
				}
				if !pyrepr.Truthy(anyGet(objChild(objChild(probe, "result"), "result"), "value")) {
					if _, err := s.cdpCall("Target.detachFromTarget", pyjson.ObjectOf("sessionId", sid), "", timeout); err != nil {
						return nil, err
					}
					continue
				}
			}
			s.setPageSession(sid)
			return pyjson.ObjectOf("ok", true, "url", c.url), nil
		}
		where := origin
		if where == "" {
			where = "any site"
		}
		suffix := ""
		if accept != "" && len(candidates) > 0 {
			suffix = " with the expected form"
		}
		return supervisorFail("no open page on " + where + suffix), nil
	}
	out, err := s.schedule(focus, timeout+1, false)
	if err != nil {
		return supervisorErr(err)
	}
	return out.(*pyjson.Object)
}

// Thread entry running the reconnecting supervisor until it returns, then closing its loop
func (s *cdpSupervisor) threadMain(done chan struct{}) {
	loop := newSupervisorLoop()
	quit := make(chan struct{})
	s.ctlMu.Lock()
	s.loop = loop
	s.ctlMu.Unlock()
	go loop.serve(quit)
	for !loop.isRunning() && !loop.isClosed() {
		runtime.Gosched()
	}
	err := catchRaised(s.run)
	if err != nil && !s.failStart(err) {
		s.log.Warn(fmt.Sprintf("CDP supervisor %s crashed: %s", s.taskID, pyrepr.ExceptionMessage(err)))
	}
	close(quit)
	<-loop.stopped
	s.setActive(false)
	close(done)
}

// Hands err to start when attach never completed, true when it was consumed that way
func (s *cdpSupervisor) failStart(err error) bool {
	if s.ready.IsSet() {
		return false
	}
	s.ctlMu.Lock()
	s.startErr = err
	s.ctlMu.Unlock()
	s.ready.Set()
	return true
}

// Detaches and closes the current socket, close errors swallowed
func (s *cdpSupervisor) closeWS() {
	s.wsMu.Lock()
	ws := s.ws
	s.ws = nil
	s.wsMu.Unlock()
	if wc, ok := ws.(*WSConn); ok {
		_ = wc.SendClose(1000, "")
		wc.ForceClose()
	} else if ws != nil {
		_ = ws.Close(1000, "")
	}
}

// True once failures consecutive reconnects failed, logging once and leaving the registry
func (s *cdpSupervisor) reconnectBudgetSpent(failures int, err error) bool {
	if failures < maxPostAttachReconnectFailures {
		return false
	}
	s.log.Warn(fmt.Sprintf("CDP supervisor %s: stopped after %d failed reconnect attempts: %s",
		s.taskID, failures, redactCDPErrorText(pyrepr.ExceptionMessage(err))))
	if supervisors.get(s.taskID) == s {
		supervisors.pop(s.taskID)
	}
	return true
}

// Seams the reconnect loop dials, attaches, reads and backs off through
var (
	supervisorDial = func(rawURL string) (supervisorWS, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ws, err := DialWS(ctx, rawURL, WSOptions{MaxMessageSize: supervisorMaxMessage, HandshakeTimeout: 10 * time.Second})
		if err != nil {
			return nil, pyWSError(err, rawURL)
		}
		return ws, nil
	}
	supervisorAttachInitialPage = (*cdpSupervisor).attachInitialPage
	supervisorReadLoop          = (*cdpSupervisor).readLoop
	supervisorSleep             = func(stop <-chan struct{}, d time.Duration) {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-stop:
		}
	}
	supervisorCDP = (*cdpSupervisor).sendCDP
)

func (s *cdpSupervisor) stopChan() <-chan struct{} {
	s.ctlMu.Lock()
	defer s.ctlMu.Unlock()
	return s.stopCh
}

// Reconnecting supervisor that re-attaches after a drop, a failure before the first attach fatal for start
func (s *cdpSupervisor) run() error {
	failures, lastSuccess, backoff := 0, 0.0, 0.5
	for !s.stopRequested.Load() {
		ws, err := supervisorDial(s.cdpURL)
		if err != nil {
			if s.failStart(err) {
				return nil
			}
			failures++
			if s.reconnectBudgetSpent(failures, err) {
				return nil
			}
			s.log.Warn(fmt.Sprintf("CDP supervisor %s: connect failed (attempt %d/%d): %s", s.taskID, failures,
				maxPostAttachReconnectFailures, redactCDPErrorText(pyrepr.ExceptionMessage(err))))
			supervisorSleep(s.stopChan(), seconds(min(backoff, 10.0)))
			backoff = min(backoff*2, 10.0)
			continue
		}
		s.wsMu.Lock()
		s.ws = ws
		s.wsMu.Unlock()
		readCtx, cancelRead := context.WithCancel(context.Background())
		readerDone := make(chan struct{})
		go func() {
			defer close(readerDone)
			supervisorReadLoop(s, readCtx)
		}()
		attachErr := catchRaised(func() error {
			s.setPageSession("")
			if err := supervisorAttachInitialPage(s); err != nil {
				return err
			}
			s.setActive(true)
			lastSuccess = float64(time.Now().UnixNano()) / 1e9
			failures, backoff = 0, 0.5
			s.ready.Set()
			<-readerDone
			return nil
		})
		cleanup := func() {
			s.setActive(false)
			cancelRead()
			<-readerDone
			s.wdMu.Lock()
			for id, t := range s.watchdogs {
				t.Stop()
				delete(s.watchdogs, id)
			}
			s.wdMu.Unlock()
			s.closeWS()
			s.wsMu.Lock()
			for id, ch := range s.pendingCalls {
				select {
				case ch <- supervisorReply{err: pyError("ConnectionError", "CDP connection closed before reply")}:
				default:
				}
				delete(s.pendingCalls, id)
			}
			s.wsMu.Unlock()
		}
		if attachErr != nil {
			if s.failStart(attachErr) {
				cleanup()
				return attachErr
			}
			failures++
			if s.reconnectBudgetSpent(failures, attachErr) {
				cleanup()
				return nil
			}
			s.log.Warn(fmt.Sprintf("CDP supervisor %s: session dropped after %.1fs (attempt %d/%d): %s", s.taskID,
				float64(time.Now().UnixNano())/1e9-lastSuccess, failures, maxPostAttachReconnectFailures,
				redactCDPErrorText(pyrepr.ExceptionMessage(attachErr))))
		}
		cleanup()
		if s.stopRequested.Load() {
			return nil
		}
		s.log.Debug(fmt.Sprintf("CDP supervisor %s: reconnecting in %.1fs...", s.taskID, backoff))
		supervisorSleep(s.stopChan(), seconds(backoff))
		backoff = min(backoff*2, 10.0)
	}
	return nil
}

// Attaches flattened to the first page target or a new blank one, then enables domains and the dialog bridge
func (s *cdpSupervisor) attachInitialPage() error {
	reply, err := s.cdpCall("Target.getTargets", nil, "", 10.0)
	if err != nil {
		return err
	}
	targets, _ := objChild(reply, "result").Get("targetInfos")
	var page any
	list, _ := targets.([]any)
	for _, t := range list {
		if anyGet(t, "type") == "page" {
			page = t
			break
		}
	}
	if page == nil {
		created, err := s.cdpCall("Target.createTarget", pyjson.ObjectOf("url", "about:blank"), "", 10.0)
		if err != nil {
			return err
		}
		if page, err = objIndex(created, "result"); err != nil {
			return err
		}
	}
	targetID, err := objIndex(page, "targetId")
	if err != nil {
		return err
	}
	attach, err := s.cdpCall("Target.attachToTarget", pyjson.ObjectOf("targetId", targetID, "flatten", true), "", 10.0)
	if err != nil {
		return err
	}
	result, err := objIndex(attach, "result")
	if err != nil {
		return err
	}
	sidVal, err := objIndex(result, "sessionId")
	if err != nil {
		return err
	}
	sid := pyrepr.Str(sidVal)
	s.setPageSession(sid)
	if err := s.enablePageDomains(sid, 10.0); err != nil {
		return err
	}
	s.installDialogBridge(sid)
	return nil
}

// Sends one CDP command and waits timeout seconds for its reply, a negative timeout waiting until the socket closes
func (s *cdpSupervisor) cdpCall(method string, params any, sessionID string, timeout float64) (*pyjson.Object, error) {
	return supervisorCDP(s, method, params, sessionID, timeout)
}

func (s *cdpSupervisor) sendCDP(method string, params any, sessionID string, timeout float64) (*pyjson.Object, error) {
	s.wsMu.Lock()
	ws := s.ws
	if ws == nil {
		s.wsMu.Unlock()
		return nil, runtimeError("supervisor WebSocket is not connected")
	}
	s.nextCallID++
	id := s.nextCallID
	payload := pyjson.ObjectOf("id", id, "method", method)
	if pyrepr.Truthy(params) {
		payload.Set("params", params)
	}
	if sessionID != "" {
		payload.Set("sessionId", sessionID)
	}
	ch := make(chan supervisorReply, 1)
	s.pendingCalls[id] = ch
	s.wsMu.Unlock()
	defer func() {
		s.wsMu.Lock()
		delete(s.pendingCalls, id)
		s.wsMu.Unlock()
	}()
	text, err := pyjson.Dumps(payload)
	if err != nil {
		return nil, err
	}
	if err := ws.WriteText(context.Background(), []byte(text)); err != nil {
		return nil, pyWSError(err, s.cdpURL)
	}
	var expire <-chan time.Time
	if timeout >= 0 {
		t := time.NewTimer(seconds(timeout))
		defer t.Stop()
		expire = t.C
	}
	select {
	case r := <-ch:
		return r.msg, r.err
	case <-expire:
		return nil, timeoutError("")
	}
}

// Dispatches incoming frames, replies to their callers and events to their handlers on the loop
func (s *cdpSupervisor) readLoop(ctx context.Context) {
	ws := s.currentWS()
	if ws == nil {
		return
	}
	for {
		_, raw, err := ws.ReadMessage(ctx)
		if err != nil {
			var closed *ErrWSClosed
			if !(errors.As(err, &closed) && (closed.Code == 1000 || closed.Code == 1001)) && ctx.Err() == nil {
				s.log.Debug(fmt.Sprintf("CDP read loop exited: %s", pyrepr.ExceptionMessage(pyWSError(err, s.cdpURL))))
			}
			return
		}
		if s.stopRequested.Load() {
			return
		}
		decoded, err := pyjson.LoadsBytes(raw)
		if err != nil {
			s.log.Debug("CDP supervisor: non-JSON frame dropped")
			continue
		}
		msg, ok := decoded.(*pyjson.Object)
		if !ok {
			s.log.Debug(fmt.Sprintf("CDP read loop exited: '%s' object has no attribute 'get'", pyrepr.TypeName(decoded)))
			return
		}
		loop := s.currentLoop()
		next := make(chan func(), 1)
		if loop == nil || !loop.submit(func() { next <- s.dispatch(msg) }) {
			return
		}
		select {
		case cont := <-next:
			if cont != nil {
				cont()
			}
		case <-loop.stopped:
			return
		}
	}
}

// Resolves a reply or runs an event handler on the loop, returning the handler's awaited remainder
func (s *cdpSupervisor) dispatch(msg *pyjson.Object) func() {
	if idVal, ok := msg.Get("id"); ok {
		id, isInt := pyrepr.Int(idVal)
		if !isInt || !id.IsInt64() {
			return nil
		}
		s.wsMu.Lock()
		ch := s.pendingCalls[id.Int64()]
		delete(s.pendingCalls, id.Int64())
		s.wsMu.Unlock()
		if ch == nil {
			return nil
		}
		if errVal, has := msg.Get("error"); has {
			ch <- supervisorReply{err: runtimeError(fmt.Sprintf("CDP error on id=%s: %s", pyrepr.Str(idVal), pyrepr.Repr(errVal)))}
		} else {
			ch <- supervisorReply{msg: msg}
		}
		return nil
	}
	method, _ := msg.Get("method")
	name, _ := method.(string)
	handler := supervisorEventHandlers[name]
	if handler == nil {
		return nil
	}
	params, ok := pyjson.AsObject(anyGet(msg, "params"))
	if !ok {
		params = pyjson.NewObject()
	}
	sid, _ := anyGet(msg, "sessionId").(string)
	return handler(s, params, sid)
}

var supervisorEventHandlers map[string]func(*cdpSupervisor, *pyjson.Object, string) func()

func init() {
	supervisorEventHandlers = map[string]func(*cdpSupervisor, *pyjson.Object, string) func(){
		"Page.javascriptDialogOpening": (*cdpSupervisor).onDialogOpening,
		"Page.javascriptDialogClosed":  (*cdpSupervisor).onDialogClosed,
		"Fetch.requestPaused":          (*cdpSupervisor).onFetchPaused,
		"Page.frameAttached":           (*cdpSupervisor).onFrameAttached,
		"Page.frameNavigated":          (*cdpSupervisor).onFrameNavigated,
		"Page.frameDetached":           (*cdpSupervisor).onFrameDetached,
		"Target.attachedToTarget":      (*cdpSupervisor).onTargetAttached,
		"Target.detachedFromTarget":    (*cdpSupervisor).onTargetDetached,
	}
}

// CDP call whose failure is only logged at debug
func (s *cdpSupervisor) cdpQuiet(method string, params any, sessionID string, timeout float64, what string) {
	if _, err := s.cdpCall(method, params, sessionID, timeout); err != nil {
		s.log.Debug(fmt.Sprintf("%s failed (%s): %s", method, what, pyrepr.ExceptionMessage(err)))
	}
}

// Installs the dialog bridge script and its Fetch interceptor on a session, then injects it into the loaded page
func (s *cdpSupervisor) installDialogBridge(sessionID string) {
	sid := []rune(sessionID)
	if len(sid) > 16 {
		sid = sid[:16]
	}
	what := "dialog bridge sid=" + string(sid)
	s.cdpQuiet("Page.addScriptToEvaluateOnNewDocument", pyjson.ObjectOf("source", dialogBridgeScript, "runImmediately", true), sessionID, 5.0, what)
	s.cdpQuiet("Fetch.enable", pyjson.ObjectOf("patterns", []any{pyjson.ObjectOf("urlPattern", dialogBridgeURLPattern, "requestStage", "Request")},
		"handleAuthRequests", false), sessionID, 5.0, what)
	s.cdpQuiet("Runtime.evaluate", pyjson.ObjectOf("expression", dialogBridgeScript, "returnByValue", true), sessionID, 3.0,
		"dialog bridge inject sid="+string(sid))
}

func (s *cdpSupervisor) onDialogOpening(params *pyjson.Object, sessionID string) func() {
	frameID, _ := params.Get("frameId")
	s.admitDialog(objStr(params, "type"), objStr(params, "message"), objStr(params, "defaultPrompt"), sessionID, frameID, "")
	return nil
}

// Bridge XHR becomes a pending dialog, any other paused request continues unchanged
func (s *cdpSupervisor) onFetchPaused(params *pyjson.Object, sessionID string) func() {
	rawURL := objStr(objChild(params, "request"), "url")
	requestID, _ := params.Get("requestId")
	if !pyrepr.Truthy(requestID) {
		return nil
	}
	if !strings.Contains(rawURL, dialogBridgeHost) {
		return func() {
			s.cdpQuiet("Fetch.continueRequest", pyjson.ObjectOf("requestId", requestID), sessionID, 3.0, "passthrough")
		}
	}
	query := map[string]string{}
	if u, err := url.Parse(rawURL); err == nil {
		for k, v := range parseQS(u.RawQuery) {
			query[k] = v
		}
	}
	kind := query["kind"]
	if kind == "" {
		kind = "alert"
	}
	frameID, _ := params.Get("frameId")
	s.admitDialog(kind, query["message"], query["default_prompt"], sessionID, frameID, pyrepr.Str(requestID))
	return nil
}

// First value of each non blank query field as parse_qs keeps them
func parseQS(raw string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == '&' }) {
		k, v, _ := strings.Cut(part, "=")
		key, err1 := url.QueryUnescape(k)
		val, err2 := url.QueryUnescape(v)
		if err1 != nil || err2 != nil || val == "" {
			continue
		}
		if _, seen := out[key]; !seen {
			out[key] = val
		}
	}
	return out
}

// Creates a dialog and applies the policy, auto responding or queueing it with a watchdog
func (s *cdpSupervisor) admitDialog(kind, message, defaultPrompt, sessionID string, frameID any, bridgeRequestID string) {
	s.wdMu.Lock()
	s.dialogSeq++
	seq := s.dialogSeq
	s.wdMu.Unlock()
	cdpSession := sessionID
	if cdpSession == "" {
		cdpSession = s.pageSession()
	}
	dialog := PendingDialog{ID: fmt.Sprintf("d-%d", seq), Type: kind, Message: message, DefaultPrompt: defaultPrompt,
		OpenedAt: float64(time.Now().UnixNano()) / 1e9, CDPSessionID: cdpSession, FrameID: frameID, BridgeRequestID: bridgeRequestID}
	if s.dialogPolicy == DialogPolicyAutoDismiss || s.dialogPolicy == DialogPolicyAutoAccept {
		accept, prompt := false, ""
		if s.dialogPolicy == DialogPolicyAutoAccept {
			accept, prompt = true, defaultPrompt
		}
		s.stateMu.Lock()
		s.archiveDialogLocked(dialog, "auto_policy")
		s.stateMu.Unlock()
		go s.respondQuiet(dialog, accept, &prompt)
		return
	}
	s.stateMu.Lock()
	if _, ok := s.pendingDialogs[dialog.ID]; !ok {
		s.pendingKeys = append(s.pendingKeys, dialog.ID)
	}
	s.pendingDialogs[dialog.ID] = dialog
	s.stateMu.Unlock()
	id := dialog.ID
	s.wdMu.Lock()
	s.watchdogs[id] = time.AfterFunc(seconds(s.dialogTimeoutS), func() { go s.dialogTimeoutExpired(id) })
	s.wdMu.Unlock()
}

// Bridge fulfil for an XHR dialog or the native dialog answer, raising on a native CDP failure
func (s *cdpSupervisor) respond(dialog PendingDialog, accept bool, promptText *string) error {
	if dialog.BridgeRequestID != "" {
		text := ""
		if dialog.Type == "prompt" && promptText != nil {
			text = *promptText
		}
		body, err := pyjson.Dumps(pyjson.ObjectOf("accept", accept, "dialog_id", dialog.ID, "prompt_text", text))
		if err != nil {
			return err
		}
		s.cdpQuiet("Fetch.fulfillRequest", pyjson.ObjectOf("requestId", dialog.BridgeRequestID, "responseCode", 200,
			"responseHeaders", []any{pyjson.ObjectOf("name", "Content-Type", "value", "application/json"),
				pyjson.ObjectOf("name", "Access-Control-Allow-Origin", "value", "*")},
			"body", base64.StdEncoding.EncodeToString([]byte(body))), dialog.CDPSessionID, 5.0, "bridge fulfill "+dialog.ID)
		return nil
	}
	params := pyjson.ObjectOf("accept", accept)
	if promptText != nil && dialog.Type == "prompt" {
		params.Set("promptText", *promptText)
	}
	_, err := s.cdpCall("Page.handleJavaScriptDialog", params, dialog.CDPSessionID, 5.0)
	return err
}

// Auto policy or watchdog answer whose failure is only logged
func (s *cdpSupervisor) respondQuiet(dialog PendingDialog, accept bool, promptText *string) {
	if err := s.respond(dialog, accept, promptText); err != nil {
		s.log.Debug(fmt.Sprintf("auto response failed for %s: %s", dialog.ID, pyrepr.ExceptionMessage(err)))
	}
}

// Agent answer, the dialog retired whatever the CDP outcome
func (s *cdpSupervisor) handleDialogCDP(dialog PendingDialog, accept bool, promptText string) error {
	defer s.retireDialog(dialog.ID, "agent")
	return s.respond(dialog, accept, &promptText)
}

func (s *cdpSupervisor) dialogTimeoutExpired(dialogID string) {
	s.stateMu.Lock()
	dialog, ok := s.pendingDialogs[dialogID]
	s.stateMu.Unlock()
	if !ok {
		return
	}
	s.log.Warn(fmt.Sprintf("CDP supervisor %s: dialog %s (%s) auto-dismissed after %ss timeout", s.taskID, dialogID, dialog.Type,
		pyrepr.Float(s.dialogTimeoutS)))
	s.retireDialog(dialogID, "watchdog")
	s.respondQuiet(dialog, false, nil)
}

// Removes a pending dialog into the recent ring and cancels its watchdog
func (s *cdpSupervisor) retireDialog(dialogID, closedBy string) {
	s.stateMu.Lock()
	if dialog, ok := s.pendingDialogs[dialogID]; ok {
		delete(s.pendingDialogs, dialogID)
		for i, k := range s.pendingKeys {
			if k == dialogID {
				s.pendingKeys = append(s.pendingKeys[:i:i], s.pendingKeys[i+1:]...)
				break
			}
		}
		s.archiveDialogLocked(dialog, closedBy)
	}
	s.stateMu.Unlock()
	s.wdMu.Lock()
	if t := s.watchdogs[dialogID]; t != nil {
		t.Stop()
	}
	delete(s.watchdogs, dialogID)
	s.wdMu.Unlock()
}

func (s *cdpSupervisor) archiveDialogLocked(d PendingDialog, closedBy string) {
	record := DialogRecord{ID: d.ID, Type: d.Type, Message: d.Message, OpenedAt: d.OpenedAt,
		ClosedAt: float64(time.Now().UnixNano()) / 1e9, ClosedBy: closedBy, FrameID: d.FrameID}
	s.recentDialogs = trimDialogRing(append(append([]DialogRecord(nil), s.recentDialogs...), record), recentDialogsMax)
}

func (s *cdpSupervisor) onDialogClosed(_ *pyjson.Object, sessionID string) func() {
	s.stateMu.Lock()
	candidate := ""
	for _, k := range s.pendingKeys {
		d := s.pendingDialogs[k]
		if d.CDPSessionID == sessionID && d.BridgeRequestID == "" {
			candidate = d.ID
			break
		}
	}
	s.stateMu.Unlock()
	if candidate != "" {
		s.retireDialog(candidate, "remote")
	}
	return nil
}

// Page and Runtime enabled with nested auto attach on one session
func (s *cdpSupervisor) enablePageDomains(sessionID string, timeout float64) error {
	if _, err := s.cdpCall("Page.enable", nil, sessionID, timeout); err != nil {
		return err
	}
	if _, err := s.cdpCall("Runtime.enable", nil, sessionID, timeout); err != nil {
		return err
	}
	_, err := s.cdpCall("Target.setAutoAttach", autoAttachParams, sessionID, timeout)
	return err
}

func (s *cdpSupervisor) onFrameAttached(params *pyjson.Object, sessionID string) func() {
	frameID := objStr(params, "frameId")
	if frameID != "" {
		s.stateMu.Lock()
		s.frames.set(FrameInfo{FrameID: frameID, ParentFrameID: objStr(params, "parentFrameId"), CDPSessionID: sessionID})
		s.stateMu.Unlock()
	}
	return nil
}

func (s *cdpSupervisor) onFrameNavigated(params *pyjson.Object, sessionID string) func() {
	frame := objChild(params, "frame")
	frameID := objStr(frame, "id")
	if frameID == "" {
		return nil
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	old, ok := s.frames.get(frameID)
	if !ok {
		old = FrameInfo{FrameID: frameID, CDPSessionID: sessionID}
	}
	origin := objStr(frame, "securityOrigin")
	if origin == "" {
		origin = objStr(frame, "origin")
	}
	parent := objStr(frame, "parentId")
	if parent == "" {
		parent = old.ParentFrameID
	}
	name := objStr(frame, "name")
	if name == "" {
		name = old.Name
	}
	s.frames.set(FrameInfo{FrameID: frameID, URL: objStr(frame, "url"), Origin: origin, ParentFrameID: parent,
		IsOOPIF: old.IsOOPIF, CDPSessionID: old.CDPSessionID, Name: name})
	return nil
}

// Drops a frame only when truly gone, never on a process swap or while its child session lives
func (s *cdpSupervisor) onFrameDetached(params *pyjson.Object, _ string) func() {
	frameID := objStr(params, "frameId")
	reason := objStr(params, "reason")
	if reason == "" {
		reason = "remove"
	}
	if frameID == "" || pystr.Lower(reason) == "swap" {
		return nil
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if old, ok := s.frames.get(frameID); !(ok && old.IsOOPIF && old.CDPSessionID != "") {
		s.frames.pop(frameID)
	}
	return nil
}

func (s *cdpSupervisor) onTargetAttached(params *pyjson.Object, _ string) func() {
	info := objChild(params, "targetInfo")
	sid := objStr(params, "sessionId")
	targetType, _ := info.Get("type")
	if sid == "" || (targetType != "iframe" && targetType != "worker") {
		return nil
	}
	if targetType == "iframe" {
		targetID := objStr(info, "targetId")
		s.stateMu.Lock()
		old, ok := s.frames.get(targetID)
		name := objStr(info, "title")
		parent := ""
		if ok {
			parent = old.ParentFrameID
			if name == "" {
				name = old.Name
			}
		}
		s.frames.set(FrameInfo{FrameID: targetID, URL: objStr(info, "url"), IsOOPIF: true, CDPSessionID: sid, ParentFrameID: parent, Name: name})
		s.stateMu.Unlock()
	}
	go s.enableChildDomains(sid)
	return nil
}

// Page, Runtime and nested auto attach plus the dialog bridge on a child session
func (s *cdpSupervisor) enableChildDomains(sid string) {
	if err := s.enablePageDomains(sid, 3.0); err != nil {
		short := []rune(sid)
		if len(short) > 16 {
			short = short[:16]
		}
		s.log.Debug(fmt.Sprintf("child session %s setup failed: %s", string(short), pyrepr.ExceptionMessage(err)))
	}
	s.installDialogBridge(sid)
}

// Clears the session binding of frames on a detached child session, keeping the frames
func (s *cdpSupervisor) onTargetDetached(params *pyjson.Object, _ string) func() {
	sid := objStr(params, "sessionId")
	if sid == "" {
		return nil
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	for _, f := range s.frames.values() {
		if f.CDPSessionID == sid {
			f.CDPSessionID = ""
			s.frames.set(f)
		}
	}
	return nil
}

// Capped breadth first frame tree from the top frame, state lock held
func (s *cdpSupervisor) buildFrameTreeLocked() *pyjson.Object {
	frames := s.frames.values()
	var tops []FrameInfo
	for _, f := range frames {
		if f.ParentFrameID == "" {
			tops = append(tops, f)
		}
	}
	if len(tops) == 0 {
		return pyjson.ObjectOf("top", nil, "children", []any{}, "truncated", false)
	}
	top := tops[0]
	for _, f := range tops {
		if !f.IsOOPIF {
			top = f
			break
		}
	}
	type queued struct {
		frame FrameInfo
		depth int
	}
	var queue []queued
	for _, f := range frames {
		if f.ParentFrameID == top.FrameID {
			queue = append(queue, queued{f, 1})
		}
	}
	children := []any{}
	truncated := false
	visited := map[string]bool{top.FrameID: true}
	for len(queue) > 0 && len(children) < frameTreeMaxEntries {
		q := queue[0]
		queue = queue[1:]
		if visited[q.frame.FrameID] {
			continue
		}
		visited[q.frame.FrameID] = true
		if q.frame.IsOOPIF && q.depth > frameTreeMaxOOPIFDepth {
			truncated = true
			continue
		}
		children = append(children, q.frame.toDict())
		for _, f := range frames {
			if f.ParentFrameID == q.frame.FrameID && !visited[f.FrameID] {
				queue = append(queue, queued{f, q.depth + 1})
			}
		}
	}
	return pyjson.ObjectOf("top", top.toDict(), "children", children, "truncated", truncated || len(queue) > 0)
}

// Process wide task to supervisor map with idempotent start and stop
type supervisorRegistry struct {
	mu         sync.Mutex
	byTask     map[string]*cdpSupervisor
	homeByTask map[string]string
}

func newSupervisorRegistry() *supervisorRegistry {
	return &supervisorRegistry{byTask: map[string]*cdpSupervisor{}, homeByTask: map[string]string{}}
}

var supervisors = newSupervisorRegistry()

func (r *supervisorRegistry) get(taskID string) *cdpSupervisor {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byTask[taskID]
}

func (r *supervisorRegistry) pop(taskID string) *cdpSupervisor {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.homeByTask, taskID)
	s := r.byTask[taskID]
	delete(r.byTask, taskID)
	return s
}

// Ensures one healthy supervisor runs for the task and endpoint, replacing a stale or retargeted one
func (r *supervisorRegistry) getOrStart(env *tools.Env, taskID, cdpURL, policy string, dialogTimeoutS, startTimeout float64) (*cdpSupervisor, error) {
	homeKey := cfgOf(env).homeKey()
	r.mu.Lock()
	existing := r.byTask[taskID]
	if existing != nil {
		if existing.cdpURL == cdpURL && existing.healthy() {
			r.mu.Unlock()
			return existing, nil
		}
		delete(r.byTask, taskID)
	}
	r.mu.Unlock()
	if existing != nil {
		existing.stop(5.0)
	}
	sup, err := newCDPSupervisor(env, taskID, cdpURL, policy, dialogTimeoutS)
	if err != nil {
		return nil, err
	}
	if err := supervisorStart(sup, startTimeout); err != nil {
		return nil, err
	}
	r.mu.Lock()
	if already := r.byTask[taskID]; already != nil && already.cdpURL == cdpURL {
		r.mu.Unlock()
		sup.stop(5.0)
		return already, nil
	}
	r.byTask[taskID] = sup
	r.homeByTask[taskID] = homeKey
	r.mu.Unlock()
	return sup, nil
}

func (r *supervisorRegistry) stop(taskID string) {
	if s := r.pop(taskID); s != nil {
		s.stop(5.0)
	}
}

// Stops the supervisors one profile home started
func (r *supervisorRegistry) stopHome(homeKey string) {
	r.mu.Lock()
	var tasks []string
	for task, h := range r.homeByTask {
		if h == homeKey {
			tasks = append(tasks, task)
		}
	}
	r.mu.Unlock()
	for _, task := range tasks {
		r.stop(task)
	}
}

// Stops every running supervisor
func (r *supervisorRegistry) stopAll() {
	r.mu.Lock()
	items := make([]*cdpSupervisor, 0, len(r.byTask))
	for _, s := range r.byTask {
		items = append(items, s)
	}
	r.byTask, r.homeByTask = map[string]*cdpSupervisor{}, map[string]string{}
	r.mu.Unlock()
	for _, s := range items {
		s.stop(5.0)
	}
}

// Starts the task's supervisor on cdpURL under the dialog policy, the start error returned
func supervisorGetOrStart(env *tools.Env, taskID, cdpURL, policy string, timeoutS float64) error {
	_, err := supervisors.getOrStart(env, taskID, cdpURL, policy, timeoutS, supervisorStartTimeoutS)
	return err
}

func supervisorStop(taskID string) { supervisors.stop(taskID) }

func supervisorStopHome(homeKey string) { supervisors.stopHome(homeKey) }

func supervisorStopAll() { supervisors.stopAll() }

// CDP channel pinned to one supervisor connection, invalid once it reconnects, stops or is replaced
type CapturedCDP struct {
	sup                           *cdpSupervisor
	registry                      *supervisorRegistry
	loop                          *supervisorLoop
	ws                            supervisorWS
	TaskID, CDPURL, PageSessionID string
}

// True while the captured connection is still the supervisor's live one
func (c *CapturedCDP) IsValid() bool {
	sup := c.sup
	return c.registry.get(c.TaskID) == sup && sup.isActive() && !sup.stopRequested.Load() && sup.currentWS() == c.ws && c.loop.isRunning()
}

func (c *CapturedCDP) invalid() error {
	return pyError("CapturedCDPInvalid", fmt.Sprintf("captured CDP connection for task %s is gone "+
		"(supervisor reconnected, stopped or was replaced); capture again", pyrepr.StrRepr(c.TaskID)))
}

const threadingTimeoutMax = 9223372036.854776

// Seam the captured dispatch is queued through
var submitCaptureSend = func(loop *supervisorLoop, fn func()) bool { return loop.submit(fn) }

// Sends method on the captured connection and returns the raw reply, nil timeout waiting until the socket closes
func (c *CapturedCDP) Call(method string, params any, sessionID string, timeout any, beforeSend func() any) (*pyjson.Object, error) {
	var limit float64
	if timeout != nil {
		switch t := timeout.(type) {
		case int:
			limit = float64(t)
		case float64:
			limit = t
		default:
			return nil, pyError("ValueError", "Captured CDP timeout must be positive and finite, or None")
		}
		if limit != limit || limit <= 0 || limit > threadingTimeoutMax-1.0 {
			return nil, pyError("ValueError", "Captured CDP timeout must be positive and finite, or None")
		}
	}
	var deadline time.Time
	if timeout != nil {
		deadline = time.Now().Add(seconds(limit))
	}
	if c.loop.onLoop() {
		return nil, runtimeError("CapturedCDP.call() blocks and cannot run on the supervisor loop")
	}
	if !c.IsValid() {
		return nil, c.invalid()
	}
	res := make(chan supervisorReply, 1)
	expired := func() bool { return timeout != nil && !time.Now().Before(deadline) }
	send := func() {
		if !c.IsValid() {
			res <- supervisorReply{err: c.invalid()}
			return
		}
		if expired() {
			res <- supervisorReply{err: timeoutError("Captured CDP dispatch deadline expired")}
			return
		}
		if beforeSend != nil {
			if out := beforeSend(); out != nil {
				if err, ok := out.(error); ok {
					res <- supervisorReply{err: err}
				} else {
					res <- supervisorReply{err: pyError("TypeError", "before_send must return None synchronously")}
				}
				return
			}
		}
		if expired() {
			res <- supervisorReply{err: timeoutError("Captured CDP dispatch deadline expired")}
			return
		}
		if !c.IsValid() {
			res <- supervisorReply{err: c.invalid()}
			return
		}
		wait := supervisorNoTimeout
		if timeout != nil {
			wait = limit
		}
		go func() {
			msg, err := c.sup.cdpCall(method, params, sessionID, wait)
			if err != nil && !c.IsValid() {
				err = c.invalid()
			}
			res <- supervisorReply{msg, err}
		}()
	}
	if !submitCaptureSend(c.loop, send) {
		return nil, c.invalid()
	}
	if timeout == nil {
		for {
			select {
			case r := <-res:
				return r.msg, r.err
			case <-time.After(time.Second):
				if c.loop.isClosed() {
					return nil, c.invalid()
				}
			}
		}
	}
	select {
	case r := <-res:
		return r.msg, r.err
	case <-time.After(seconds(limit + 1.0)):
		shown := pyrepr.Float(limit)
		if _, isInt := timeout.(int); isInt {
			shown = strconv.Itoa(timeout.(int))
		}
		return nil, timeoutError(fmt.Sprintf("CDP %s got no reply within %ss", method, shown))
	}
}

// Pins the running supervisor of a task, never starting, reconnecting or refocusing one
func (r *supervisorRegistry) capture(taskID string, timeout float64) (*CapturedCDP, error) {
	gone := pyError("CapturedCDPInvalid", "no running browser supervisor for task "+pyrepr.StrRepr(taskID))
	found := r.get(taskID)
	if found == nil || !found.currentLoop().isRunning() {
		return nil, gone
	}
	loop := found.currentLoop()
	out, err := found.schedule(func() (any, error) {
		if !found.isActive() || found.currentWS() == nil || found.pageSession() == "" {
			return nil, pyError("CapturedCDPInvalid", "browser supervisor for task "+pyrepr.StrRepr(taskID)+" has no attached page")
		}
		return &CapturedCDP{sup: found, registry: r, loop: loop, ws: found.currentWS(), TaskID: found.taskID,
			CDPURL: found.cdpURL, PageSessionID: found.pageSession()}, nil
	}, timeout, true)
	if errors.Is(err, errLoopUnavailable) {
		return nil, gone
	}
	if err != nil {
		return nil, err
	}
	return out.(*CapturedCDP), nil
}

// Captured CDP handle of a task's running supervisor for trusted in process plugins
func CaptureSupervisor(taskID string, timeout float64) (*CapturedCDP, error) {
	return supervisors.capture(taskID, timeout)
}
