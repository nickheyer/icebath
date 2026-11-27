package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/botdesktop"
	"github.com/nickheyer/icebath/internal/config"
	cuconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/computeruse/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/computeruse/cua"
	"github.com/nickheyer/icebath/internal/tools"
)

// Element cap of a capture's result
const maxElements = 100

// What an action is: how it runs, whether it is input the sticky target receives, and whether it asks approval
type spec struct {
	run         func(c *call) (any, error)
	input       bool
	destructive bool
}

// The actions there are, by name
var actions = func() map[string]spec {
	click := func(button string, count int) func(c *call) (any, error) {
		return func(c *call) (any, error) { return c.click(button, count) }
	}
	in := func(run func(c *call) (any, error)) spec { return spec{run: run, input: true, destructive: true} }
	return map[string]spec{
		"click": in(click("", 1)), "double_click": in(click("", 2)), "right_click": in(click("right", 1)), "middle_click": in(click("middle", 1)),
		"drag": in((*call).drag), "scroll": in((*call).scroll),
		"type":      in(func(c *call) (any, error) { return c.backend.Type(c.ctx, text(c.args, "text", ""), c.delivery), nil }),
		"key":       in(func(c *call) (any, error) { return c.backend.Key(c.ctx, text(c.args, "keys", ""), c.delivery), nil }),
		"set_value": in((*call).setValue),
		"focus_app": {run: (*call).focusApp, destructive: true},
		"capture":   {run: (*call).capture},
		"wait": {run: func(c *call) (any, error) {
			seconds, err := number(arg(c.args, "seconds", 1.0))
			if err != nil {
				return nil, err
			}
			return tools.JSON(payload(c.backend.Wait(c.ctx, seconds))), nil
		}},
		"list_apps":    {run: func(c *call) (any, error) { return c.listing("apps") }},
		"list_windows": {run: func(c *call) (any, error) { return c.listing("windows") }},
	}
}()

// Real action to suggest for a name the model guessed, never taken as an alias
var suggestions = map[string]string{"hotkey": "key", "press_key": "key", "keypress": "key", "key_combo": "key", "shortcut": "key", "type_text": "type",
	"input_text": "type", "screenshot": "capture", "get_window_state": "capture", "left_click": "click", "mouse_click": "click"}

// Key combinations that are refused whatever the approvals say, and the aliases folded before they are compared
var (
	blockedCombos = [][]string{{"backspace", "cmd", "shift"}, {"backspace", "cmd", "option"}, {"cmd", "ctrl", "q"}, {"cmd", "q", "shift"},
		{"cmd", "option", "q", "shift"}, {"l", "win"}, {"ctrl", "delete", "option"}, {"ctrl", "del", "option"}, {"f4", "option"}}
	keyAliases = map[string]string{"command": "cmd", "control": "ctrl", "alt": "option", "⌘": "cmd", "⌥": "option", "windows": "win", "super": "win",
		"meta": "win"}
	keySplit = regexp.MustCompile(`\s*[+\-]\s*`)
	// Shell patterns no text typed may hold
	blockedTyped = []string{`curl\s+[^|]*\|\s*bash`, `curl\s+[^|]*\|\s*sh`, `wget\s+[^|]*\|\s*bash`, `\bsudo\s+rm\s+-[rf]`, `\brm\s+-rf\s+/\s*$`,
		`:\s*\(\)\s*\{\s*:\|:\s*&\s*\}`}
	blockedTypedRe = func() []*regexp.Regexp {
		var out []*regexp.Regexp
		for _, p := range blockedTyped {
			out = append(out, regexp.MustCompile(`(?i)`+p))
		}
		return out
	}()
)

func arg(args map[string]any, key string, def any) any {
	if v, ok := args[key]; ok {
		return v
	}
	return def
}

// Text of an argument, the default when it is absent
func text(args map[string]any, key, def string) string {
	v, ok := args[key]
	switch {
	case !ok:
		return def
	case v == nil:
		return ""
	}
	if s, isText := v.(string); isText {
		return s
	}
	return show(v)
}

// Text of a value as Python prints it, for what the model reads back of its own arguments
func show(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		return "'" + t + "'"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case float64:
		if t == math.Trunc(t) && !math.IsInf(t, 0) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	case []any:
		parts := make([]string, len(t))
		for i, item := range t {
			parts[i] = show(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return fmt.Sprint(v)
}

// Error result of a refused call, its fields in the order given
func refusal(pairs ...any) string {
	out := map[string]any{}
	for i := 0; i+1 < len(pairs); i += 2 {
		out[pairs[i].(string)] = pairs[i+1]
	}
	return tools.JSON(out)
}

// Refusal of input no approval may let through: shell patterns typed, destructive shortcuts, bring_to_front without foreground delivery
func unsafe(action string, args map[string]any) string {
	if action == "type" {
		typed := text(args, "text", "")
		for i, p := range blockedTypedRe {
			if p.MatchString(typed) {
				return refusal("error", "blocked pattern in type text: '"+blockedTyped[i]+"'", "hint", "Dangerous shell patterns cannot be typed via computer_use.")
			}
		}
	}
	if action == "key" {
		combo := map[string]bool{}
		for _, part := range keySplit.Split(text(args, "keys", ""), -1) {
			part = strings.ToLower(strings.TrimSpace(part))
			if alias, ok := keyAliases[part]; ok {
				part = alias
			}
			if part != "" {
				combo[part] = true
			}
		}
		for _, blocked := range blockedCombos {
			if !slices.ContainsFunc(blocked, func(k string) bool { return !combo[k] }) {
				return refusal("error", "blocked key combo: ['"+strings.Join(blocked, "', '")+"']", "hint", "Destructive system shortcuts are hard-blocked.")
			}
		}
	}
	if config.Truthy(args["bring_to_front"]) && args["delivery_mode"] != "foreground" {
		return refusal("error", "bring_to_front requires delivery_mode='foreground'", "code", "bring_to_front_requires_foreground")
	}
	return ""
}

const foregroundSuffix = " [FOREGROUND — briefly raises the window / changes focus]"

// One line of what the action does, as the approval prompt shows it
func summarize(action string, args map[string]any) string {
	fg := ""
	if args["delivery_mode"] == "foreground" {
		fg = foregroundSuffix
	}
	switch action {
	case "click", "double_click", "right_click", "middle_click":
		where := ""
		if el, ok := args["element"]; ok && el != nil {
			where = " element #" + strings.Trim(show(el), "'")
		} else if coord := args["coordinate"]; config.Truthy(coord) {
			where = " at " + show(coord)
		}
		return action + where + fg
	case "drag":
		return "drag " + endpoint(args, "from") + " → " + endpoint(args, "to") + fg
	case "scroll":
		return "scroll " + strings.Trim(show(arg(args, "direction", "?")), "'") + " x" + strings.Trim(show(arg(args, "amount", 3.0)), "'") + fg
	case "type":
		typed, more := []rune(text(args, "text", "")), ""
		if len(typed) > 60 {
			typed, more = typed[:60], "..."
		}
		return "type '" + string(typed) + "'" + more + fg
	case "key":
		return "key " + show(arg(args, "keys", "")) + fg
	case "focus_app":
		raise := ""
		if config.Truthy(args["raise_window"]) {
			raise = " (raise)"
		}
		return "focus " + show(arg(args, "app", "")) + raise
	}
	return action + fg
}

// Drag end of a prompt: the element when one is set, else the point, ? with neither
func endpoint(args map[string]any, side string) string {
	end := args[side+"_element"]
	if !config.Truthy(end) {
		end = args[side+"_coordinate"]
	}
	if end == nil {
		return "?"
	}
	return strings.Trim(show(end), "'")
}

// Asks for the action in its scope and delivery, nil when it may go ahead, else the refusal the model reads
func approve(ctx context.Context, env *tools.Env, scope string, args map[string]any) string {
	if env.Gate == nil {
		return ""
	}
	mode := "background"
	if args["delivery_mode"] == "foreground" {
		mode = "foreground"
	}
	err := env.Gate(ctx, tools.Gate{Key: "cua:" + scope + ":" + mode, Description: "Allow computer_use to perform `" + scope + "`?",
		Target: "computer_use: " + summarize(scope, args), Subject: "computer_use `" + scope + "` requires approval", Noun: "desktop actions",
		Advice:     "Find an alternative approach that avoids driving the desktop.",
		Unanswered: "BLOCKED: computer_use `" + scope + "` requires approval but no interactive user or gateway is present to approve it."})
	if err == nil {
		return ""
	}
	message := err.Error()
	if r, ok := errors.AsType[*tools.Refusal](err); ok && r.Message != "" {
		message = r.Message
	}
	if message == "" {
		message = "denied by user"
	}
	return refusal("error", message, "action", scope)
}

// Runs one computer_use call through the hard blocks, the approval of each scope it touches, and the session's backend
func (r *registrant) handle(ctx context.Context, tc *tools.Call) string {
	args := tc.Args
	if raw, ok := args["action"]; ok && config.Truthy(raw) {
		if _, isText := raw.(string); !isText {
			return tools.ErrorResult(fmt.Errorf("action is %T where text was expected", raw))
		}
	}
	action := strings.ToLower(strings.TrimSpace(text(args, "action", "")))
	if action == "" {
		return refusal("error", "missing `action`")
	}
	cfg, desktop := tc.Env.Config, r.driver.Desktop
	admitted, err := desktop.Admit(cfg)
	if err != nil {
		return taken(action, err)
	}
	desktop.Ready(ctx, cfg)
	// Input on the agent's own screen steals no focus of the user's, so it is delivered in the foreground unless the call says otherwise
	if s, ok := actions[action]; ok && s.input && args["delivery_mode"] == nil && r.onScreen(ctx, cfg) {
		args["delivery_mode"] = "foreground"
	}
	if err := cua.Unknown(cfg); err != nil {
		return refusal("error", "computer_use backend unavailable: "+err.Error())
	}
	if blocked := unsafe(action, args); blocked != "" {
		return blocked
	}
	var scopes []string
	if s, ok := actions[action]; ok && s.destructive {
		scopes = append(scopes, action)
	}
	if config.Truthy(args["bring_to_front"]) || action == "focus_app" && config.Truthy(args["raise_window"]) {
		scopes = append(scopes, "bring_to_front")
	}
	for _, scope := range scopes {
		if refused := approve(ctx, tc.Env, scope, args); refused != "" {
			return refused
		}
	}
	h, err := r.backend(ctx, tc.Env.Config, tc.SessionID)
	if err != nil {
		return refusal("error", "computer_use backend unavailable: "+err.Error(), "hint", "If the cua-driver binary is missing, run `icebath "+
			"computer-use install`. If a Python dependency is missing, the error above shows the exact install command.")
	}
	defer h.call.Unlock()
	if _, err := desktop.Admit(cfg); err != nil {
		return taken(action, err)
	}
	// A person who took the screen over while the action ran voids its result
	fence := func() error {
		if desktop.Epoch(cfg) != admitted {
			return &botdesktop.HumanHasControl{Message: fenceMessage}
		}
		return nil
	}
	if err := fence(); err != nil {
		return taken(action, err)
	}
	out, err := r.dispatch(ctx, tc, h.backend, action, args, fence)
	if err == nil {
		err = fence()
	}
	if _, human := errors.AsType[*botdesktop.HumanHasControl](err); human {
		return taken(action, err)
	}
	if err != nil {
		slog.Error(fmt.Sprintf("computer_use %s failed", action), "error", err)
		return refusal("error", fmt.Sprintf("%s failed: %v", action, err))
	}
	return out
}

const fenceMessage = "A human took over this desktop while the action ran; its result was discarded. Tell the user what you need and re-capture " +
	"once they hand back."

// Refusal of an action while a person holds the agent's screen
func taken(action string, err error) string {
	return refusal("ok", false, "action", action, "code", "human_has_control", "error", err.Error())
}

// Whether the driver acts on the agent's own screen: one in the terminal backend's sandbox, or one running on this machine
func (r *registrant) onScreen(ctx context.Context, cfg *config.Config) bool {
	if r.driver.Desktop.InSandbox(cfg) {
		return true
	}
	running, _ := r.driver.Desktop.Running(ctx, cfg)
	return running
}

// One action on the backend with what its arguments say
type call struct {
	ctx      context.Context
	r        *registrant
	tc       *tools.Call
	backend  *cua.Backend
	action   string
	args     map[string]any
	delivery cua.Delivery
	// Fails once the screen changed hands since the action was admitted, checked before a capture is shaped for the model
	fence func() error
}

// Runs the action, its follow-up capture when it asks for one
func (r *registrant) dispatch(ctx context.Context, tc *tools.Call, backend *cua.Backend, action string, args map[string]any, fence func() error) (string, error) {
	s, ok := actions[action]
	if !ok {
		message := "unknown action '" + action + "'"
		if hint := suggestions[action]; hint != "" {
			message += " — did you mean '" + hint + "'? See the action enum in the tool schema."
		}
		return refusal("error", message), nil
	}
	if requested, isText := args["app"].(string); s.input && isText && strings.TrimSpace(requested) != "" {
		current, wanted := strings.ToLower(strings.TrimSpace(backend.App())), strings.ToLower(strings.TrimSpace(requested))
		if current != "" && !strings.Contains(current, wanted) && !strings.Contains(wanted, current) {
			shown := "'" + strings.TrimSpace(requested) + "'"
			return refusal("ok", false, "action", action, "code", "input_target_mismatch", "error", fmt.Sprintf("%s would go to the current target '%s', "+
				"not %s — input actions always hit the sticky target from the last capture/focus_app. Call capture(app=%s) or focus_app first, then retry.",
				action, backend.App(), shown, shown)), nil
		}
	}
	c := &call{ctx: ctx, r: r, tc: tc, backend: backend, action: action, args: args, fence: fence}
	if s.input {
		mode, isText := args["delivery_mode"].(string)
		if raw := args["delivery_mode"]; raw != nil && !isText {
			return "", fmt.Errorf("delivery_mode must be a string, got %s", show(raw))
		}
		c.delivery = cua.Delivery{Mode: mode, Front: config.Truthy(args["bring_to_front"])}
	}
	res, err := s.run(c)
	if err != nil {
		return "", err
	}
	switch v := res.(type) {
	case string:
		return v, nil
	case *cua.Action:
		return c.follow(v)
	}
	return "", fmt.Errorf("computer_use %s returned %T", action, res)
}

// Optional whole number argument
func whole(key string, v any) (*int, error) {
	if v == nil {
		return nil, nil
	}
	switch n := v.(type) {
	case bool:
		i := 0
		if n {
			i = 1
		}
		return &i, nil
	case float64:
		if n == math.Trunc(n) && !math.IsInf(n, 0) {
			i := int(n)
			return &i, nil
		}
	}
	return nil, fmt.Errorf("%s must be an integer, got %s", key, show(v))
}

func number(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case bool:
		if n {
			return 1, nil
		}
		return 0, nil
	case string:
		f, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(n), "_", ""), 64)
		if err != nil {
			return 0, fmt.Errorf("%s is not a number", n)
		}
		return f, nil
	}
	return 0, fmt.Errorf("expected a number, got %T", v)
}

// Modifier keys, nil when absent
func modifiers(args map[string]any) ([]string, error) {
	raw, ok := args["modifiers"]
	if !ok || raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("modifiers must be a list of strings, got %s", show(raw))
	}
	out := make([]string, 0, len(list))
	for _, m := range list {
		s, ok := m.(string)
		if !ok {
			return nil, fmt.Errorf("modifiers must be a list of strings, got %s", show(raw))
		}
		out = append(out, s)
	}
	return out, nil
}

func coordinate(args map[string]any, key string) []any {
	c, _ := args[key].([]any)
	return c
}

// Point of a click or scroll
func (c *call) point() (*int, *int, error) {
	coord := coordinate(c.args, "coordinate")
	var x, y any
	if len(coord) > 0 {
		x = coord[0]
	}
	if len(coord) > 1 {
		y = coord[1]
	}
	px, err := whole("coordinate", x)
	if err != nil {
		return nil, nil, err
	}
	py, err := whole("coordinate", y)
	return px, py, err
}

func (c *call) click(button string, count int) (any, error) {
	element, err := whole("element", c.args["element"])
	if err != nil {
		return nil, err
	}
	x, y, err := c.point()
	if err != nil {
		return nil, err
	}
	mods, err := modifiers(c.args)
	if err != nil {
		return nil, err
	}
	if button == "" {
		button = "left"
		if b := c.args["button"]; config.Truthy(b) {
			button = strings.Trim(show(b), "'")
		}
	}
	return c.backend.Click(c.ctx, cua.Click{Element: element, X: x, Y: y, Button: button, Count: count, Modifiers: mods, Delivery: c.delivery}), nil
}

// Pair a drag end unpacks into
func pair(key string, coord []any) (*[2]int, error) {
	switch {
	case len(coord) == 0:
		return nil, nil
	case len(coord) > 2:
		return nil, errors.New("too many values to unpack (expected 2)")
	case len(coord) < 2:
		return nil, fmt.Errorf("not enough values to unpack (expected 2, got %d)", len(coord))
	}
	x, errX := whole(key, coord[0])
	y, errY := whole(key, coord[1])
	if errX != nil || errY != nil || x == nil || y == nil {
		return nil, fmt.Errorf("%s must hold two integers, got %s", key, show(coord))
	}
	return &[2]int{*x, *y}, nil
}

func (c *call) drag() (any, error) {
	from, to := coordinate(c.args, "from_coordinate"), coordinate(c.args, "to_coordinate")
	if (c.args["from_element"] == nil || c.args["to_element"] == nil) && !(len(from) > 0 && len(to) > 0) {
		return refusal("error", "drag requires from_coordinate/to_coordinate or from_element/to_element"), nil
	}
	fromElement, err := whole("from_element", c.args["from_element"])
	if err != nil {
		return nil, err
	}
	toElement, err := whole("to_element", c.args["to_element"])
	if err != nil {
		return nil, err
	}
	fromXY, err := pair("from_coordinate", from)
	if err != nil {
		return nil, err
	}
	toXY, err := pair("to_coordinate", to)
	if err != nil {
		return nil, err
	}
	mods, err := modifiers(c.args)
	if err != nil {
		return nil, err
	}
	return c.backend.Drag(c.ctx, cua.Drag{From: fromElement, To: toElement, FromXY: fromXY, ToXY: toXY, Button: text(c.args, "button", "left"),
		Modifiers: mods, Delivery: c.delivery}), nil
}

func (c *call) scroll() (any, error) {
	amount, err := number(arg(c.args, "amount", 3.0))
	if err != nil {
		return nil, err
	}
	element, err := whole("element", c.args["element"])
	if err != nil {
		return nil, err
	}
	x, y, err := c.point()
	if err != nil {
		return nil, err
	}
	mods, err := modifiers(c.args)
	if err != nil {
		return nil, err
	}
	return c.backend.Scroll(c.ctx, cua.Scroll{Direction: text(c.args, "direction", "down"), Amount: int(amount), Element: element, X: x, Y: y,
		Modifiers: mods, Delivery: c.delivery}), nil
}

func (c *call) setValue() (any, error) {
	if c.args["value"] == nil {
		return refusal("error", "set_value requires `value`"), nil
	}
	element, err := whole("element", c.args["element"])
	if err != nil {
		return nil, err
	}
	return c.backend.SetValue(c.ctx, text(c.args, "value", ""), element), nil
}

func (c *call) focusApp() (any, error) {
	app, isText := c.args["app"].(string)
	if !config.Truthy(c.args["app"]) {
		return refusal("error", "focus_app requires `app`"), nil
	}
	if !isText {
		return nil, fmt.Errorf("app must be a string, got %s", show(c.args["app"]))
	}
	return c.backend.FocusApp(c.ctx, app, config.Truthy(c.args["raise_window"]))
}

func (c *call) capture() (any, error) {
	mode := text(c.args, "mode", "som")
	if mode != "som" && mode != "vision" && mode != "ax" {
		return refusal("error", "bad mode '"+mode+"'; use som|vision|ax"), nil
	}
	app, isText := c.args["app"].(string)
	if raw := c.args["app"]; raw != nil && !isText {
		return nil, fmt.Errorf("app must be a string, got %s", show(raw))
	}
	pid, err := whole("pid", c.args["pid"])
	if err != nil {
		return nil, err
	}
	window, err := whole("window_id", c.args["window_id"])
	if err != nil {
		return nil, err
	}
	shot, err := c.backend.Capture(c.ctx, mode, app, pid, window)
	if err != nil {
		return nil, err
	}
	if err := c.fence(); err != nil {
		return nil, err
	}
	return c.r.respond(c.ctx, c.tc, shot, maxElements).render(c.tc), nil
}

// Apps or windows with how many there are
func (c *call) listing(key string) (any, error) {
	var items []any
	if key == "apps" {
		apps, err := c.backend.ListApps(c.ctx)
		if err != nil {
			return nil, err
		}
		items = apps
	} else {
		windows, err := c.backend.ListWindows(c.ctx)
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
	return tools.JSON(map[string]any{key: items, "count": len(items)}), nil
}

// Next rung of the ladder from what the action proved, in order of precedence
func ladder(a *cua.Action) map[string]any {
	effect := ""
	if a.Effect != nil {
		effect = *a.Effect
	}
	switch {
	case effect == "confirmed" || a.Verified != nil && *a.Verified:
		return map[string]any{"decision": "done"}
	case effect == "unverifiable":
		return map[string]any{"decision": "verify_fresh_state", "hint": "Input was delivered but not confirmed. Re-capture and check the result BEFORE " +
			"any retry — do not repeat the input on an escalation recommendation alone."}
	case effect == "suspected_noop" || !a.OK || a.Code != nil:
		delivered, okD := a.Meta["delivered_chars"].(float64)
		requested, okR := a.Meta["requested_chars"].(float64)
		if (a.Action == "type" || a.Action == "type_text") && a.Code != nil && *a.Code == "type_text_incomplete" && okD && okR && requested > 0 && delivered <= 0 {
			return map[string]any{"decision": "escalate", "recommended": "set_value", "hint": "0 characters landed: this input drops synthetic " +
				"keystrokes, so no delivery rung will fix it. Climb to the set_value action on the field's element index instead — it is an action, not " +
				"a delivery mode, and sets the value through the accessibility API, bypassing event filtering. Re-capture first if the index is stale."}
		}
		out := map[string]any{"decision": "escalate", "hint": "The input likely did not land. Climb one rung following `recommended`: 'px' → re-issue " +
			"by coordinate; 'foreground' (or a failed pixel click) → re-issue with delivery_mode='foreground' (separate approval). Do not predict the " +
			"rung from the app being Electron/Chromium — react to this signal."}
		if a.Escalation != nil {
			out["recommended"] = a.Escalation["recommended"]
		}
		return out
	}
	return map[string]any{"decision": "verify_fresh_state", "hint": "Transport succeeded but the effect is unproven. Re-capture and confirm before continuing."}
}

// What the model reads of an action: whether it went through, the driver's verdict and the next rung
func payload(a *cua.Action) map[string]any {
	out := map[string]any{"ok": a.OK, "action": a.Action, "verdict": ladder(a)}
	if a.Message != "" {
		out["message"] = a.Message
	}
	for key, v := range map[string]any{"verified": a.Verified, "degraded": a.Degraded} {
		if b, ok := v.(*bool); ok && b != nil {
			out[key] = *b
		}
	}
	for key, v := range map[string]*string{"effect": a.Effect, "path": a.Path, "delivery_mode": a.Delivery, "code": a.Code} {
		if v != nil {
			out[key] = *v
		}
	}
	if a.Escalation != nil {
		out["escalation"] = a.Escalation
	}
	if len(a.Meta) > 0 {
		out["meta"] = a.Meta
	}
	return out
}

// The action's result, with a capture after it when the call asked and the action went through
func (c *call) follow(a *cua.Action) (string, error) {
	if !config.Truthy(c.args["capture_after"]) || !a.OK {
		return tools.JSON(payload(a)), nil
	}
	mode := c.tc.Env.Config.GetString(cuconfig.CaptureAfterMode)
	var shot *cua.Capture
	var err error
	if pid, window := c.backend.Target(); pid != nil && window != nil {
		shot, err = c.backend.Capture(c.ctx, mode, "", pid, window)
	} else {
		shot, err = c.backend.Capture(c.ctx, mode, c.backend.App(), nil, nil)
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("follow-up capture failed: %v", err))
		return tools.JSON(payload(a)), nil
	}
	if err := c.fence(); err != nil {
		return "", err
	}
	view := c.r.respond(c.ctx, c.tc, shot, maxElements)
	result := payload(a)
	if view.media != nil {
		view.summary = tools.JSON(result) + "\n\n" + view.summary
		return view.render(c.tc), nil
	}
	for k, v := range result {
		view.fields[k] = v
	}
	return view.render(c.tc), nil
}
