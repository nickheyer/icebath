package tools

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	browserconfig "github.com/nickheyer/icebath/internal/browser/config"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/tools/redact"
	"github.com/nickheyer/icebath/internal/vault"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// Reconnects that may fail in a row after the first attach before the supervisor gives up
	reconnectBudget = 5
	recentDialogs   = 20
	frameEntries    = 30
	oopifDepth      = 2
	bridgeHost      = "icebath-dialog-bridge.invalid"
	bridgePattern   = "http://" + bridgeHost + "/*"
	supervisorStart = 15 * time.Second
	supervisorMax   = 50 << 20
)

// Script that routes alert, confirm and prompt through a synchronous request the supervisor answers, for pages where the native dialog
// event never fires
const bridgeScript = `
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

var autoAttach = map[string]any{"autoAttach": true, "waitForDebuggerOnStart": false, "flatten": true}

// Page text safe to show, any secret in it masked
func pageText(text string) string { return redact.Text(text, false, redact.Ends) }

// A dialog open on some frame's session
type pendingDialog struct {
	ID, Type, Message, DefaultPrompt string
	OpenedAt                         float64
	Session                          string
	FrameID                          any
	// Request of the bridge it came through, empty for a native dialog
	Bridge string
}

func (d pendingDialog) view() map[string]any {
	return map[string]any{"id": d.ID, "type": d.Type, "message": pageText(d.Message), "default_prompt": pageText(d.DefaultPrompt),
		"opened_at": d.OpenedAt, "frame_id": d.FrameID}
}

// A dialog that opened and was handled
type dialogRecord struct {
	ID, Type, Message  string
	OpenedAt, ClosedAt float64
	ClosedBy           string
	FrameID            any
}

func (r dialogRecord) view() map[string]any {
	return map[string]any{"id": r.ID, "type": r.Type, "message": pageText(r.Message), "opened_at": r.OpenedAt, "closed_at": r.ClosedAt,
		"closed_by": r.ClosedBy, "frame_id": r.FrameID}
}

// One frame of the page, an out of process one with its own session
type frameInfo struct {
	FrameID, URL, Origin, Parent string
	OOPIF                        bool
	Session, Name                string
}

func (f frameInfo) view() map[string]any {
	d := map[string]any{"frame_id": f.FrameID, "url": f.URL, "origin": f.Origin, "is_oopif": f.OOPIF}
	for k, v := range map[string]string{"session_id": f.Session, "parent_frame_id": f.Parent, "name": f.Name} {
		if v != "" {
			d[k] = v
		}
	}
	return d
}

// Frames by id in the order they were seen
type frames struct {
	keys []string
	m    map[string]frameInfo
}

func (t *frames) get(id string) (frameInfo, bool) {
	f, ok := t.m[id]
	return f, ok
}

func (t *frames) set(f frameInfo) {
	if t.m == nil {
		t.m = map[string]frameInfo{}
	}
	if _, ok := t.m[f.FrameID]; !ok {
		t.keys = append(t.keys, f.FrameID)
	}
	t.m[f.FrameID] = f
}

func (t *frames) pop(id string) {
	if _, ok := t.m[id]; ok {
		delete(t.m, id)
		t.keys = slices.DeleteFunc(t.keys, func(k string) bool { return k == id })
	}
}

func (t *frames) values() []frameInfo {
	out := make([]frameInfo, 0, len(t.keys))
	for _, k := range t.keys {
		out = append(out, t.m[k])
	}
	return out
}

// What the supervisor saw, for the tools
type watched struct {
	Pending []pendingDialog
	Recent  []dialogRecord
	Tree    map[string]any
	Active  bool
}

func (w watched) view() map[string]any {
	pending := make([]any, 0, len(w.Pending))
	for _, d := range w.Pending {
		pending = append(pending, d.view())
	}
	out := map[string]any{"pending_dialogs": pending, "frame_tree": w.Tree}
	if len(w.Recent) > 0 {
		recent := make([]any, 0, len(w.Recent))
		for _, r := range w.Recent {
			recent = append(recent, r.view())
		}
		out["recent_dialogs"] = recent
	}
	return out
}

func failed(msg string) map[string]any { return map[string]any{"ok": false, "error": msg} }

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// One connection per task that watches the page's dialogs and frames, reconnecting when it drops
type supervisor struct {
	task, cdp, policy string
	timeout           time.Duration

	mu      sync.Mutex
	keys    []string
	pending map[string]pendingDialog
	recent  []dialogRecord
	frames  frames
	active  bool
	page    string
	conn    *cdpConn
	seq     int
	dogs    map[string]*time.Timer
	stopped bool
	stop    chan struct{}
	done    chan struct{}
	// Told the first attach went or failed, once
	ready chan error
}

func newSupervisor(task, cdp, policy string, timeout time.Duration) (*supervisor, error) {
	if !slices.Contains([]string{browserconfig.AutoAccept, browserconfig.AutoDismiss, browserconfig.MustRespond}, policy) {
		return nil, fmt.Errorf("Invalid dialog_policy %q; must be one of ['auto_accept', 'auto_dismiss', 'must_respond']", policy)
	}
	return &supervisor{task: task, cdp: cdp, policy: policy, timeout: timeout, pending: map[string]pendingDialog{}, dogs: map[string]*time.Timer{},
		stop: make(chan struct{}), done: make(chan struct{}), ready: make(chan error, 1)}, nil
}

func (s *supervisor) healthy() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

func (s *supervisor) connection() *cdpConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn
}

// Starts the supervisor and waits for its first attach, the redacted failure when it doesn't
func (s *supervisor) start(timeout time.Duration, gone func(*supervisor)) error {
	go s.run(gone)
	select {
	case err := <-s.ready:
		if err != nil {
			s.end(5 * time.Second)
			return errors.New("CDP supervisor failed to start: " + redactEndpoint(err.Error()))
		}
		return nil
	case <-time.After(timeout):
		s.end(5 * time.Second)
		shown := []rune(redactEndpoint(s.cdp))
		if len(shown) > 80 {
			shown = shown[:80]
		}
		return fmt.Errorf("CDP supervisor did not attach within %gs (cdp_url=%s...)", timeout.Seconds(), string(shown))
	}
}

// Stops it and waits up to the timeout for its loop to end
func (s *supervisor) end(timeout time.Duration) {
	s.mu.Lock()
	if !s.stopped {
		s.stopped = true
		close(s.stop)
	}
	conn := s.conn
	s.mu.Unlock()
	if conn != nil {
		conn.close()
	}
	select {
	case <-s.done:
	case <-time.After(timeout):
	}
	s.mu.Lock()
	s.active = false
	s.mu.Unlock()
}

func (s *supervisor) halted() bool {
	select {
	case <-s.stop:
		return true
	default:
		return false
	}
}

func (s *supervisor) sleep(d time.Duration) {
	select {
	case <-time.After(d):
	case <-s.stop:
	}
}

// Whether the first attach is still awaited, handing it the failure when so
func (s *supervisor) failStart(err error) bool {
	select {
	case s.ready <- err:
		return true
	default:
		return false
	}
}

// Connects, attaches and watches until stopped; drops reconnect with backoff, a failure before the first attach ending it for start
func (s *supervisor) run(gone func(*supervisor)) {
	defer close(s.done)
	attached := false
	failures, backoff := 0, 500*time.Millisecond
	var since time.Time
	spent := func(err error) bool {
		if failures < reconnectBudget {
			return false
		}
		slog.Warn(fmt.Sprintf("CDP supervisor %s: stopped after %d failed reconnect attempts: %s", s.task, failures, redactEndpoint(err.Error())))
		gone(s)
		return true
	}
	for !s.halted() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		conn, err := dialCDP(ctx, s.cdp, supervisorMax)
		cancel()
		if err != nil {
			if !attached && s.failStart(err) {
				return
			}
			failures++
			if spent(err) {
				return
			}
			slog.Warn(fmt.Sprintf("CDP supervisor %s: connect failed (attempt %d/%d): %s", s.task, failures, reconnectBudget, redactEndpoint(err.Error())))
			s.sleep(min(backoff, 10*time.Second))
			backoff = min(backoff*2, 10*time.Second)
			continue
		}
		s.mu.Lock()
		s.conn, s.page = conn, ""
		s.mu.Unlock()
		off := conn.on(s.dispatch)
		err = s.attachInitial(conn)
		if err == nil {
			s.mu.Lock()
			s.active = true
			s.mu.Unlock()
			attached, since = true, time.Now()
			failures, backoff = 0, 500*time.Millisecond
			s.failStart(nil)
			select {
			case <-conn.done:
			case <-s.stop:
			}
		}
		off()
		s.mu.Lock()
		s.active = false
		for id, t := range s.dogs {
			t.Stop()
			delete(s.dogs, id)
		}
		s.conn = nil
		s.mu.Unlock()
		conn.close()
		if err != nil {
			if !attached && s.failStart(err) {
				return
			}
			failures++
			if spent(err) {
				return
			}
			slog.Warn(fmt.Sprintf("CDP supervisor %s: session dropped after %.1fs (attempt %d/%d): %s", s.task, time.Since(since).Seconds(), failures,
				reconnectBudget, redactEndpoint(err.Error())))
		}
		if s.halted() {
			return
		}
		slog.Debug(fmt.Sprintf("CDP supervisor %s: reconnecting in %.1fs...", s.task, backoff.Seconds()))
		s.sleep(backoff)
		backoff = min(backoff*2, 10*time.Second)
	}
}

func (s *supervisor) call(conn *cdpConn, method string, params any, session string, timeout time.Duration) (json.RawMessage, error) {
	if conn == nil {
		return nil, errors.New("supervisor WebSocket is not connected")
	}
	return conn.callWithin(method, params, session, timeout)
}

// A command whose failure is only logged
func (s *supervisor) quiet(conn *cdpConn, method string, params any, session string, timeout time.Duration, what string) {
	if _, err := s.call(conn, method, params, session, timeout); err != nil {
		slog.Debug(fmt.Sprintf("%s failed (%s): %s", method, what, err))
	}
}

// Attaches flattened to the first page, a new blank one when there is none, and enables its domains and the dialog bridge
func (s *supervisor) attachInitial(conn *cdpConn) error {
	raw, err := s.call(conn, "Target.getTargets", nil, "", 10*time.Second)
	if err != nil {
		return err
	}
	var listed struct {
		TargetInfos []targetInfo `json:"targetInfos"`
	}
	json.Unmarshal(raw, &listed)
	target := ""
	for _, t := range listed.TargetInfos {
		if t.Type == "page" {
			target = t.TargetID
			break
		}
	}
	if target == "" {
		raw, err := s.call(conn, "Target.createTarget", map[string]any{"url": "about:blank"}, "", 10*time.Second)
		if err != nil {
			return err
		}
		var created struct {
			TargetID string `json:"targetId"`
		}
		if json.Unmarshal(raw, &created) != nil || created.TargetID == "" {
			return errors.New("KeyError: 'targetId'")
		}
		target = created.TargetID
	}
	session, err := s.attach(conn, target, 10*time.Second)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.page = session
	s.mu.Unlock()
	return nil
}

// Session of a target attached flattened, its page domains enabled and the dialog bridge put in
func (s *supervisor) attach(conn *cdpConn, target string, timeout time.Duration) (string, error) {
	raw, err := s.call(conn, "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true}, "", timeout)
	if err != nil {
		return "", err
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(raw, &attached) != nil || attached.SessionID == "" {
		return "", errors.New("KeyError: 'sessionId'")
	}
	if err := s.enable(conn, attached.SessionID, timeout); err != nil {
		return "", err
	}
	s.bridge(conn, attached.SessionID)
	return attached.SessionID, nil
}

// Page and Runtime on, and nested targets attached, on one session
func (s *supervisor) enable(conn *cdpConn, session string, timeout time.Duration) error {
	for _, method := range []string{"Page.enable", "Runtime.enable"} {
		if _, err := s.call(conn, method, nil, session, timeout); err != nil {
			return err
		}
	}
	_, err := s.call(conn, "Target.setAutoAttach", autoAttach, session, timeout)
	return err
}

// Puts the dialog bridge and the interception of its requests on a session, and into the page already loaded
func (s *supervisor) bridge(conn *cdpConn, session string) {
	what := "dialog bridge sid=" + strs.Head(session, 16)
	s.quiet(conn, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": bridgeScript, "runImmediately": true}, session, 5*time.Second, what)
	s.quiet(conn, "Fetch.enable", map[string]any{"patterns": []any{map[string]any{"urlPattern": bridgePattern, "requestStage": "Request"}},
		"handleAuthRequests": false}, session, 5*time.Second, what)
	s.quiet(conn, "Runtime.evaluate", map[string]any{"expression": bridgeScript, "returnByValue": true}, session, 3*time.Second,
		"dialog bridge inject sid="+strs.Head(session, 16))
}

// Takes in one event: dialogs and frames kept, the commands it needs sent from their own goroutine so the reader never waits on itself
func (s *supervisor) dispatch(e event) {
	if s.halted() {
		return
	}
	var p map[string]any
	if len(e.Params) > 0 && json.Unmarshal(e.Params, &p) != nil {
		return
	}
	if p == nil {
		p = map[string]any{}
	}
	switch e.Method {
	case "Page.javascriptDialogOpening":
		s.admit(str(p, "type"), str(p, "message"), str(p, "defaultPrompt"), e.SessionID, p["frameId"], "")
	case "Page.javascriptDialogClosed":
		s.closed(e.SessionID)
	case "Fetch.requestPaused":
		s.paused(p, e.SessionID)
	case "Page.frameAttached":
		if id := str(p, "frameId"); id != "" {
			s.mu.Lock()
			s.frames.set(frameInfo{FrameID: id, Parent: str(p, "parentFrameId"), Session: e.SessionID})
			s.mu.Unlock()
		}
	case "Page.frameNavigated":
		s.navigated(obj(p, "frame"), e.SessionID)
	case "Page.frameDetached":
		id, reason := str(p, "frameId"), cmp.Or(str(p, "reason"), "remove")
		if id == "" || strings.ToLower(reason) == "swap" {
			return
		}
		s.mu.Lock()
		if old, ok := s.frames.get(id); !(ok && old.OOPIF && old.Session != "") {
			s.frames.pop(id)
		}
		s.mu.Unlock()
	case "Target.attachedToTarget":
		s.targetAttached(p)
	case "Target.detachedFromTarget":
		session := str(p, "sessionId")
		if session == "" {
			return
		}
		s.mu.Lock()
		for _, f := range s.frames.values() {
			if f.Session == session {
				f.Session = ""
				s.frames.set(f)
			}
		}
		s.mu.Unlock()
	}
}

// Text of a field of a decoded object, empty when missing or not text
func str(v map[string]any, key string) string {
	switch x := v[key].(type) {
	case string:
		return x
	case nil, bool:
		if x == true {
			return "True"
		}
		return ""
	case float64:
		if x == 0 {
			return ""
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return fmt.Sprint(v[key])
}

func obj(v map[string]any, key string) map[string]any {
	if o, ok := v[key].(map[string]any); ok {
		return o
	}
	return map[string]any{}
}

func (s *supervisor) navigated(frame map[string]any, session string) {
	id := str(frame, "id")
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.frames.get(id)
	if !ok {
		old = frameInfo{FrameID: id, Session: session}
	}
	s.frames.set(frameInfo{FrameID: id, URL: str(frame, "url"), Origin: cmp.Or(str(frame, "securityOrigin"), str(frame, "origin")),
		Parent: cmp.Or(str(frame, "parentId"), old.Parent), OOPIF: old.OOPIF, Session: old.Session, Name: cmp.Or(str(frame, "name"), old.Name)})
}

func (s *supervisor) targetAttached(p map[string]any) {
	info := obj(p, "targetInfo")
	session := str(p, "sessionId")
	kind := info["type"]
	if session == "" || kind != "iframe" && kind != "worker" {
		return
	}
	if kind == "iframe" {
		id := str(info, "targetId")
		s.mu.Lock()
		old, ok := s.frames.get(id)
		name, parent := str(info, "title"), ""
		if ok {
			parent = old.Parent
			name = cmp.Or(name, old.Name)
		}
		s.frames.set(frameInfo{FrameID: id, URL: str(info, "url"), OOPIF: true, Session: session, Parent: parent, Name: name})
		s.mu.Unlock()
	}
	conn := s.connection()
	go func() {
		if err := s.enable(conn, session, 3*time.Second); err != nil {
			slog.Debug(fmt.Sprintf("child session %s setup failed: %s", strs.Head(session, 16), err))
		}
		s.bridge(conn, session)
	}()
}

// A request of the bridge becomes a pending dialog, any other paused request goes on unchanged
func (s *supervisor) paused(p map[string]any, session string) {
	request := str(p, "requestId")
	if request == "" {
		return
	}
	raw := str(obj(p, "request"), "url")
	conn := s.connection()
	if !strings.Contains(raw, bridgeHost) {
		go s.quiet(conn, "Fetch.continueRequest", map[string]any{"requestId": request}, session, 3*time.Second, "passthrough")
		return
	}
	query := map[string]string{}
	if u, err := url.Parse(raw); err == nil {
		for _, part := range strings.FieldsFunc(u.RawQuery, func(r rune) bool { return r == '&' }) {
			k, v, _ := strings.Cut(part, "=")
			key, err1 := url.QueryUnescape(k)
			val, err2 := url.QueryUnescape(v)
			if err1 != nil || err2 != nil || val == "" {
				continue
			}
			if _, seen := query[key]; !seen {
				query[key] = val
			}
		}
	}
	s.admit(cmp.Or(query["kind"], "alert"), query["message"], query["default_prompt"], session, p["frameId"], request)
}

// A dialog under the policy: answered at once by an auto policy, else kept pending under a watchdog that dismisses it in time
func (s *supervisor) admit(kind, message, prompt, session string, frame any, bridge string) {
	s.mu.Lock()
	s.seq++
	d := pendingDialog{ID: fmt.Sprintf("d-%d", s.seq), Type: kind, Message: message, DefaultPrompt: prompt, OpenedAt: now(),
		Session: cmp.Or(session, s.page), FrameID: frame, Bridge: bridge}
	if s.policy == browserconfig.AutoDismiss || s.policy == browserconfig.AutoAccept {
		s.archive(d, "auto_policy")
		s.mu.Unlock()
		accept := s.policy == browserconfig.AutoAccept
		text := ""
		if accept {
			text = prompt
		}
		go s.respondQuiet(d, accept, &text)
		return
	}
	if _, ok := s.pending[d.ID]; !ok {
		s.keys = append(s.keys, d.ID)
	}
	s.pending[d.ID] = d
	id := d.ID
	s.dogs[id] = time.AfterFunc(s.timeout, func() { s.expired(id) })
	s.mu.Unlock()
}

// The bridge's request fulfilled, or the native dialog answered
func (s *supervisor) respond(d pendingDialog, accept bool, prompt *string) error {
	conn := s.connection()
	if d.Bridge != "" {
		text := ""
		if d.Type == "prompt" && prompt != nil {
			text = *prompt
		}
		body, err := json.Marshal(map[string]any{"accept": accept, "dialog_id": d.ID, "prompt_text": text})
		if err != nil {
			return err
		}
		s.quiet(conn, "Fetch.fulfillRequest", map[string]any{"requestId": d.Bridge, "responseCode": 200, "responseHeaders": []any{
			map[string]any{"name": "Content-Type", "value": "application/json"}, map[string]any{"name": "Access-Control-Allow-Origin", "value": "*"}},
			"body": base64.StdEncoding.EncodeToString(body)}, d.Session, 5*time.Second, "bridge fulfill "+d.ID)
		return nil
	}
	params := map[string]any{"accept": accept}
	if prompt != nil && d.Type == "prompt" {
		params["promptText"] = *prompt
	}
	_, err := s.call(conn, "Page.handleJavaScriptDialog", params, d.Session, 5*time.Second)
	return err
}

func (s *supervisor) respondQuiet(d pendingDialog, accept bool, prompt *string) {
	if err := s.respond(d, accept, prompt); err != nil {
		slog.Debug(fmt.Sprintf("auto response failed for %s: %s", d.ID, err))
	}
}

func (s *supervisor) expired(id string) {
	s.mu.Lock()
	d, ok := s.pending[id]
	s.mu.Unlock()
	if !ok {
		return
	}
	slog.Warn(fmt.Sprintf("CDP supervisor %s: dialog %s (%s) auto-dismissed after %gs timeout", s.task, id, d.Type, s.timeout.Seconds()))
	s.retire(id, "watchdog")
	s.respondQuiet(d, false, nil)
}

// Moves a pending dialog to the recent ring and stops its watchdog
func (s *supervisor) retire(id, by string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.pending[id]; ok {
		delete(s.pending, id)
		s.keys = slices.DeleteFunc(s.keys, func(k string) bool { return k == id })
		s.archive(d, by)
	}
	if t := s.dogs[id]; t != nil {
		t.Stop()
	}
	delete(s.dogs, id)
}

func (s *supervisor) archive(d pendingDialog, by string) {
	s.recent = append(s.recent, dialogRecord{ID: d.ID, Type: d.Type, Message: d.Message, OpenedAt: d.OpenedAt, ClosedAt: now(), ClosedBy: by,
		FrameID: d.FrameID})
	if len(s.recent) > recentDialogs*2 {
		s.recent = slices.Clone(s.recent[len(s.recent)-recentDialogs:])
	}
}

// A native dialog closed by the page or a person, retired as the first pending one of its session
func (s *supervisor) closed(session string) {
	s.mu.Lock()
	candidate := ""
	for _, k := range s.keys {
		if d := s.pending[k]; d.Session == session && d.Bridge == "" {
			candidate = d.ID
			break
		}
	}
	s.mu.Unlock()
	if candidate != "" {
		s.retire(candidate, "remote")
	}
}

// Copy of the dialogs and the frame tree
func (s *supervisor) snapshot() watched {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := make([]pendingDialog, 0, len(s.keys))
	for _, k := range s.keys {
		pending = append(pending, s.pending[k])
	}
	recent := s.recent
	if len(recent) > recentDialogs {
		recent = recent[len(recent)-recentDialogs:]
	}
	return watched{Pending: pending, Recent: slices.Clone(recent), Tree: s.tree(), Active: s.active}
}

// Frame tree from the top frame, breadth first and capped, under the lock
func (s *supervisor) tree() map[string]any {
	all := s.frames.values()
	var tops []frameInfo
	for _, f := range all {
		if f.Parent == "" {
			tops = append(tops, f)
		}
	}
	if len(tops) == 0 {
		return map[string]any{"top": nil, "children": []any{}, "truncated": false}
	}
	top := tops[0]
	for _, f := range tops {
		if !f.OOPIF {
			top = f
			break
		}
	}
	type queued struct {
		frame frameInfo
		depth int
	}
	var queue []queued
	for _, f := range all {
		if f.Parent == top.FrameID {
			queue = append(queue, queued{f, 1})
		}
	}
	children := []any{}
	truncated := false
	visited := map[string]bool{top.FrameID: true}
	for len(queue) > 0 && len(children) < frameEntries {
		q := queue[0]
		queue = queue[1:]
		if visited[q.frame.FrameID] {
			continue
		}
		visited[q.frame.FrameID] = true
		if q.frame.OOPIF && q.depth > oopifDepth {
			truncated = true
			continue
		}
		children = append(children, q.frame.view())
		for _, f := range all {
			if f.Parent == q.frame.FrameID && !visited[f.FrameID] {
				queue = append(queue, queued{f, q.depth + 1})
			}
		}
	}
	return map[string]any{"top": top.view(), "children": children, "truncated": truncated || len(queue) > 0}
}

// Accepts or dismisses a pending dialog, a result with ok false for what the agent can fix
func (s *supervisor) answer(action string, prompt *string, id string) map[string]any {
	if action != "accept" && action != "dismiss" {
		return failed("action must be 'accept' or 'dismiss', got " + strconv.Quote(action))
	}
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return failed("supervisor is not active")
	}
	if len(s.keys) == 0 {
		s.mu.Unlock()
		return failed("no dialog is currently open")
	}
	var d pendingDialog
	switch {
	case id != "":
		found, ok := s.pending[id]
		if !ok {
			known := slices.Sorted(slices.Values(s.keys))
			s.mu.Unlock()
			return failed(fmt.Sprintf("dialog_id %q not found (known: %s)", id, pyList(known)))
		}
		d = found
	case len(s.keys) > 1:
		ids := slices.Clone(s.keys)
		s.mu.Unlock()
		return failed(fmt.Sprintf("%d pending dialogs; specify dialog_id. Candidates: %s", len(ids), pyList(ids)))
	default:
		d = s.pending[s.keys[0]]
	}
	s.mu.Unlock()
	text := ""
	if prompt != nil {
		text = *prompt
	}
	err := s.respond(d, action == "accept", &text)
	s.retire(d.ID, "agent")
	if err != nil {
		return failed(err.Error())
	}
	return map[string]any{"ok": true, "dialog": d.view()}
}

// List of texts as the tools have always shown it
func pyList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = "'" + strings.ReplaceAll(strings.ReplaceAll(item, `\`, `\\`), "'", `\'`) + "'"
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// Evaluates in the attached page over the live connection, again by reference when the value is too deep to send
func (s *supervisor) evaluate(expression string, byValue, await bool, timeout time.Duration) map[string]any {
	s.mu.Lock()
	active, session, conn := s.active, s.page, s.conn
	s.mu.Unlock()
	if !active {
		return failed("supervisor is not active")
	}
	if session == "" {
		return failed("supervisor has no attached page session")
	}
	run := func(value bool) (json.RawMessage, error) {
		return s.call(conn, "Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": value, "awaitPromise": await,
			"userGesture": true}, session, timeout)
	}
	raw, err := run(byValue)
	if err != nil {
		if !byValue || !strings.Contains(strings.ToLower(err.Error()), "reference chain is too long") {
			return failed(err.Error())
		}
		if raw, err = run(false); err != nil {
			return failed(err.Error())
		}
	}
	var payload map[string]any
	json.Unmarshal(raw, &payload)
	if details, ok := payload["exceptionDetails"].(map[string]any); ok && len(details) > 0 {
		text := cmp.Or(str(details, "text"), "JavaScript exception")
		if description := str(obj(details, "exception"), "description"); description != "" {
			return failed(text + ": " + description)
		}
		return failed(text)
	}
	result := obj(payload, "result")
	kind, ok := result["type"]
	if !ok {
		kind = "undefined"
	}
	var value any
	if v, has := result["value"]; has {
		value = v
	} else if kind != "undefined" {
		value = cmp.Or(str(result, "description"), str(result, "unserializableValue"))
		if value == "" {
			value = nil
		}
	}
	return map[string]any{"ok": true, "result": value, "result_type": kind}
}

// Moves the page session to an open page of the origin, the first where accept holds when given
func (s *supervisor) focus(origin, accept string, timeout time.Duration) map[string]any {
	conn := s.connection()
	if conn == nil {
		return failed("supervisor loop is not running")
	}
	raw, err := s.call(conn, "Target.getTargets", nil, "", timeout)
	if err != nil {
		return failed(err.Error())
	}
	var listed struct {
		TargetInfos []targetInfo `json:"targetInfos"`
	}
	json.Unmarshal(raw, &listed)
	type candidate struct{ target, url string }
	var candidates []candidate
	for _, t := range listed.TargetInfos {
		if t.Type != "page" || !strings.HasPrefix(t.URL, "http://") && !strings.HasPrefix(t.URL, "https://") || t.TargetID == "" {
			continue
		}
		if origin != "" {
			if norm, err := vault.NormalizeOrigin(t.URL); err != nil || norm != origin {
				continue
			}
		}
		candidates = append(candidates, candidate{t.TargetID, t.URL})
	}
	for _, c := range candidates {
		session, err := s.attach(conn, c.target, timeout)
		if err != nil {
			return failed(err.Error())
		}
		if accept != "" {
			probe, err := s.call(conn, "Runtime.evaluate", map[string]any{"expression": accept, "returnByValue": true}, session, timeout)
			if err != nil {
				return failed(err.Error())
			}
			var got struct {
				Result struct {
					Value any `json:"value"`
				} `json:"result"`
			}
			json.Unmarshal(probe, &got)
			if !config.Truthy(got.Result.Value) {
				if _, err := s.call(conn, "Target.detachFromTarget", map[string]any{"sessionId": session}, "", timeout); err != nil {
					return failed(err.Error())
				}
				continue
			}
		}
		s.mu.Lock()
		s.page = session
		s.mu.Unlock()
		return map[string]any{"ok": true, "url": c.url}
	}
	where := cmp.Or(origin, "any site")
	suffix := ""
	if accept != "" && len(candidates) > 0 {
		suffix = " with the expected form"
	}
	return failed("no open page on " + where + suffix)
}

// The supervisor of every task, by task, with the profile that started each
type supervisors struct {
	mu      sync.Mutex
	byTask  map[string]*supervisor
	profile map[string]string
}

func newSupervisors() supervisors {
	return supervisors{byTask: map[string]*supervisor{}, profile: map[string]string{}}
}

func (r *supervisors) get(task string) *supervisor {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byTask[task]
}

func (r *supervisors) pop(task string) *supervisor {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.byTask[task]
	delete(r.byTask, task)
	delete(r.profile, task)
	return s
}

// The task's healthy supervisor on the endpoint, a stale or retargeted one replaced
func (r *supervisors) ensure(cfg *config.Config, task, cdp string) (*supervisor, error) {
	r.mu.Lock()
	existing := r.byTask[task]
	if existing != nil {
		if existing.cdp == cdp && existing.healthy() {
			r.mu.Unlock()
			return existing, nil
		}
		delete(r.byTask, task)
		delete(r.profile, task)
	}
	r.mu.Unlock()
	if existing != nil {
		existing.end(5 * time.Second)
	}
	policy, timeout := dialogPolicy(cfg)
	s, err := newSupervisor(task, cdp, policy, seconds(timeout))
	if err != nil {
		return nil, err
	}
	gone := func(s *supervisor) {
		r.mu.Lock()
		if r.byTask[task] == s {
			delete(r.byTask, task)
			delete(r.profile, task)
		}
		r.mu.Unlock()
	}
	if err := s.start(supervisorStart, gone); err != nil {
		return nil, err
	}
	r.mu.Lock()
	if already := r.byTask[task]; already != nil && already.cdp == cdp {
		r.mu.Unlock()
		s.end(5 * time.Second)
		return already, nil
	}
	r.byTask[task] = s
	r.profile[task] = cfg.Profile()
	r.mu.Unlock()
	return s, nil
}

func (r *supervisors) stop(task string) {
	if s := r.pop(task); s != nil {
		s.end(5 * time.Second)
	}
}

// Stops the supervisors one profile started
func (r *supervisors) stopProfile(profile string) {
	r.mu.Lock()
	var tasks []string
	for task, p := range r.profile {
		if p == profile {
			tasks = append(tasks, task)
		}
	}
	r.mu.Unlock()
	for _, task := range tasks {
		r.stop(task)
	}
}

func (r *supervisors) stopAll() {
	r.mu.Lock()
	all := make([]*supervisor, 0, len(r.byTask))
	for _, s := range r.byTask {
		all = append(all, s)
	}
	r.byTask, r.profile = map[string]*supervisor{}, map[string]string{}
	r.mu.Unlock()
	for _, s := range all {
		s.end(5 * time.Second)
	}
}

// Starts the supervisor of the key's session on the attached endpoint, else on the session's own; a failure only logged
func (r *registrant) ensureSupervisor(ctx context.Context, cfg *config.Config, key string) {
	endpoint := r.override(ctx, cfg)
	if endpoint == "" {
		r.sessions.mu.Lock()
		s := r.sessions.get(key)
		r.sessions.mu.Unlock()
		if s != nil && s.cdp != "" {
			endpoint = resolveCDP(ctx, s.cdp)
		}
	}
	if endpoint == "" {
		return
	}
	if _, err := r.supervisors.ensure(cfg, key, endpoint); err != nil {
		slog.Debug(fmt.Sprintf("CDP supervisor attach for task=%s failed (non-fatal): %s", key, redactEndpoint(err.Error())))
	}
}
