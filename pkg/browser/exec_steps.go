package browser

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/HugoSmits86/nativewebp"
	"golang.org/x/image/bmp"
	"golang.org/x/image/tiff"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pypil"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// A png that has not arrived by then will not, a hidden tab answers jpeg within a few seconds
var (
	pngDeadline  = 10 * time.Second
	jpegDeadline = 60 * time.Second
)

// Deadline of one browser round trip a helper makes unless it names its own
var execResponseTimeout = 5 * time.Second

const (
	execMaxMessage    = 256 << 20
	execPoll          = 300 * time.Millisecond
	execTabMarker     = "\U0001F9CA"
	execSetupTimeout  = 120 * time.Second
	execEventBuffer   = 500
	execMarkerTimeout = 2 * time.Second
	execNoDeadline    = 1 << 62
)

// URL prefixes of pages that belong to the browser itself
var internalURLPrefixes = []string{"chrome://", "chrome-untrusted://", "devtools://", "chrome-extension://", "about:"}

func isInternalURL(u string) bool {
	for _, prefix := range internalURLPrefixes {
		if strings.HasPrefix(u, prefix) {
			return true
		}
	}
	return false
}

// A blank startup page a launcher shows while the agent starts, never a work tab
func isAgentStartupPage(title, u string) bool {
	return strings.HasPrefix(title, "Starting agent ") && (u == "" || u == "about:blank" || strings.HasPrefix(u, "about:blank#"))
}

// Tab marker on unless BH_TAB_MARKER turns it off
func tabMarkerEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BH_TAB_MARKER"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// execEvent is one protocol event kept for drain_events, its session empty for a browser level event
type execEvent struct {
	Method    string
	Params    json.RawMessage
	SessionID string
}

// execSession is the browser one browser_exec session drives, its connection, attached tab and events kept between calls
type execSession struct {
	mu         sync.Mutex
	conn       *CDPConn
	endpoint   string
	target     string
	session    string
	pinned     bool
	selectAll  int
	stopEvents func()
	eventMu    sync.Mutex
	dialog     map[string]any
	events     []execEvent
}

var execSessions = struct {
	sync.Mutex
	byKey map[string]*execSession
	homes map[string]string
}{byKey: map[string]*execSession{}, homes: map[string]string{}}

// The session of a key, created on first use and remembered under its home
func execSessionFor(key, homeKey string) *execSession {
	execSessions.Lock()
	defer execSessions.Unlock()
	s, ok := execSessions.byKey[key]
	if !ok {
		s = &execSession{}
		execSessions.byKey[key] = s
		execSessions.homes[key] = homeKey
	}
	return s
}

// Closes the connections of every browser_exec session, only those of one home when a key is given
func closeExecSessions(homeKey string) {
	execSessions.Lock()
	var closing []*execSession
	for key, s := range execSessions.byKey {
		if homeKey == "" || execSessions.homes[key] == homeKey {
			closing = append(closing, s)
			delete(execSessions.byKey, key)
			delete(execSessions.homes, key)
		}
	}
	execSessions.Unlock()
	for _, s := range closing {
		s.mu.Lock()
		s.disconnect()
		s.mu.Unlock()
	}
}

func (s *execSession) disconnect() {
	if s.stopEvents != nil {
		s.stopEvents()
	}
	if s.conn != nil {
		_ = s.conn.Close()
	}
	s.eventMu.Lock()
	s.conn, s.stopEvents, s.target, s.session, s.pinned, s.selectAll = nil, nil, "", "", false, 0
	s.eventMu.Unlock()
}

// Buffers every event for drain_events, tracks the open dialog and re-marks the attached tab after a load
func (s *execSession) onEvent(ev CDPEvent) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if len(s.events) >= execEventBuffer {
		s.events = s.events[1:]
	}
	s.events = append(s.events, execEvent{Method: ev.Method, Params: ev.Params, SessionID: ev.SessionID})
	switch ev.Method {
	case "Page.javascriptDialogOpening":
		var dialog map[string]any
		if json.Unmarshal(ev.Params, &dialog) == nil {
			s.dialog = dialog
		}
	case "Page.javascriptDialogClosed":
		s.dialog = nil
	case "Page.loadEventFired", "Page.domContentEventFired":
		if conn, sid := s.conn, s.session; conn != nil && sid != "" && tabMarkerEnabled() {
			go func() {
				_, _ = conn.CallTimeout("Runtime.evaluate", map[string]any{"expression": markTabJS}, sid, execMarkerTimeout)
			}()
		}
	}
}

// Connects to the endpoint when the session holds no live connection to it
func (s *execSession) connect(ctx context.Context, endpoint string) error {
	if s.conn != nil && s.conn.Alive() && s.endpoint == endpoint {
		return nil
	}
	s.disconnect()
	conn, err := DialCDP(ctx, endpoint, execMaxMessage)
	if err != nil {
		return fmt.Errorf("the browser at %s could not be reached: %w", endpoint, err)
	}
	s.conn, s.endpoint = conn, endpoint
	s.eventMu.Lock()
	s.events, s.dialog = nil, nil
	s.eventMu.Unlock()
	s.stopEvents = conn.OnEvent(s.onEvent)
	return nil
}

var (
	markTabJS   = "if(!document.title.startsWith('" + execTabMarker + "'))document.title='" + execTabMarker + " '+document.title"
	unmarkTabJS = "if(document.title.startsWith('" + execTabMarker + " '))document.title=document.title.slice(3)"
)

// helperRun answers the helper calls of one browser_exec call on a session
type helperRun struct {
	ctx       context.Context
	s         *execSession
	workspace string
	shotDir   string
	activate  bool
	ownTab    bool
}

// helperTimeout is a round trip that missed its deadline, raised in the code as TimeoutError
type helperTimeout string

func (e helperTimeout) Error() string { return string(e) }

// Seconds the way Python's g format prints them
func secondsG(d time.Duration) string { return strconv.FormatFloat(d.Seconds(), 'g', 6, 64) }

// One round trip under a deadline, a missed deadline reported as the helper timeout
func (r *helperRun) send(session, method string, params any, timeout time.Duration) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(r.ctx, timeout)
	defer cancel()
	raw, err := r.s.conn.Call(ctx, method, params, session)
	if err != nil && errors.Is(err, context.DeadlineExceeded) && r.ctx.Err() == nil {
		return nil, helperTimeout(fmt.Sprintf("%s timed out after %ss waiting for the browser", method, secondsG(timeout)))
	}
	return raw, err
}

// Round trip on the attached tab, browser level for Target methods, decoded into out when given
func (r *helperRun) call(method string, params any, out any) error {
	session := r.s.session
	if strings.HasPrefix(method, "Target.") {
		session = ""
	}
	raw, err := r.send(session, method, params, execResponseTimeout)
	if err != nil || out == nil || len(raw) == 0 {
		return err
	}
	return json.Unmarshal(raw, out)
}

func (r *helperRun) sleep(d time.Duration) error {
	select {
	case <-r.ctx.Done():
		return r.ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// Page targets of the browser, the launcher startup page left out and internal pages only when asked
func (r *helperRun) listTabs(includeChrome bool) ([]map[string]any, error) {
	var out struct {
		TargetInfos []TargetInfo `json:"targetInfos"`
	}
	if err := r.call("Target.getTargets", nil, &out); err != nil {
		return nil, err
	}
	tabs := []map[string]any{}
	for _, t := range out.TargetInfos {
		if t.Type != "page" || isAgentStartupPage(t.Title, t.URL) || (!includeChrome && isInternalURL(t.URL)) {
			continue
		}
		tabs = append(tabs, map[string]any{"targetId": t.TargetID, "target_id": t.TargetID, "title": t.Title, "url": t.URL})
	}
	return tabs, nil
}

// The attached tab, not_attached before any attach and cdp_disconnected when the browser cannot describe it
func (r *helperRun) currentTab() (map[string]any, error) {
	if r.s.target == "" {
		return nil, errors.New("not_attached")
	}
	var out struct {
		TargetInfo TargetInfo `json:"targetInfo"`
	}
	if err := r.call("Target.getTargetInfo", map[string]any{"targetId": r.s.target}, &out); err != nil {
		return nil, errors.New("cdp_disconnected")
	}
	return map[string]any{"targetId": out.TargetInfo.TargetID, "target_id": out.TargetInfo.TargetID, "url": out.TargetInfo.URL, "title": out.TargetInfo.Title}, nil
}

// Attaches the session to a tab, moving the marker, and returns the new session id
func (r *helperRun) switchTab(target string, activate bool) (string, error) {
	if tabMarkerEnabled() && r.s.session != "" {
		_ = r.call("Runtime.evaluate", map[string]any{"expression": unmarkTabJS}, nil)
	}
	if activate {
		if err := r.call("Target.activateTarget", map[string]any{"targetId": target}, nil); err != nil {
			return "", err
		}
	}
	var out struct {
		SessionID *string `json:"sessionId"`
	}
	if err := r.call("Target.attachToTarget", map[string]any{"targetId": target, "flatten": true}, &out); err != nil {
		return "", err
	}
	if out.SessionID == nil {
		return "", errors.New("Target.attachToTarget did not return a sessionId")
	}
	r.s.eventMu.Lock()
	r.s.target, r.s.session, r.s.dialog = target, *out.SessionID, nil
	r.s.eventMu.Unlock()
	for _, domain := range []string{"Page.enable", "Network.enable"} {
		if err := r.call(domain, nil, nil); err != nil {
			slog.Default().Debug(fmt.Sprintf("browser_exec: %s failed on %s: %s", domain, target, err))
		}
	}
	if tabMarkerEnabled() {
		_ = r.call("Runtime.evaluate", map[string]any{"expression": markTabJS}, nil)
	}
	return r.s.session, nil
}

// Makes sure the session is attached to a live tab, a named session on a shared browser to a tab of its own
func (r *helperRun) ensureTab() error {
	tabs, err := r.listTabs(true)
	if err != nil {
		return err
	}
	if r.s.session != "" {
		for _, t := range tabs {
			if t["targetId"] == r.s.target {
				return nil
			}
		}
	}
	if r.ownTab && !r.s.pinned {
		r.s.pinned = true
		return r.openTab(false)
	}
	for _, t := range tabs {
		if !isInternalURL(t["url"].(string)) {
			_, err := r.switchTab(t["targetId"].(string), false)
			return err
		}
	}
	if len(tabs) > 0 {
		_, err := r.switchTab(tabs[0]["targetId"].(string), false)
		return err
	}
	return r.openTab(true)
}

func (r *helperRun) openTab(background bool) error {
	params := map[string]any{"url": "about:blank"}
	if background {
		params["background"] = true
	}
	var out struct {
		TargetID string `json:"targetId"`
	}
	if err := r.call("Target.createTarget", params, &out); err != nil {
		return err
	}
	_, err := r.switchTab(out.TargetID, false)
	return err
}

// jsUnserializable is a value JSON cannot carry, NaN, infinities, negative zero or a BigInt
type jsUnserializable string

// Expression shortened to one line of at most 160 characters for an error message
func jsSnippet(expr string) string {
	snippet := strings.ReplaceAll(pystr.Strip(expr), "\n", `\n`)
	if runes := []rune(snippet); len(runes) > 160 {
		return string(runes[:157]) + "..."
	}
	return snippet
}

// Value of a Runtime.evaluate, its exception raised with the line, column and expression
func (r *helperRun) evaluate(session, expr string, awaitPromise bool) (any, error) {
	params := map[string]any{"expression": expr, "returnByValue": true, "awaitPromise": awaitPromise}
	raw, err := r.send(session, "Runtime.evaluate", params, execResponseTimeout)
	var timeout helperTimeout
	if errors.As(err, &timeout) {
		return nil, fmt.Errorf("Runtime.evaluate timed out; expression: %s", jsSnippet(expr))
	}
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result  map[string]json.RawMessage `json:"result"`
		Details json.RawMessage            `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	details := pyjson.NewObject()
	if len(resp.Details) > 0 {
		if decoded, err := pyjson.LoadsBytes(resp.Details); err == nil {
			if obj, ok := decoded.(*pyjson.Object); ok {
				details = obj
			}
		}
	}
	var subtype string
	_ = json.Unmarshal(resp.Result["subtype"], &subtype)
	if details.Len() > 0 || subtype == "error" {
		where := ""
		if line, col := details.GetOr("lineNumber", nil), details.GetOr("columnNumber", nil); line != nil && col != nil {
			where = fmt.Sprintf(" at line %s, column %s", pyrepr.Str(line), pyrepr.Str(col))
		}
		return nil, fmt.Errorf("JavaScript evaluation failed%s: %s; expression: %s", where, jsExceptionDescription(resp.Result, details), jsSnippet(expr))
	}
	if value, ok := resp.Result["value"]; ok {
		return value, nil
	}
	if value, ok := resp.Result["unserializableValue"]; ok {
		var text string
		_ = json.Unmarshal(value, &text)
		return jsUnserializable(text), nil
	}
	return nil, nil
}

// What a thrown evaluation says, the exception description, value or class, then the details text
func jsExceptionDescription(result map[string]json.RawMessage, details *pyjson.Object) string {
	desc := ""
	if raw, ok := result["description"]; ok {
		if v, err := pyjson.LoadsBytes(raw); err == nil && pyrepr.Truthy(v) {
			desc = pyrepr.Str(v)
		}
	}
	exc, isDict := details.GetOr("exception", nil).(*pyjson.Object)
	if desc == "" && isDict {
		var d any
		found := false
		if v, ok := exc.Get("description"); ok && v != nil {
			d, found = v, true
		} else if v, ok := exc.Get("value"); ok {
			d, found = pyrepr.Str(v), true
		} else if v, ok := exc.Get("className"); ok && v != nil {
			d, found = v, true
		}
		if found && pyrepr.Truthy(d) {
			desc = pyrepr.Str(d)
		}
	}
	if desc == "" && details.Len() > 0 {
		if v := details.GetOr("text", nil); pyrepr.Truthy(v) {
			desc = pyrepr.Str(v)
		}
	}
	if desc == "" {
		return "JavaScript evaluation failed"
	}
	return desc
}

// Evaluates in the page awaiting promises, a top level return retried inside a function
func (r *helperRun) js(session, expr string) (any, error) {
	value, err := r.evaluate(session, expr, true)
	if err != nil && strings.Contains(err.Error(), "Illegal return statement") {
		return r.evaluate(session, "(function(){"+expr+"})()", true)
	}
	return value, err
}

// Python truth of an evaluated value
func jsTruthy(value any) bool {
	raw, ok := value.(json.RawMessage)
	if !ok {
		return pyrepr.Truthy(value)
	}
	var decoded any
	if json.Unmarshal(raw, &decoded) != nil {
		return false
	}
	return pyrepr.Truthy(decoded)
}

func quoteJS(s string) string {
	data, _ := json.Marshal(s)
	return string(data)
}

// Polls an expression until its value passes the test or the timeout passes
func (r *helperRun) poll(expr string, timeout float64, pass func(any) bool) (bool, error) {
	deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
	for time.Now().Before(deadline) {
		value, err := r.js(r.s.session, expr)
		if err != nil {
			return false, err
		}
		if pass(value) {
			return true, nil
		}
		if err := r.sleep(execPoll); err != nil {
			return false, err
		}
	}
	return false, nil
}

func (r *helperRun) waitForElement(selector string, timeout float64, visible bool) (bool, error) {
	check := "!!document.querySelector(" + quoteJS(selector) + ")"
	if visible {
		check = "(()=>{const e=document.querySelector(" + quoteJS(selector) + ");if(!e)return false;" +
			"if(typeof e.checkVisibility==='function')return e.checkVisibility({checkOpacity:true,checkVisibilityCSS:true});" +
			"const s=getComputedStyle(e);return s.display!=='none'&&s.visibility!=='hidden'&&s.opacity!=='0'})()"
	}
	return r.poll(check, timeout, jsTruthy)
}

// Named keys with their virtual key code, physical code and text
var namedKeys = map[string]struct {
	vk   int
	code string
	text string
}{
	"Enter": {13, "Enter", "\r"}, "Tab": {9, "Tab", "\t"}, "Backspace": {8, "Backspace", ""}, "Escape": {27, "Escape", ""},
	"Delete": {46, "Delete", ""}, " ": {32, "Space", " "}, "ArrowLeft": {37, "ArrowLeft", ""}, "ArrowUp": {38, "ArrowUp", ""},
	"ArrowRight": {39, "ArrowRight", ""}, "ArrowDown": {40, "ArrowDown", ""}, "Home": {36, "Home", ""}, "End": {35, "End", ""},
	"PageUp": {33, "PageUp", ""}, "PageDown": {34, "PageDown", ""},
}

// Key codes dispatch_key gives a DOM KeyboardEvent, any other single character its code point
var domKeyCodes = map[string]int{"Enter": 13, "Tab": 9, "Escape": 27, "Backspace": 8, " ": 32, "ArrowLeft": 37, "ArrowUp": 38, "ArrowRight": 39, "ArrowDown": 40}

// Physical key and virtual key code of the punctuation a US keyboard types without Shift
var punctuationKeys = map[rune]struct {
	code string
	vk   int
}{
	'`': {"Backquote", 192}, '-': {"Minus", 189}, '=': {"Equal", 187}, '[': {"BracketLeft", 219}, ']': {"BracketRight", 221}, '\\': {"Backslash", 220},
	';': {"Semicolon", 186}, '\'': {"Quote", 222}, ',': {"Comma", 188}, '.': {"Period", 190}, '/': {"Slash", 191},
}

// Characters a US keyboard types with Shift and the character sharing their key
var shiftedChars = map[rune]rune{
	'~': '`', '!': '1', '@': '2', '#': '3', '$': '4', '%': '5', '^': '6', '&': '7', '*': '8', '(': '9', ')': '0', '_': '-', '+': '=',
	'{': '[', '}': ']', '|': '\\', ':': ';', '"': '\'', '<': ',', '>': '.', '?': '/',
}

// Physical code, virtual key and Shift need of one printable character on a US keyboard, false for one it has no key for
func printableKey(char rune) (string, int, bool, bool) {
	unshifted, shifted := char, false
	if base, ok := shiftedChars[char]; ok {
		unshifted, shifted = base, true
	}
	shifted = shifted || unicode.IsUpper(char)
	switch lower := unicode.ToLower(unshifted); {
	case unshifted < 128 && lower >= 'a' && lower <= 'z':
		upper := unicode.ToUpper(unshifted)
		return "Key" + string(upper), int(upper), shifted, true
	case unshifted >= '0' && unshifted <= '9':
		return "Digit" + string(unshifted), int(unshifted), shifted, true
	}
	if key, ok := punctuationKeys[unshifted]; ok {
		return key.code, key.vk, shifted, true
	}
	return "", 0, false, false
}

// Sends the key down, character and key up events a keyboard produces for one key
func (r *helperRun) pressKey(key string, modifiers int) error {
	vk, code, text := 0, key, ""
	runes := []rune(key)
	if named, ok := namedKeys[key]; ok {
		vk, code, text = named.vk, named.code, named.text
	} else if len(runes) == 1 {
		text, code = key, ""
		if c, v, shift, ok := printableKey(runes[0]); ok {
			code, vk = c, v
			if shift && modifiers&7 == 0 {
				modifiers |= 8
			}
		}
	}
	base := func(kind string) map[string]any {
		return map[string]any{"type": kind, "key": key, "code": code, "modifiers": modifiers, "windowsVirtualKeyCode": vk, "nativeVirtualKeyCode": vk}
	}
	printable := len(runes) == 1 && text != "" && modifiers&7 == 0
	down := base("keyDown")
	if !printable && text != "" {
		down["text"] = text
	}
	if err := r.call("Input.dispatchKeyEvent", down, nil); err != nil {
		return err
	}
	if printable {
		char := base("char")
		char["text"] = text
		if err := r.call("Input.dispatchKeyEvent", char, nil); err != nil {
			return err
		}
	}
	return r.call("Input.dispatchKeyEvent", base("keyUp"), nil)
}

// Focuses an input, clears it, types the text key by key and fires the events frameworks listen for
func (r *helperRun) fillInput(selector, text string, clear bool, timeout float64) error {
	notFound := fmt.Errorf("fill_input: element not found: %s", pyrepr.StrRepr(selector))
	if timeout > 0 {
		found, err := r.waitForElement(selector, timeout, false)
		if err != nil {
			return err
		}
		if !found {
			return notFound
		}
	}
	focused, err := r.js(r.s.session, "(()=>{const e=document.querySelector("+quoteJS(selector)+");if(!e)return false;e.focus();return true;})()")
	if err != nil {
		return err
	}
	if !jsTruthy(focused) {
		return notFound
	}
	if clear {
		if r.s.selectAll == 0 {
			var version struct {
				UserAgent string `json:"userAgent"`
			}
			if err := r.call("Browser.getVersion", nil, &version); err != nil {
				return err
			}
			r.s.selectAll = 2
			if strings.Contains(version.UserAgent, "Mac OS X") || strings.Contains(version.UserAgent, "Macintosh") {
				r.s.selectAll = 4
			}
		}
		selectAll := map[string]any{"type": "rawKeyDown", "key": "a", "code": "KeyA", "modifiers": r.s.selectAll,
			"windowsVirtualKeyCode": 65, "nativeVirtualKeyCode": 65, "commands": []string{"SelectAll"}}
		if err := r.call("Input.dispatchKeyEvent", selectAll, nil); err != nil {
			return err
		}
		delete(selectAll, "commands")
		selectAll["type"] = "keyUp"
		if err := r.call("Input.dispatchKeyEvent", selectAll, nil); err != nil {
			return err
		}
		if err := r.pressKey("Backspace", 0); err != nil {
			return err
		}
	}
	for _, ch := range text {
		if err := r.pressKey(string(ch), 0); err != nil {
			return err
		}
	}
	_, err = r.js(r.s.session, "(()=>{const e=document.querySelector("+quoteJS(selector)+");if(!e)return;"+
		"e.dispatchEvent(new Event('input',{bubbles:true}));e.dispatchEvent(new Event('change',{bubbles:true}));})();")
	return err
}

// Takes the buffered events, oldest first
func (r *helperRun) drainEvents() []execEvent {
	r.s.eventMu.Lock()
	defer r.s.eventMu.Unlock()
	out := r.s.events
	r.s.events = nil
	return out
}

// Waits until no request of the attached tab is in flight and no network event arrived for the idle window
func (r *helperRun) waitNetworkIdle(timeout, idleMS float64) (bool, error) {
	deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
	last := time.Now()
	inflight := map[string]bool{}
	active := r.s.session
	for time.Now().Before(deadline) {
		for _, ev := range r.drainEvents() {
			if ev.SessionID != active {
				continue
			}
			var p struct {
				RequestID string `json:"requestId"`
			}
			_ = json.Unmarshal(ev.Params, &p)
			switch {
			case ev.Method == "Network.requestWillBeSent":
				inflight[p.RequestID] = true
				last = time.Now()
			case ev.Method == "Network.loadingFinished" || ev.Method == "Network.loadingFailed":
				delete(inflight, p.RequestID)
				last = time.Now()
			case strings.HasPrefix(ev.Method, "Network."):
				last = time.Now()
			}
		}
		if len(inflight) == 0 && float64(time.Since(last).Milliseconds()) >= idleMS {
			return true, nil
		}
		if err := r.sleep(100 * time.Millisecond); err != nil {
			return false, err
		}
	}
	return false, nil
}

// Navigates the attached tab, naming the workspace domain skills of the host when they are on
func (r *helperRun) gotoURL(target string, domainSkills bool) (any, error) {
	var out map[string]any
	if err := r.call("Page.navigate", map[string]any{"url": target}, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	if !domainSkills || r.workspace == "" {
		return out, nil
	}
	host := ""
	if u, err := url.Parse(target); err == nil {
		host = strings.ToLower(u.Hostname())
	}
	host = strings.TrimPrefix(host, "www.")
	host, _, _ = strings.Cut(host, ".")
	dir := filepath.Join(r.workspace, "domain-skills", host)
	if !isDirPath(dir) {
		return out, nil
	}
	names := []string{}
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && path != dir && strings.HasSuffix(d.Name(), ".md") {
			names = append(names, d.Name())
		}
		return nil
	})
	sort.Strings(names)
	if len(names) > 10 {
		names = names[:10]
	}
	out["domain_skills"] = names
	return out, nil
}

// Opens or reuses a blank tab, navigates it and brings it forward on a browser nobody watches
func (r *helperRun) newTab(target string) (any, error) {
	tid, err := r.openOrReuse(target)
	if err != nil {
		return nil, err
	}
	if tid != "" && r.activate {
		if err := r.call("Target.activateTarget", map[string]any{"targetId": tid}, nil); err != nil {
			slog.Default().Warn(fmt.Sprintf("new_tab: Target.activateTarget failed, tab left in background: %s", err))
		}
	}
	return tid, nil
}

func (r *helperRun) openOrReuse(target string) (string, error) {
	if target != "about:blank" {
		if cur, err := r.currentTab(); err == nil {
			u := cur["url"].(string)
			if u == "" || u == "about:blank" || u == "data:text/html," || strings.HasPrefix(u, "about:blank#") ||
				strings.HasPrefix(u, "chrome://newtab") || strings.HasPrefix(u, "chrome://new-tab-page") ||
				strings.HasPrefix(u, "edge://newtab") || strings.HasPrefix(u, "about:newtab") {
				if _, err := r.gotoURL(target, false); err == nil {
					return cur["targetId"].(string), nil
				}
			}
		}
	}
	var out struct {
		TargetID string `json:"targetId"`
	}
	if err := r.call("Target.createTarget", map[string]any{"url": "about:blank", "background": true}, &out); err != nil {
		return "", err
	}
	if _, err := r.switchTab(out.TargetID, false); err != nil {
		return "", err
	}
	if target != "about:blank" {
		if _, err := r.gotoURL(target, false); err != nil {
			return "", err
		}
	}
	return out.TargetID, nil
}

// Switches to a real tab when the attached one is internal or gone, nothing when the browser holds none
func (r *helperRun) ensureRealTab() (any, error) {
	tabs, err := r.listTabs(false)
	if err != nil || len(tabs) == 0 {
		return nil, err
	}
	if cur, err := r.currentTab(); err == nil && cur["url"] != "" && !isInternalURL(cur["url"].(string)) {
		return cur, nil
	}
	if _, err := r.switchTab(tabs[0]["targetId"].(string), false); err != nil {
		return nil, err
	}
	return tabs[0], nil
}

// Evaluates in the attached tab or in an iframe target attached for the call, the iframe session released after
func (r *helperRun) jsIn(expr, target string) (any, error) {
	if target == "" {
		return r.js(r.s.session, expr)
	}
	var out struct {
		SessionID *string `json:"sessionId"`
	}
	if err := r.call("Target.attachToTarget", map[string]any{"targetId": target, "flatten": true}, &out); err != nil {
		return nil, err
	}
	if out.SessionID == nil {
		return nil, errors.New("Target.attachToTarget did not return a sessionId")
	}
	value, err := r.js(*out.SessionID, expr)
	derr := r.call("Target.detachFromTarget", map[string]any{"sessionId": *out.SessionID}, nil)
	if err != nil {
		return nil, err
	}
	if derr != nil {
		msg := strings.ToLower(derr.Error())
		if !strings.Contains(msg, "no session with given id") && !strings.Contains(msg, "session with given id not found") {
			return nil, derr
		}
	}
	return value, nil
}

// Brings the attached tab forward when the page reports it hidden, capturing anyway on any failure
func (r *helperRun) bringToFrontIfHidden() {
	var state struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
	}
	if err := r.call("Runtime.evaluate", map[string]any{"expression": "document.visibilityState", "returnByValue": true}, &state); err != nil || state.Result.Value != "hidden" {
		return
	}
	cur, err := r.currentTab()
	if err != nil {
		return
	}
	_ = r.call("Target.activateTarget", map[string]any{"targetId": cur["targetId"]}, nil)
}

// Saves a screenshot of the attached tab, a png within its deadline else a jpeg, shrunk to max_dim when asked
func (r *helperRun) captureScreenshot(path string, full json.RawMessage, maxDim float64) (string, error) {
	if path == "" {
		if err := os.MkdirAll(r.shotDir, 0o700); err != nil {
			return "", err
		}
		path = filepath.Join(r.shotDir, "shot.png")
	}
	if r.activate {
		r.bringToFrontIfHidden()
	}
	shot := func(params map[string]any, deadline time.Duration) (string, error) {
		raw, err := r.send(r.s.session, "Page.captureScreenshot", params, deadline)
		if err != nil {
			return "", err
		}
		var out map[string]json.RawMessage
		if err := json.Unmarshal(raw, &out); err != nil {
			return "", err
		}
		data, ok := out["data"]
		if !ok {
			return "", errors.New("'data'")
		}
		var text string
		err = json.Unmarshal(data, &text)
		return text, err
	}
	var timeout helperTimeout
	format := "png"
	data, err := shot(map[string]any{"format": "png", "captureBeyondViewport": full}, pngDeadline)
	if errors.As(err, &timeout) {
		format = "jpeg"
		data, err = shot(map[string]any{"format": "jpeg", "quality": 90, "captureBeyondViewport": full}, jpegDeadline)
		if errors.As(err, &timeout) {
			err = fmt.Errorf("Page.captureScreenshot timed out (png after %ss, jpeg after %ss)", secondsG(pngDeadline), secondsG(jpegDeadline))
		}
	}
	if err != nil {
		return "", err
	}
	bytesOut, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return "", err
	}
	if err := writeShot(bytesOut, path, format); err != nil {
		return "", err
	}
	if maxDim != 0 {
		if err := shrinkImageFile(path, int(math.Floor(maxDim))); err != nil {
			return "", err
		}
	}
	return path, nil
}

// Writes the capture, a jpeg turned into a png when the path says png since callers trust the extension
func writeShot(data []byte, path, format string) error {
	if format == "jpeg" && strings.HasSuffix(strings.ToLower(path), ".png") {
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return err
		}
		data = buf.Bytes()
	}
	return os.WriteFile(path, data, 0o666)
}

// Shrinks the image file to fit a side by side box, saved in the format its extension names
func shrinkImageFile(path string, side int) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return err
	}
	size := img.Bounds().Size()
	if max(size.X, size.Y) <= side {
		return nil
	}
	hasAlpha := false
	switch img.(type) {
	case *image.NRGBA, *image.NRGBA64:
		hasAlpha = true
	}
	thumb := pypil.Thumbnail(pypil.ToNRGBA(img), side, hasAlpha)
	var buf bytes.Buffer
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".png", ".apng":
		err = png.Encode(&buf, thumb)
	case ".jpg", ".jpeg", ".jpe", ".jfif":
		if hasAlpha {
			return errors.New("cannot write mode RGBA as JPEG")
		}
		err = jpeg.Encode(&buf, thumb, &jpeg.Options{Quality: 75})
	case ".gif":
		err = gif.Encode(&buf, thumb, nil)
	case ".bmp", ".dib":
		err = bmp.Encode(&buf, thumb)
	case ".tif", ".tiff":
		err = tiff.Encode(&buf, thumb, nil)
	case ".webp":
		err = nativewebp.Encode(&buf, thumb, nil)
	default:
		return fmt.Errorf("unknown file extension: %s", ext)
	}
	if err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o666)
}

// helperArgs are the arguments of one helper call as the code bound them, by parameter name
type helperArgs map[string]json.RawMessage

func (a helperArgs) str(key string) string {
	var v string
	_ = json.Unmarshal(a[key], &v)
	return v
}

func (a helperArgs) num(key string) float64 {
	var v float64
	_ = json.Unmarshal(a[key], &v)
	return v
}

func (a helperArgs) truthy(key string) bool {
	var v any
	if json.Unmarshal(a[key], &v) != nil {
		return false
	}
	return pyrepr.Truthy(v)
}

// Raw argument, JSON null when the code did not bind it
func (a helperArgs) raw(key string) json.RawMessage {
	if v, ok := a[key]; ok {
		return v
	}
	return json.RawMessage("null")
}

// Target id of a raw id or of a tab dict the helpers returned, empty for None
func (a helperArgs) target(key string) string {
	var v any
	_ = json.Unmarshal(a[key], &v)
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		for _, k := range []string{"targetId", "target_id"} {
			if id, ok := t[k].(string); ok && id != "" {
				return id
			}
		}
	}
	return ""
}

// Runs one helper the code called and returns the value it hands back
func (r *helperRun) helper(name string, a helperArgs) (any, error) {
	switch name {
	case "cdp":
		session := r.s.session
		var explicit string
		if json.Unmarshal(a["session_id"], &explicit) == nil && explicit != "" {
			session = explicit
		}
		method := a.str("method")
		if strings.HasPrefix(method, "Target.") {
			session = ""
		}
		timeout := time.Duration(a.num("response_timeout") * float64(time.Second))
		if string(a.raw("response_timeout")) == "null" {
			timeout = execNoDeadline
		}
		params := a.raw("params")
		if string(params) == "null" {
			params = json.RawMessage("{}")
		}
		raw, err := r.send(session, method, params, timeout)
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			return map[string]any{}, nil
		}
		return raw, nil
	case "drain_events":
		events := []map[string]any{}
		for _, ev := range r.drainEvents() {
			params := ev.Params
			if len(params) == 0 {
				params = json.RawMessage("{}")
			}
			var session any
			if ev.SessionID != "" {
				session = ev.SessionID
			}
			events = append(events, map[string]any{"method": ev.Method, "params": params, "session_id": session})
		}
		return events, nil
	case "goto_url":
		return r.gotoURL(a.str("url"), a.truthy("domain_skills"))
	case "page_info":
		r.s.eventMu.Lock()
		dialog := r.s.dialog
		r.s.eventMu.Unlock()
		if dialog != nil {
			return map[string]any{"dialog": dialog}, nil
		}
		value, err := r.evaluate(r.s.session, "JSON.stringify({url:location.href,title:document.title,w:innerWidth,h:innerHeight,"+
			"sx:scrollX,sy:scrollY,pw:document.documentElement.scrollWidth,ph:document.documentElement.scrollHeight})", false)
		if err != nil {
			return nil, err
		}
		var text string
		if raw, ok := value.(json.RawMessage); !ok || json.Unmarshal(raw, &text) != nil || !json.Valid([]byte(text)) {
			return nil, errors.New("page_info: the page did not describe itself as JSON")
		}
		return json.RawMessage(text), nil
	case "click_at_xy":
		for _, kind := range []string{"mousePressed", "mouseReleased"} {
			if err := r.call("Input.dispatchMouseEvent", map[string]any{"type": kind, "x": a.raw("x"), "y": a.raw("y"),
				"button": a.raw("button"), "clickCount": a.raw("clicks")}, nil); err != nil {
				return nil, err
			}
		}
		return nil, nil
	case "type_text":
		return nil, r.call("Input.insertText", map[string]any{"text": a.raw("text")}, nil)
	case "fill_input":
		return nil, r.fillInput(a.str("selector"), a.str("text"), a.truthy("clear_first"), a.num("timeout"))
	case "press_key":
		return nil, r.pressKey(a.str("key"), int(a.num("modifiers")))
	case "scroll":
		return nil, r.call("Input.dispatchMouseEvent", map[string]any{"type": "mouseWheel", "x": a.raw("x"), "y": a.raw("y"),
			"deltaX": a.raw("dx"), "deltaY": a.raw("dy")}, nil)
	case "capture_screenshot":
		return r.captureScreenshot(a.str("path"), a.raw("full"), a.num("max_dim"))
	case "list_tabs":
		return r.listTabs(a.truthy("include_chrome"))
	case "current_tab":
		return r.currentTab()
	case "activate_tab":
		target := a.target("target")
		return target, r.call("Target.activateTarget", map[string]any{"targetId": target}, nil)
	case "switch_tab":
		return r.switchTab(a.target("target"), a.truthy("activate"))
	case "new_tab":
		return r.newTab(a.str("url"))
	case "close_tab":
		target := a.target("target")
		if target == "" {
			cur, err := r.currentTab()
			if err != nil {
				return nil, err
			}
			target = cur["targetId"].(string)
		}
		return nil, r.call("Target.closeTarget", map[string]any{"targetId": target}, nil)
	case "ensure_real_tab":
		return r.ensureRealTab()
	case "iframe_target":
		var out struct {
			TargetInfos []TargetInfo `json:"targetInfos"`
		}
		if err := r.call("Target.getTargets", nil, &out); err != nil {
			return nil, err
		}
		for _, t := range out.TargetInfos {
			if t.Type == "iframe" && strings.Contains(t.URL, a.str("url_substr")) {
				return t.TargetID, nil
			}
		}
		return nil, nil
	case "wait_for_load":
		return r.poll("document.readyState", a.num("timeout"), func(v any) bool {
			raw, ok := v.(json.RawMessage)
			return ok && string(raw) == `"complete"`
		})
	case "wait_for_element":
		return r.waitForElement(a.str("selector"), a.num("timeout"), a.truthy("visible"))
	case "wait_for_network_idle":
		return r.waitNetworkIdle(a.num("timeout"), a.num("idle_ms"))
	case "js":
		return r.jsIn(a.str("expression"), a.target("target_id"))
	case "dispatch_key":
		key := a.str("key")
		code, ok := domKeyCodes[key]
		if runes := []rune(key); !ok && len(runes) == 1 {
			code = int(runes[0])
		}
		_, err := r.js(r.s.session, fmt.Sprintf("(()=>{const e=document.querySelector(%s);if(e){e.focus();e.dispatchEvent(new KeyboardEvent(%s,"+
			"{key:%s,code:%s,keyCode:%d,which:%d,bubbles:true}));}})()", quoteJS(a.str("selector")), quoteJS(a.str("event")), quoteJS(key), quoteJS(key), code, code))
		return nil, err
	case "upload_file":
		var doc struct {
			Root struct {
				NodeID int `json:"nodeId"`
			} `json:"root"`
		}
		if err := r.call("DOM.getDocument", map[string]any{"depth": -1}, &doc); err != nil {
			return nil, err
		}
		var node struct {
			NodeID int `json:"nodeId"`
		}
		if err := r.call("DOM.querySelector", map[string]any{"nodeId": doc.Root.NodeID, "selector": a.raw("selector")}, &node); err != nil {
			return nil, err
		}
		if node.NodeID == 0 {
			return nil, fmt.Errorf("no element for %s", a.str("selector"))
		}
		files := a.raw("path")
		var single string
		if json.Unmarshal(files, &single) == nil {
			files, _ = json.Marshal([]string{single})
		}
		return nil, r.call("DOM.setFileInputFiles", map[string]any{"files": files, "nodeId": node.NodeID}, nil)
	}
	return nil, fmt.Errorf("%s is not a browser helper", pyrepr.StrRepr(name))
}

// Names every helper the code's preamble defines that Go answers
var servedHelpers = []string{"cdp", "drain_events", "goto_url", "page_info", "click_at_xy", "type_text", "fill_input", "press_key",
	"scroll", "capture_screenshot", "list_tabs", "current_tab", "activate_tab", "switch_tab", "new_tab", "close_tab",
	"ensure_real_tab", "iframe_target", "wait_for_load", "wait_for_element", "wait_for_network_idle", "js", "dispatch_key", "upload_file"}

// The reply line of one request, its value or the error the code raises
func (r *helperRun) answer(line []byte) []byte {
	var req struct {
		Helper string     `json:"helper"`
		Args   helperArgs `json:"args"`
	}
	var value any
	err := json.Unmarshal(line, &req)
	if err == nil {
		if !slices.Contains(servedHelpers, req.Helper) {
			err = fmt.Errorf("%s is not a browser helper", pyrepr.StrRepr(req.Helper))
		} else {
			value, err = r.helper(req.Helper, req.Args)
		}
	}
	var reply map[string]any
	if err == nil {
		if text, ok := value.(jsUnserializable); ok {
			reply = map[string]any{"unserializable": string(text)}
		} else {
			reply = map[string]any{"result": value}
		}
	} else {
		reply = helperErrorReply(err)
	}
	data, merr := json.Marshal(reply)
	if merr != nil {
		data, _ = json.Marshal(map[string]any{"error": merr.Error(), "type": "RuntimeError"})
	}
	return data
}

// Error reply naming the exception type the code raises, errno fields for a file system failure
func helperErrorReply(err error) map[string]any {
	var timeout helperTimeout
	if errors.As(err, &timeout) {
		return map[string]any{"error": err.Error(), "type": "TimeoutError"}
	}
	var pathErr *os.PathError
	var errno syscall.Errno
	if errors.As(err, &pathErr) && errors.As(pathErr.Err, &errno) {
		args := []any{int(errno), pypath.Strerror(errno), pathErr.Path}
		if runtime.GOOS == "windows" {
			args = []any{0, strings.TrimRight(errno.Error(), " .\r\n"), pathErr.Path, int(errno)}
		}
		return map[string]any{"error": pypath.OSErrorText(err), "type": "OSError", "oserror": args}
	}
	return map[string]any{"error": err.Error(), "type": "RuntimeError"}
}

// Answers each request line of the child until it closes its end
func (r *helperRun) serve(requests io.Reader, replies io.Writer) {
	var pending []byte
	buf := make([]byte, 64<<10)
	for {
		n, err := requests.Read(buf)
		pending = append(pending, buf[:n]...)
		for {
			i := bytes.IndexByte(pending, '\n')
			if i < 0 {
				break
			}
			line := pending[:i]
			pending = pending[i+1:]
			if _, werr := replies.Write(append(r.answer(line), '\n')); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
