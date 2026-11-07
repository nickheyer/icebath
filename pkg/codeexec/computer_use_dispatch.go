package codeexec

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// AttributeError is the Python exception a non string action raises, named so the dispatcher reports its class
type AttributeError struct{ msg string }

func (e *AttributeError) Error() string { return e.msg }

// Raised when the screen lease moved since the action was admitted
type cuHumanHasControl struct{ msg string }

func (e *cuHumanHasControl) Error() string { return e.msg }

// Element cap of a capture response
const cuDefaultMaxElements = 100

const cuFenceMessage = "A human took over this desktop while the action ran; its result was discarded. " +
	"Tell the user what you need and re-capture once they hand back."

// One dispatched action with its backend, arguments and the read or input keyword set
type cuCall struct {
	ctx       context.Context
	env       *tools.Env
	backend   ComputerUseBackend
	action    string
	args      map[string]any
	delivery  DeliveryArgs
	fence     func() error
	sessionID *string
}

// Handler, input flag and destructive flag of one computer_use action
type cuActionSpec struct {
	handler     func(c *cuCall) (any, error)
	input       bool
	destructive bool
}

var cuActions map[string]cuActionSpec

func init() {
	click := func(button string, count int) func(c *cuCall) (any, error) {
		return func(c *cuCall) (any, error) { return doClick(c, button, count) }
	}
	in := func(h func(c *cuCall) (any, error)) cuActionSpec {
		return cuActionSpec{handler: h, input: true, destructive: true}
	}
	cuActions = map[string]cuActionSpec{
		"click": in(click("", 1)), "double_click": in(click("", 2)), "right_click": in(click("right", 1)),
		"middle_click": in(click("middle", 1)), "drag": in(doDrag), "scroll": in(doScroll),
		"type": in(func(c *cuCall) (any, error) {
			return c.backend.TypeText(cuStrOr(c.args, "text", ""), c.delivery)
		}),
		"key": in(func(c *cuCall) (any, error) {
			return c.backend.Key(cuStrOr(c.args, "keys", ""), c.delivery)
		}),
		"set_value": in(doSetValue),
		"focus_app": {handler: doFocusApp, destructive: true},
		"capture":   {handler: doCapture},
		"wait": {handler: func(c *cuCall) (any, error) {
			secs, err := cuPyFloat(cuArgOr(c.args, "seconds", 1.0))
			if err != nil {
				return nil, err
			}
			res, err := c.backend.Wait(secs)
			if err != nil {
				return nil, err
			}
			return textResponse(res), nil
		}},
		"list_apps":    {handler: func(c *cuCall) (any, error) { return doListing(c, "apps") }},
		"list_windows": {handler: func(c *cuCall) (any, error) { return doListing(c, "windows") }},
	}
}

// Nearest real action named as guidance for an unknown one, never an alias
var cuActionSuggestions = map[string]string{
	"hotkey": "key", "press_key": "key", "keypress": "key", "key_combo": "key", "shortcut": "key", "type_text": "type",
	"input_text": "type", "screenshot": "capture", "get_window_state": "capture", "left_click": "click", "mouse_click": "click",
}

// Optional integer argument, an error naming the key for a value no integer reads from
func cuOptInt(key string, v any) (*int, error) {
	if v == nil {
		return nil, nil
	}
	if n, ok := cuIsInt(v); ok {
		i := int(n)
		return &i, nil
	}
	if f, ok := v.(float64); ok && f == math.Trunc(f) && !math.IsInf(f, 0) {
		i := int(f)
		return &i, nil
	}
	return nil, fmt.Errorf("%s must be an integer, got %s", key, pyrepr.Repr(v))
}

// Optional string argument, an error naming the key for a non string value
func cuOptStr(key string, v any) (*string, error) {
	if v == nil {
		return nil, nil
	}
	if s, ok := v.(string); ok {
		return &s, nil
	}
	return nil, fmt.Errorf("%s must be a string, got %s", key, pyrepr.Repr(v))
}

// String argument with the default dict.get applies to an absent key
func cuStrOr(args map[string]any, key, def string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
		if v == nil {
			return ""
		}
		return pyrepr.Str(v)
	}
	return def
}

// Modifier keys as a string list, nil when absent
func cuModifiers(args map[string]any) ([]string, error) {
	raw, ok := args["modifiers"]
	if !ok || raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("modifiers must be a list of strings, got %s", pyrepr.Repr(raw))
	}
	out := make([]string, 0, len(list))
	for _, m := range list {
		s, ok := m.(string)
		if !ok {
			return nil, fmt.Errorf("modifiers must be a list of strings, got %s", pyrepr.Repr(raw))
		}
		out = append(out, s)
	}
	return out, nil
}

// Python float() of a tool argument with its ValueError and TypeError texts
func cuPyFloat(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case string:
		f, err := strconv.ParseFloat(strings.ReplaceAll(pystr.Strip(n), "_", ""), 64)
		if err != nil {
			return 0, fmt.Errorf("could not convert string to float: %s", pyrepr.Repr(n))
		}
		return f, nil
	}
	if i, ok := cuIsInt(v); ok {
		return float64(i), nil
	}
	return 0, fmt.Errorf("float() argument must be a string or a real number, not '%s'", pyrepr.TypeName(v))
}

// Python int() of a tool argument, floats truncated, with its ValueError and TypeError texts
func cuPyInt(v any) (int, error) {
	switch n := v.(type) {
	case float64:
		if math.IsInf(n, 0) || math.IsNaN(n) {
			return 0, fmt.Errorf("cannot convert float %s to integer", pyrepr.Repr(n))
		}
		return int(n), nil
	case string:
		i, err := strconv.ParseInt(strings.ReplaceAll(pystr.Strip(n), "_", ""), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.Repr(n))
		}
		return int(i), nil
	}
	if i, ok := cuIsInt(v); ok {
		return int(i), nil
	}
	return 0, fmt.Errorf("int() argument must be a string, a bytes-like object or a real number, not '%s'", pyrepr.TypeName(v))
}

// Coordinate pair a drag endpoint unpacks into, the unpacking ValueError otherwise
func cuPair(key string, c []any) (*[2]int, error) {
	if len(c) == 0 {
		return nil, nil
	}
	if len(c) > 2 {
		return nil, errors.New("too many values to unpack (expected 2)")
	}
	if len(c) < 2 {
		return nil, fmt.Errorf("not enough values to unpack (expected 2, got %d)", len(c))
	}
	x, err := cuOptInt(key, c[0])
	if err != nil {
		return nil, err
	}
	y, err := cuOptInt(key, c[1])
	if err != nil || x == nil || y == nil {
		return nil, fmt.Errorf("%s must hold two integers, got %s", key, pyrepr.Repr(c))
	}
	return &[2]int{*x, *y}, nil
}

// Click, double, right and middle click through the backend with the click point when set
func doClick(c *cuCall, button string, count int) (any, error) {
	element, err := cuOptInt("element", c.args["element"])
	if err != nil {
		return nil, err
	}
	rawX, rawY := xyArgs(c.args)
	x, err := cuOptInt("coordinate", rawX)
	if err != nil {
		return nil, err
	}
	y, err := cuOptInt("coordinate", rawY)
	if err != nil {
		return nil, err
	}
	mods, err := cuModifiers(c.args)
	if err != nil {
		return nil, err
	}
	if button == "" {
		if b, ok := c.args["button"].(string); ok && b != "" {
			button = b
		} else if pyrepr.Truthy(c.args["button"]) {
			button = pyrepr.Str(c.args["button"])
		} else {
			button = "left"
		}
	}
	return c.backend.Click(ClickArgs{Element: element, X: x, Y: y, Button: button, ClickCount: count, Modifiers: mods,
		DeliveryMode: c.delivery.DeliveryMode, BringToFront: c.delivery.BringToFront})
}

// Drag between elements or coordinates, an error JSON when neither pair is complete
func doDrag(c *cuCall) (any, error) {
	src, dst := cuCoord(c.args, "from_coordinate"), cuCoord(c.args, "to_coordinate")
	if (c.args["from_element"] == nil || c.args["to_element"] == nil) && !(len(src) > 0 && len(dst) > 0) {
		return cuDumps(map[string]any{"error": "drag requires from_coordinate/to_coordinate or from_element/to_element"}, "error"), nil
	}
	from, err := cuOptInt("from_element", c.args["from_element"])
	if err != nil {
		return nil, err
	}
	to, err := cuOptInt("to_element", c.args["to_element"])
	if err != nil {
		return nil, err
	}
	fromXY, err := cuPair("from_coordinate", src)
	if err != nil {
		return nil, err
	}
	toXY, err := cuPair("to_coordinate", dst)
	if err != nil {
		return nil, err
	}
	mods, err := cuModifiers(c.args)
	if err != nil {
		return nil, err
	}
	return c.backend.Drag(DragArgs{FromElement: from, ToElement: to, FromXY: fromXY, ToXY: toXY,
		Button: cuStrOr(c.args, "button", "left"), Modifiers: mods, DeliveryMode: c.delivery.DeliveryMode,
		BringToFront: c.delivery.BringToFront})
}

// Scroll in a direction by an amount at an element or point
func doScroll(c *cuCall) (any, error) {
	amount, err := cuPyInt(cuArgOr(c.args, "amount", int64(3)))
	if err != nil {
		return nil, err
	}
	element, err := cuOptInt("element", c.args["element"])
	if err != nil {
		return nil, err
	}
	rawX, rawY := scrollXYArgs(c.args)
	x, err := cuOptInt("coordinate", rawX)
	if err != nil {
		return nil, err
	}
	y, err := cuOptInt("coordinate", rawY)
	if err != nil {
		return nil, err
	}
	mods, err := cuModifiers(c.args)
	if err != nil {
		return nil, err
	}
	return c.backend.Scroll(ScrollArgs{Direction: cuStrOr(c.args, "direction", "down"), Amount: amount, Element: element,
		X: x, Y: y, Modifiers: mods, DeliveryMode: c.delivery.DeliveryMode, BringToFront: c.delivery.BringToFront})
}

// Sets a control value, an error JSON without a value
func doSetValue(c *cuCall) (any, error) {
	if c.args["value"] == nil {
		return cuDumps(map[string]any{"error": "set_value requires `value`"}, "error"), nil
	}
	element, err := cuOptInt("element", c.args["element"])
	if err != nil {
		return nil, err
	}
	value, ok := c.args["value"].(string)
	if !ok {
		value = pyrepr.Str(c.args["value"])
	}
	return c.backend.SetValue(value, element)
}

// Routes input to an app, an error JSON without one
func doFocusApp(c *cuCall) (any, error) {
	if !pyrepr.Truthy(c.args["app"]) {
		return cuDumps(map[string]any{"error": "focus_app requires `app`"}, "error"), nil
	}
	app, err := cuOptStr("app", c.args["app"])
	if err != nil {
		return nil, err
	}
	return c.backend.FocusApp(*app, pyrepr.Truthy(c.args["raise_window"]))
}

// Captures the screen in som, vision or ax mode, fenced before the frame is shaped
func doCapture(c *cuCall) (any, error) {
	mode := cuStrOr(c.args, "mode", "som")
	if mode != "som" && mode != "vision" && mode != "ax" {
		return cuDumps(map[string]any{"error": fmt.Sprintf("bad mode %s; use som|vision|ax", pyrepr.Repr(mode))}, "error"), nil
	}
	app, err := cuOptStr("app", c.args["app"])
	if err != nil {
		return nil, err
	}
	pid, err := cuOptInt("pid", c.args["pid"])
	if err != nil {
		return nil, err
	}
	windowID, err := cuOptInt("window_id", c.args["window_id"])
	if err != nil {
		return nil, err
	}
	capture, err := c.backend.Capture(mode, app, pid, windowID)
	if err != nil {
		return nil, err
	}
	if err := c.fence(); err != nil {
		return nil, err
	}
	return captureResponse(c.ctx, c.env, capture, cuDefaultMaxElements, c.sessionID)
}

// Lists apps or windows with their count
func doListing(c *cuCall, key string) (any, error) {
	var items []any
	if c.action == "list_apps" {
		apps, err := c.backend.ListApps()
		if err != nil {
			return nil, err
		}
		items = apps
	} else {
		windows, err := c.backend.ListWindows()
		if err != nil {
			return nil, err
		}
		for _, w := range windows {
			items = append(items, w)
		}
	}
	if items == nil {
		items = []any{}
	}
	return cuDumps(map[string]any{key: items, "count": len(items)}, key, "count"), nil
}

// Runs one action on the backend, a JSON string, a multimodal object or the shaped action result
func dispatchComputerUse(ctx context.Context, env *tools.Env, backend ComputerUseBackend, action string, args map[string]any,
	fence func() error, sessionID *string) (any, error) {
	spec, ok := cuActions[action]
	if !ok {
		msg := "unknown action " + pyrepr.Repr(action)
		if hint := cuActionSuggestions[action]; hint != "" {
			msg += " — did you mean " + pyrepr.Repr(hint) + "? See the action enum in the tool schema."
		}
		return cuDumps(map[string]any{"error": msg}, "error"), nil
	}
	if requested, isStr := args["app"].(string); spec.input && isStr && pystr.Strip(requested) != "" {
		if mismatch := inputTargetMismatch(backend, requested); mismatch != nil {
			want := pyrepr.Repr(pystr.Strip(requested))
			return cuDumps(map[string]any{"ok": false, "action": action, "code": "input_target_mismatch",
				"error": fmt.Sprintf("%s would go to the current target %s, not %s — input actions always hit the sticky "+
					"target from the last capture/focus_app. Call capture(app=%s) or focus_app first, then retry.",
					action, pyrepr.Repr(*mismatch), want, want)}, "ok", "action", "code", "error"), nil
		}
	}
	c := &cuCall{ctx: ctx, env: env, backend: backend, action: action, args: args, fence: fence, sessionID: sessionID}
	if spec.input {
		mode, err := cuOptStr("delivery_mode", args["delivery_mode"])
		if err != nil {
			return nil, err
		}
		c.delivery = DeliveryArgs{DeliveryMode: mode, BringToFront: pyrepr.Truthy(args["bring_to_front"])}
		c.fence, c.sessionID = func() error { return nil }, nil
	}
	res, err := spec.handler(c)
	if err != nil {
		return nil, err
	}
	switch r := res.(type) {
	case string, *pyjson.Object:
		return r, nil
	case *ActionResult:
		return maybeFollowCapture(ctx, env, backend, r, pyrepr.Truthy(args["capture_after"]), fence, sessionID)
	}
	return res, nil
}

// Backend exposing the exact pid and window of its last target
type lastTargetBackend interface{ lastTargetIDs() (pid, windowID *int) }

func (b *CuaDriverBackend) lastTargetIDs() (pid, windowID *int) {
	if b.lastTarget == nil {
		return nil, nil
	}
	p, _ := cuOptInt("pid", b.lastTarget.Vals["pid"])
	w, _ := cuOptInt("window_id", b.lastTarget.Vals["window_id"])
	return p, w
}

// Follow up capture after a successful action with the action payload kept beside the frame
func maybeFollowCapture(ctx context.Context, env *tools.Env, backend ComputerUseBackend, res *ActionResult, doCapture bool,
	fence func() error, sessionID *string) (any, error) {
	if !doCapture || !res.OK {
		return textResponse(res), nil
	}
	var pid, windowID *int
	if t, ok := backend.(lastTargetBackend); ok {
		pid, windowID = t.lastTargetIDs()
	}
	var capture *CaptureResult
	var err error
	if pid != nil && windowID != nil {
		capture, err = backend.Capture(captureAfterMode(env), nil, pid, windowID)
	} else {
		var app *string
		if a, ok := backend.(lastAppBackend); ok {
			app = a.lastAppName()
		}
		capture, err = backend.Capture(captureAfterMode(env), app, nil, nil)
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("follow-up capture failed: %v", err))
		return textResponse(res), nil
	}
	if err := fence(); err != nil {
		return nil, err
	}
	resp, err := captureResponse(ctx, env, capture, cuDefaultMaxElements, sessionID)
	if err != nil {
		return nil, err
	}
	payload := actionPayload(res)
	head, err := pyjson.Dumps(payload)
	if err != nil {
		return nil, err
	}
	if obj, ok := resp.(*pyjson.Object); ok && pyrepr.Truthy(obj.Vals["_multimodal"]) {
		summary := head + "\n\n" + pyrepr.Str(obj.Vals["text_summary"])
		if content, ok := obj.Vals["content"].([]any); ok && len(content) > 0 {
			if first, ok := content[0].(*pyjson.Object); ok {
				first.Set("text", summary)
			}
		}
		obj.Set("text_summary", summary)
		obj.Set("action_result", payload)
		return obj, nil
	}
	text, _ := resp.(string)
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return nil, err
	}
	merged, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("'%s' object is not a mapping", pyrepr.TypeName(decoded))
	}
	for _, k := range payload.Keys {
		merged.Set(k, payload.Vals[k])
	}
	out, err := pyjson.Dumps(merged)
	return out, err
}

// Mode of capture_after follow ups from computer_use.capture_after_mode, som unless som, vision or ax
func captureAfterMode(env *tools.Env) string {
	raw := computerUseCfg(env)["capture_after_mode"]
	mode := "som"
	if v, ok := raw.(string); ok && v != "" {
		mode = v
	} else if raw != nil && pyrepr.Truthy(raw) {
		mode = pyrepr.Str(raw)
	}
	switch mode = strings.ToLower(pystr.Strip(mode)); mode {
	case "som", "vision", "ax":
		return mode
	}
	return "som"
}

// JSON refusal while a human holds the bot desktop
func cuRefused(action string, err error) string {
	return cuDumps(map[string]any{"ok": false, "action": action, "code": "human_has_control", "error": err.Error()},
		"ok", "action", "code", "error")
}

// Runs one computer_use call through hard blocks, approval scopes, the backend and the lease fenced dispatch
func handleComputerUse(ctx context.Context, env *tools.Env, args map[string]any, sessionID string) (any, error) {
	rawAction := args["action"]
	if rawAction != nil && pyrepr.Truthy(rawAction) {
		if _, ok := rawAction.(string); !ok {
			return nil, &AttributeError{fmt.Sprintf("'%s' object has no attribute 'strip'", pyrepr.TypeName(rawAction))}
		}
	}
	action := strings.ToLower(pystr.Strip(cuArgStr(args, "action")))
	if action == "" {
		return cuDumps(map[string]any{"error": "missing `action`"}, "error"), nil
	}
	admitted, err := BotDesktopAssertAgentMayAct(env)
	if err != nil {
		return cuRefused(action, err), nil
	}
	BotDesktopEnsureStartedForTool(env)
	if blocked := rejectUnsafe(action, args); blocked != "" {
		return blocked, nil
	}
	var scopes []string
	if spec, ok := cuActions[action]; ok && spec.destructive {
		scopes = append(scopes, action)
	}
	if pyrepr.Truthy(args["bring_to_front"]) || (action == "focus_app" && pyrepr.Truthy(args["raise_window"])) {
		scopes = append(scopes, "bring_to_front")
	}
	for _, scope := range scopes {
		if refused := requestApproval(ctx, env, scope, args); refused != "" {
			return refused, nil
		}
	}
	backend, release, err := backendForCall(ctx, env, sessionID)
	if err != nil {
		body := map[string]any{"error": "computer_use backend unavailable: " + err.Error()}
		keys := []string{"error"}
		if configuredBackendName(env) == defaultComputerUseBackend {
			body["hint"] = "If the cua-driver binary is missing, run `icebath computer-use install`. If a Python " +
				"dependency is missing, the error above shows the exact install command."
			keys = append(keys, "hint")
		}
		return cuDumps(body, keys...), nil
	}
	defer release()
	if _, err := BotDesktopAssertAgentMayAct(env); err != nil {
		return cuRefused(action, err), nil
	}
	fence := func() error {
		if BotDesktopLeaseEpoch(env) != admitted {
			return &cuHumanHasControl{cuFenceMessage}
		}
		return nil
	}
	var sid *string
	if sessionID != "" {
		sid = &sessionID
	}
	result, err := func() (any, error) {
		if err := fence(); err != nil {
			return nil, err
		}
		result, err := dispatchComputerUse(ctx, env, backend, action, args, fence, sid)
		if err != nil {
			return nil, err
		}
		if err := fence(); err != nil {
			return nil, err
		}
		return result, nil
	}()
	var human *cuHumanHasControl
	switch {
	case errors.As(err, &human):
		return cuRefused(action, err), nil
	case err != nil:
		slog.Error(fmt.Sprintf("computer_use %s failed", action), "error", err)
		return cuDumps(map[string]any{"error": fmt.Sprintf("%s failed: %v", action, err)}, "error"), nil
	}
	return result, nil
}

// Selected provider availability, true for an unregistered name so the call can say why
func checkComputerUseRequirements(env *tools.Env) (bool, error) {
	provider, err := getActiveProvider(env)
	var missing *lookupError
	if errors.As(err, &missing) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return provider.IsAvailable()
}
