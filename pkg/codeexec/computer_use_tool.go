package codeexec

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Approval prompt handed to the shared gate, the gate resolving the per thread callback when nil
var cuApprovalCallback tools.Asker

// Installs the computer use specific approval prompt
func SetComputerUseApprovalCallback(cb tools.Asker) { cuApprovalCallback = cb }

var cuBlockedKeyCombos = [][]string{
	{"backspace", "cmd", "shift"}, {"backspace", "cmd", "option"},
	{"cmd", "ctrl", "q"}, {"cmd", "q", "shift"},
	{"cmd", "option", "q", "shift"}, {"l", "win"},
	{"ctrl", "delete", "option"}, {"ctrl", "del", "option"}, {"f4", "option"},
}

var cuKeyAliases = map[string]string{"command": "cmd", "control": "ctrl", "alt": "option", "⌘": "cmd", "⌥": "option",
	"windows": "win", "super": "win", "meta": "win"}

var cuBlockedTypePatterns = func() []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, p := range []string{`curl\s+[^|]*\|\s*bash`, `curl\s+[^|]*\|\s*sh`, `wget\s+[^|]*\|\s*bash`,
		`\bsudo\s+rm\s+-[rf]`, `\brm\s+-rf\s+/\s*$`, `:\s*\(\)\s*\{\s*:\|:\s*&\s*\}`} {
		out = append(out, regexp.MustCompile(`(?i)`+p))
	}
	return out
}()

var cuBlockedTypeSources = []string{`curl\s+[^|]*\|\s*bash`, `curl\s+[^|]*\|\s*sh`, `wget\s+[^|]*\|\s*bash`,
	`\bsudo\s+rm\s+-[rf]`, `\brm\s+-rf\s+/\s*$`, `:\s*\(\)\s*\{\s*:\|:\s*&\s*\}`}

var cuKeySplit = regexp.MustCompile(`\s*[+\-]\s*`)

// Lowercased key set of a combo split on plus and hyphen with the platform aliases folded
func canonKeyCombo(keys string) map[string]bool {
	out := map[string]bool{}
	for _, q := range cuKeySplit.Split(keys, -1) {
		p := strings.ToLower(strings.TrimSpace(q))
		if alias, ok := cuKeyAliases[p]; ok {
			p = alias
		}
		if p != "" {
			out[p] = true
		}
	}
	return out
}

func cuArgStr(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

// JSON error for hard blocked input, empty when the action may proceed to approval
func rejectUnsafe(action string, args map[string]any) string {
	if action == "type" {
		text := cuArgStr(args, "text")
		for i, p := range cuBlockedTypePatterns {
			if p.MatchString(text) {
				return cuDumps(map[string]any{"error": "blocked pattern in type text: " + pyrepr.Repr(cuBlockedTypeSources[i]),
					"hint": "Dangerous shell patterns cannot be typed via computer_use."}, "error", "hint")
			}
		}
	}
	if action == "key" {
		combo := canonKeyCombo(cuArgStr(args, "keys"))
		for _, blocked := range cuBlockedKeyCombos {
			subset := true
			for _, k := range blocked {
				subset = subset && combo[k]
			}
			if subset {
				items := make([]any, len(blocked))
				for i, k := range blocked {
					items[i] = k
				}
				return cuDumps(map[string]any{"error": "blocked key combo: " + pyrepr.Repr(items),
					"hint": "Destructive system shortcuts are hard-blocked."}, "error", "hint")
			}
		}
	}
	if pyrepr.Truthy(args["bring_to_front"]) && args["delivery_mode"] != "foreground" {
		return cuDumps(map[string]any{"error": "bring_to_front requires delivery_mode='foreground'",
			"code": "bring_to_front_requires_foreground"}, "error", "code")
	}
	return ""
}

// json.dumps of a dict built in the given key order
func cuDumps(m map[string]any, keys ...string) string {
	o := pyjson.NewObject()
	for _, k := range keys {
		o.Set(k, m[k])
	}
	out, err := pyjson.Dumps(o)
	if err != nil {
		panic(err)
	}
	return out
}

// Backend exposing the sticky target app an input action would land on
type lastAppBackend interface{ lastAppName() *string }

func (b *CuaDriverBackend) lastAppName() *string { return b.lastApp }

// Sticky target app when it provably differs from the requested one, nil when unknown or overlapping
func inputTargetMismatch(backend ComputerUseBackend, requestedApp string) *string {
	var last *string
	if lb, ok := backend.(lastAppBackend); ok {
		last = lb.lastAppName()
	}
	current := ""
	if last != nil {
		current = strings.ToLower(strings.TrimSpace(*last))
	}
	wanted := strings.ToLower(strings.TrimSpace(requestedApp))
	if current == "" || wanted == "" || strings.Contains(current, wanted) || strings.Contains(wanted, current) {
		return nil
	}
	return last
}

// Per session backend caches, the empty session mirrored onto the injection hook older tests populate
var (
	cuBackendMu             sync.Mutex
	cuInjectedBackend       ComputerUseBackend
	cuBackends              = map[string]ComputerUseBackend{}
	cuBackendCallLocks      = map[string]*sync.Mutex{}
	cuBackendPermissionMode = map[string]string{}
	cuBackendDisplays       = map[string]string{}
	cuApprovalMu            sync.Mutex
	cuEscalationWarned      = map[string]bool{}
)

// Screenshot dedup state per scoped session, digest target and omission streak
type screenshotState struct {
	digest string
	target [2]string
	streak int
}

var (
	cuScreenshotMu    sync.Mutex
	cuLastScreenshot  = map[string]*screenshotState{}
	cuDedupMaxStreak  = 2
	cuWarnEscalation  = func(msg string) { slog.Warn(msg) }
	cuDebugStopFailed = func(msg string) { slog.Debug(msg) }
)

// True when the frame repeats the previous one of the same target below the streak cap, any miss storing it
func screenshotDedupCheck(sessionID, digest string, target [2]string) bool {
	cuScreenshotMu.Lock()
	defer cuScreenshotMu.Unlock()
	if st := cuLastScreenshot[sessionID]; st != nil && st.digest == digest && st.target == target && st.streak < cuDedupMaxStreak {
		st.streak++
		return true
	}
	cuLastScreenshot[sessionID] = &screenshotState{digest: digest, target: target}
	return false
}

// Forgets dedup state of every session when all is set, else of one scoped key
func resetScreenshotDedupState(sessionID string, all bool) {
	cuScreenshotMu.Lock()
	defer cuScreenshotMu.Unlock()
	if all {
		clear(cuLastScreenshot)
		return
	}
	delete(cuLastScreenshot, sessionID)
}

// Compaction boundary hook so the next capture of the session delivers pixels again
func ResetScreenshotDedup(ctx context.Context, sessionID string) {
	resetScreenshotDedupState(scopedSID(ctx, sessionID), false)
}

// Cache key of a session backend, the served profile home key appended under a home override
func scopedSID(ctx context.Context, sessionID string) string {
	override := home.Override(ctx)
	if override == "" {
		return sessionID
	}
	return sessionID + "@" + home.Key(override)
}

// Driver permission mode, unrestricted under an approval bypass with a one time warning per session
func cuaPermissionMode(env *tools.Env, sessionID string) string {
	configured := cuaConfiguredPermissionMode(env)
	bypass := tools.IsApprovalBypassActiveForSession(env, sessionID)
	if !bypass {
		if key := tools.CurrentSessionKeyOr(env, ""); key != "" {
			bypass = tools.IsApprovalBypassActiveForSession(env, key)
		}
	}
	if !bypass {
		return configured
	}
	cuApprovalMu.Lock()
	warn := !cuEscalationWarned[sessionID]
	cuEscalationWarned[sessionID] = true
	cuApprovalMu.Unlock()
	if warn {
		cuWarnEscalation(fmt.Sprintf("computer_use: approval bypass (--yolo / -z) escalated the cua-driver permission mode from the "+
			"configured '%s' to 'unrestricted' for this session. Runtime approval prompts are disabled and the "+
			"driver's residual ceilings no longer apply. Drop the bypass flag to keep '%s', or declare a "+
			"version-3 computer_use.capability_manifest to keep a ceiling on bypassed runs.", configured, configured))
	}
	return "unrestricted"
}

// Backend the active provider creates, a seam the tests swap
var newComputerUseBackend = func(env *tools.Env, permissionMode string) (ComputerUseBackend, error) {
	provider, err := getActiveProvider(env)
	if err != nil {
		return nil, err
	}
	return provider.CreateBackend(permissionMode)
}

// Records a backend in the session caches, the caller holding cuBackendMu
func installBackend(env *tools.Env, sid string, backend ComputerUseBackend, permissionMode string) ComputerUseBackend {
	cuBackends[sid], cuBackendPermissionMode[sid] = backend, permissionMode
	cuBackendDisplays[sid] = desktopIdentity(env, nil)
	cuBackendCallLocks[sid] = &sync.Mutex{}
	if sid == "" {
		cuInjectedBackend = backend
	}
	return backend
}

// Removes one session from the caches with the injection hook when it aliases the empty session
func detachLocked(sid string) (ComputerUseBackend, *sync.Mutex) {
	delete(cuBackendPermissionMode, sid)
	delete(cuBackendDisplays, sid)
	backend, callLock := cuBackends[sid], cuBackendCallLocks[sid]
	delete(cuBackends, sid)
	delete(cuBackendCallLocks, sid)
	if sid == "" {
		if backend == nil {
			backend = cuInjectedBackend
		}
		if cuInjectedBackend == backend {
			cuInjectedBackend = nil
		}
	}
	return backend, callLock
}

// Stops a backend under its call lock so an in flight action finishes, the failure absorbed by onError
func stopBackend(backend ComputerUseBackend, callLock *sync.Mutex, onError func(error)) {
	if callLock != nil {
		callLock.Lock()
		defer callLock.Unlock()
	}
	defer func() {
		if r := recover(); r != nil {
			onError(fmt.Errorf("%v", r))
		}
	}()
	if err := backend.Stop(); err != nil {
		onError(err)
	}
}

// Live backend of a session, replaced when the permission mode or the desktop identity changed
func getBackend(ctx context.Context, env *tools.Env, sessionID string) (ComputerUseBackend, error) {
	sid := scopedSID(ctx, sessionID)
	for {
		cuBackendMu.Lock()
		permissionMode := cuaPermissionMode(env, sessionID)
		if _, cached := cuBackends[sid]; sid == "" && cuInjectedBackend != nil && !cached {
			installBackend(env, sid, cuInjectedBackend, permissionMode)
		}
		cached := cuBackends[sid]
		if cached == nil {
			backend, err := newComputerUseBackend(env, permissionMode)
			if err == nil {
				err = backend.Start()
			}
			if err != nil {
				cuBackendMu.Unlock()
				return nil, err
			}
			installBackend(env, sid, backend, permissionMode)
			cuBackendMu.Unlock()
			return backend, nil
		}
		mode, ok := cuBackendPermissionMode[sid]
		if !ok {
			mode = "standard"
		}
		if mode == permissionMode && !backendDisplayStale(cuBackendDisplays[sid], desktopIdentity(env, nil)) {
			cuBackendMu.Unlock()
			return cached, nil
		}
		_, staleLock := detachLocked(sid)
		cuBackendMu.Unlock()
		stopBackend(cached, staleLock, func(error) {})
	}
}

// Admits a live backend and holds its call lock, revalidating after the wait, release unlocks it
func backendForCall(ctx context.Context, env *tools.Env, sessionID string) (ComputerUseBackend, func(), error) {
	sid := scopedSID(ctx, sessionID)
	for {
		backend, err := getBackend(ctx, env, sessionID)
		if err != nil {
			return nil, nil, err
		}
		cuBackendMu.Lock()
		if cuBackends[sid] != backend {
			cuBackendMu.Unlock()
			continue
		}
		callLock := cuBackendCallLocks[sid]
		cuBackendMu.Unlock()
		callLock.Lock()
		cuBackendMu.Lock()
		stale := cuBackends[sid] != backend || cuBackendCallLocks[sid] != callLock ||
			cuBackendPermissionMode[sid] != cuaPermissionMode(env, sessionID) ||
			backendDisplayStale(cuBackendDisplays[sid], desktopIdentity(env, nil))
		cuBackendMu.Unlock()
		if stale {
			callLock.Unlock()
			continue
		}
		return backend, callLock.Unlock, nil
	}
}

// Releases one session owned backend, true when one was released
func ReleaseComputerUseSession(ctx context.Context, sessionID string) bool {
	sid := scopedSID(ctx, sessionID)
	resetScreenshotDedupState(sid, false)
	cuBackendMu.Lock()
	backend, callLock := detachLocked(sid)
	cuBackendMu.Unlock()
	if backend == nil {
		return false
	}
	stopBackend(backend, callLock, func(err error) {
		cuDebugStopFailed(fmt.Sprintf("computer_use backend release failed for session %s: %v", sid, err))
	})
	return true
}

// Stops every cached backend so no cua-driver outlives the process, never raising
func ShutdownComputerUseBackends() {
	type entry struct {
		backend ComputerUseBackend
		lock    *sync.Mutex
	}
	cuBackendMu.Lock()
	var unique []entry
	seen := map[ComputerUseBackend]bool{}
	sids := make([]string, 0, len(cuBackends))
	for sid := range cuBackends {
		sids = append(sids, sid)
	}
	sort.Strings(sids)
	for _, sid := range sids {
		if b := cuBackends[sid]; !seen[b] {
			seen[b] = true
			unique = append(unique, entry{b, cuBackendCallLocks[sid]})
		}
	}
	if cuInjectedBackend != nil && !seen[cuInjectedBackend] {
		unique = append(unique, entry{cuInjectedBackend, cuBackendCallLocks[""]})
	}
	cuInjectedBackend = nil
	clear(cuBackends)
	clear(cuBackendCallLocks)
	clear(cuBackendPermissionMode)
	clear(cuBackendDisplays)
	cuBackendMu.Unlock()
	cuApprovalMu.Lock()
	clear(cuEscalationWarned)
	cuApprovalMu.Unlock()
	for _, e := range unique {
		stopBackend(e.backend, e.lock, func(err error) { cuDebugStopFailed(fmt.Sprintf("cua-driver atexit teardown failed: %v", err)) })
	}
}

// Tears down every cached backend and the dedup state between tests
func resetBackendForTests() {
	ShutdownComputerUseBackends()
	resetScreenshotDedupState("", true)
}

// Recording backend the tests install through newComputerUseBackend
type noopBackend struct {
	BackendDefaults
	mu    sync.Mutex
	calls []noopCall
}

type noopCall struct {
	name string
	args map[string]any
}

func (b *noopBackend) record(name string, args map[string]any) *ActionResult {
	b.mu.Lock()
	b.calls = append(b.calls, noopCall{name, args})
	b.mu.Unlock()
	return &ActionResult{OK: true, Action: name, Meta: pyjson.NewObject()}
}

func (b *noopBackend) Start() error               { return nil }
func (b *noopBackend) Stop() error                { return nil }
func (b *noopBackend) IsAvailable() (bool, error) { return true, nil }

func (b *noopBackend) Capture(mode string, app *string, pid, windowID *int) (*CaptureResult, error) {
	b.record("capture", map[string]any{"mode": mode, "app": optStr(app), "pid": optInt(pid), "window_id": optInt(windowID)})
	if mode == "" {
		mode = "som"
	}
	a := ""
	if app != nil {
		a = *app
	}
	return &CaptureResult{Mode: mode, Width: 1024, Height: 768, App: a}, nil
}

func (b *noopBackend) Click(a ClickArgs) (*ActionResult, error) {
	return b.record("click", map[string]any{"element": optInt(a.Element), "x": optInt(a.X), "y": optInt(a.Y), "button": a.Button,
		"click_count": a.ClickCount, "modifiers": optStrs(a.Modifiers), "delivery_mode": optStr(a.DeliveryMode), "bring_to_front": a.BringToFront}), nil
}

func (b *noopBackend) Drag(a DragArgs) (*ActionResult, error) {
	return b.record("drag", map[string]any{"from_element": optInt(a.FromElement), "to_element": optInt(a.ToElement), "from_xy": optXY(a.FromXY),
		"to_xy": optXY(a.ToXY), "button": a.Button, "modifiers": optStrs(a.Modifiers), "delivery_mode": optStr(a.DeliveryMode), "bring_to_front": a.BringToFront}), nil
}

func (b *noopBackend) Scroll(a ScrollArgs) (*ActionResult, error) {
	return b.record("scroll", map[string]any{"direction": a.Direction, "amount": a.Amount, "element": optInt(a.Element), "x": optInt(a.X),
		"y": optInt(a.Y), "modifiers": optStrs(a.Modifiers), "delivery_mode": optStr(a.DeliveryMode), "bring_to_front": a.BringToFront}), nil
}

func (b *noopBackend) TypeText(text string, d DeliveryArgs) (*ActionResult, error) {
	return b.record("type", map[string]any{"text": text, "delivery_mode": optStr(d.DeliveryMode), "bring_to_front": d.BringToFront}), nil
}

func (b *noopBackend) Key(keys string, d DeliveryArgs) (*ActionResult, error) {
	return b.record("key", map[string]any{"keys": keys, "delivery_mode": optStr(d.DeliveryMode), "bring_to_front": d.BringToFront}), nil
}

func (b *noopBackend) ListApps() ([]any, error) {
	b.record("list_apps", map[string]any{})
	return []any{}, nil
}

func (b *noopBackend) ListWindows() ([]*pyjson.Object, error) {
	b.record("list_windows", map[string]any{})
	return []*pyjson.Object{}, nil
}

func (b *noopBackend) FocusApp(app string, raiseWindow bool) (*ActionResult, error) {
	return b.record("focus_app", map[string]any{"app": app, "raise_window": raiseWindow}), nil
}

func (b *noopBackend) SetValue(value string, element *int) (*ActionResult, error) {
	return b.record("set_value", map[string]any{"value": value, "element": optInt(element)}), nil
}

const cuForegroundSuffix = " [FOREGROUND — briefly raises the window / changes focus]"

// Empty when the shared approval gate approves, else the JSON refusal under the cua scope key
func requestApproval(ctx context.Context, env *tools.Env, action string, args map[string]any) string {
	mode := "background"
	if args["delivery_mode"] == "foreground" {
		mode = "foreground"
	}
	result := tools.RunApprovalGate(ctx, env, tools.ApprovalGateOptions{
		PatternKey: "cua:" + action + ":" + mode, Description: "Allow computer_use to perform `" + action + "`?",
		DisplayTarget: "computer_use: " + summarizeAction(action, args), ApprovalCallback: cuApprovalCallback,
		Subject: "computer_use `" + action + "` requires approval", Noun: "desktop actions",
		Advice:                "Find an alternative approach that avoids driving the desktop.",
		AutoapproveLogPrefix:  "computer_use action in non-interactive non-gateway context",
		FailClosedWhenNoHuman: true,
		NoHumanBlockMessage: "BLOCKED: computer_use `" + action + "` requires approval but no interactive user or " +
			"gateway is present to approve it.",
	})
	if result.Approved {
		return ""
	}
	msg := result.Message
	if msg == "" {
		msg = "denied by user"
	}
	return cuDumps(map[string]any{"error": msg, "action": action}, "error", "action")
}

func cuArgOr(args map[string]any, key string, def any) any {
	if v, ok := args[key]; ok {
		return v
	}
	return def
}

// One line approval prompt of an action, the foreground suffix appended where hermes appends it
func summarizeAction(action string, args map[string]any) string {
	fg := ""
	if args["delivery_mode"] == "foreground" {
		fg = cuForegroundSuffix
	}
	switch action {
	case "click", "double_click", "right_click", "middle_click":
		return summarizeClick(action, args, fg)
	case "drag":
		from, to := args["from_element"], args["to_element"]
		if !pyrepr.Truthy(from) {
			from = args["from_coordinate"]
		}
		if !pyrepr.Truthy(to) {
			to = args["to_coordinate"]
		}
		return "drag " + pyrepr.Str(from) + " → " + pyrepr.Str(to) + fg
	case "scroll":
		return "scroll " + pyrepr.Str(cuArgOr(args, "direction", "?")) + " x" + pyrepr.Str(cuArgOr(args, "amount", 3)) + fg
	case "type":
		text := []rune(cuArgStr(args, "text"))
		head, more := text, ""
		if len(text) > 60 {
			head, more = text[:60], "..."
		}
		return "type " + pyrepr.Repr(string(head)) + more + fg
	case "key":
		return "key " + pyrepr.Repr(cuArgOr(args, "keys", "")) + fg
	case "focus_app":
		raise := ""
		if pyrepr.Truthy(args["raise_window"]) {
			raise = " (raise)"
		}
		return "focus " + pyrepr.Repr(cuArgOr(args, "app", "")) + raise
	}
	return action + fg
}

// Click prompt naming the element index or the coordinate tuple
func summarizeClick(action string, args map[string]any, fg string) string {
	where := ""
	if el, ok := args["element"]; ok && el != nil {
		where = " element #" + pyrepr.Str(el)
	} else if coord := args["coordinate"]; pyrepr.Truthy(coord) {
		items, _ := coord.([]any)
		where = " at " + pyrepr.Repr(pyrepr.Tuple(items))
	}
	return action + where + fg
}

func cuCoord(args map[string]any, key string) []any {
	c, _ := args[key].([]any)
	return c
}

// Click point, counted only when its x is set
func xyArgs(args map[string]any) (x, y any) {
	if c := cuCoord(args, "coordinate"); len(c) > 0 && c[0] != nil {
		if len(c) > 1 {
			return c[0], c[1]
		}
		return c[0], nil
	}
	return nil, nil
}

// Scroll point with independent axes
func scrollXYArgs(args map[string]any) (x, y any) {
	c := cuCoord(args, "coordinate")
	if len(c) > 0 {
		x = c[0]
	}
	if len(c) > 1 {
		y = c[1]
	}
	return x, y
}

// Integer value as isinstance int sees it, bools counting as ints
func cuIsInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// Next ladder step from the semantic evidence of an action in precedence order
func classifyActionResult(res *ActionResult) *pyjson.Object {
	out := pyjson.NewObject()
	effect := ""
	if res.Effect != nil {
		effect = *res.Effect
	}
	if effect == "confirmed" || (res.Verified != nil && *res.Verified) {
		out.Set("decision", "done")
		return out
	}
	if effect == "unverifiable" {
		out.Set("decision", "verify_fresh_state")
		out.Set("hint", "Input was delivered but not confirmed. Re-capture and check the "+
			"result BEFORE any retry — do not repeat the input on an escalation recommendation alone.")
		return out
	}
	if effect == "suspected_noop" || !res.OK || res.Code != nil {
		var delivered, requested any
		if res.Meta != nil {
			delivered, _ = res.Meta.Get("delivered_chars")
			requested, _ = res.Meta.Get("requested_chars")
		}
		d, dOK := cuIsInt(delivered)
		r, rOK := cuIsInt(requested)
		if (res.Action == "type" || res.Action == "type_text") && res.Code != nil && *res.Code == "type_text_incomplete" && dOK && rOK && r > 0 && d <= 0 {
			out.Set("decision", "escalate")
			out.Set("recommended", "set_value")
			out.Set("hint", "0 characters landed: this input drops synthetic keystrokes, so no delivery rung will fix it. "+
				"Climb to the set_value action on the field's element index instead — it is an action, not a "+
				"delivery mode, and sets the value through the accessibility API, bypassing event filtering. "+
				"Re-capture first if the index is stale.")
			return out
		}
		out.Set("decision", "escalate")
		if res.Escalation != nil {
			rec, _ := res.Escalation.Get("recommended")
			out.Set("recommended", rec)
		}
		out.Set("hint", "The input likely did not land. Climb one rung following `recommended`: 'px' → re-issue by coordinate; "+
			"'foreground' (or a failed pixel click) → re-issue with delivery_mode='foreground' (separate approval). "+
			"Do not predict the rung from the app being Electron/Chromium — react to this signal.")
		return out
	}
	out.Set("decision", "verify_fresh_state")
	out.Set("hint", "Transport succeeded but the effect is unproven. Re-capture and confirm before continuing.")
	return out
}

// Transport success, the driver verdict fields it returned, truthy message and meta, and the ladder verdict
func actionPayload(res *ActionResult) *pyjson.Object {
	out := pyjson.NewObject()
	out.Set("ok", res.OK)
	out.Set("action", res.Action)
	if res.Message != "" {
		out.Set("message", res.Message)
	}
	if res.Verified != nil {
		out.Set("verified", *res.Verified)
	}
	if res.Effect != nil {
		out.Set("effect", *res.Effect)
	}
	if res.Escalation != nil {
		out.Set("escalation", res.Escalation)
	}
	for _, f := range []struct {
		key string
		v   *string
	}{{"path", res.Path}} {
		if f.v != nil {
			out.Set(f.key, *f.v)
		}
	}
	if res.Degraded != nil {
		out.Set("degraded", *res.Degraded)
	}
	if res.DeliveryMode != nil {
		out.Set("delivery_mode", *res.DeliveryMode)
	}
	if res.Code != nil {
		out.Set("code", *res.Code)
	}
	if res.Meta != nil && len(res.Meta.Keys) > 0 {
		out.Set("meta", res.Meta)
	}
	out.Set("verdict", classifyActionResult(res))
	return out
}

// JSON text of the action payload
func textResponse(res *ActionResult) string {
	out, err := pyjson.Dumps(actionPayload(res))
	if err != nil {
		panic(err)
	}
	return out
}
