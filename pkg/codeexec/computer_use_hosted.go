package codeexec

import (
	"context"
	"fmt"
	"sort"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// User installed provider living in the plugin host child, every hook a call on its object
type hostedComputerUseProvider struct {
	ctx         context.Context
	obj         *plugins.HostedObject
	name        string
	displayName string
}

// Wraps a hosted provider object, reading its name and display_name class attributes once
func newHostedComputerUseProvider(ctx context.Context, obj *plugins.HostedObject) (*hostedComputerUseProvider, error) {
	p := &hostedComputerUseProvider{ctx: ctx, obj: obj}
	name, err := obj.Get(ctx, "name")
	if err != nil {
		return nil, err
	}
	p.name = pyrepr.Str(name)
	display, err := obj.Get(ctx, "display_name")
	if err != nil {
		return nil, err
	}
	p.displayName = pyrepr.Str(display)
	if !pyrepr.Truthy(display) {
		p.displayName = p.name
	}
	return p, nil
}

func (p *hostedComputerUseProvider) Name() string        { return p.name }
func (p *hostedComputerUseProvider) DisplayName() string { return p.displayName }

// Picker row the provider builds, a dict answer required
func (p *hostedComputerUseProvider) GetSetupSchema() (map[string]any, error) {
	v, err := p.obj.Call(p.ctx, "get_setup_schema", nil, nil)
	if err != nil {
		return nil, err
	}
	m, ok := hostedMap(v)
	if !ok {
		return nil, fmt.Errorf("computer-use provider %s get_setup_schema() returned %T, not a dict", pyrepr.Repr(p.name), v)
	}
	return m, nil
}

// Backend object the provider creates in the child, driven through hosted calls
func (p *hostedComputerUseProvider) CreateBackend(permissionMode string) (ComputerUseBackend, error) {
	v, err := p.obj.Call(p.ctx, "create_backend", nil, map[string]any{"permission_mode": permissionMode})
	if err != nil {
		return nil, err
	}
	obj, ok := v.(*plugins.HostedObject)
	if !ok || obj == nil {
		return nil, fmt.Errorf("computer-use provider %s create_backend() returned %T, not a ComputerUseBackend", pyrepr.Repr(p.name), v)
	}
	return &hostedComputerUseBackend{ctx: p.ctx, obj: obj}, nil
}

// Truthiness of the provider is_available answer
func (p *hostedComputerUseProvider) IsAvailable() (bool, error) {
	v, err := p.obj.Call(p.ctx, "is_available", nil, nil)
	if err != nil {
		return false, err
	}
	return config.PyTruthy(v), nil
}

// Exit code the provider doctor returns, ok false when it answers None
func (p *hostedComputerUseProvider) Doctor() (int, bool, error) {
	v, err := p.obj.Call(p.ctx, "doctor", nil, nil)
	if err != nil || v == nil {
		return 0, false, err
	}
	code, err := pyIntOf(v)
	if err != nil {
		return 0, false, err
	}
	return code, true, nil
}

// Backend object living in the plugin host child
type hostedComputerUseBackend struct {
	ctx context.Context
	obj *plugins.HostedObject
}

func (b *hostedComputerUseBackend) call(method string, args []any, kwargs map[string]any) (any, error) {
	return b.obj.Call(b.ctx, method, args, kwargs)
}

func (b *hostedComputerUseBackend) action(method string, args []any, kwargs map[string]any) (*ActionResult, error) {
	v, err := b.call(method, args, kwargs)
	if err != nil {
		return nil, err
	}
	return hostedActionResult(method, v)
}

func (b *hostedComputerUseBackend) Start() error {
	_, err := b.call("start", nil, nil)
	return err
}

func (b *hostedComputerUseBackend) Stop() error {
	_, err := b.call("stop", nil, nil)
	return err
}

func (b *hostedComputerUseBackend) IsAvailable() (bool, error) {
	v, err := b.call("is_available", nil, nil)
	return config.PyTruthy(v), err
}

func (b *hostedComputerUseBackend) Capture(mode string, app *string, pid, windowID *int) (*CaptureResult, error) {
	v, err := b.call("capture", nil, map[string]any{"mode": mode, "app": optStr(app), "pid": optInt(pid), "window_id": optInt(windowID)})
	if err != nil {
		return nil, err
	}
	return hostedCaptureResult(v)
}

func (b *hostedComputerUseBackend) Click(a ClickArgs) (*ActionResult, error) {
	return b.action("click", nil, map[string]any{"element": optInt(a.Element), "x": optInt(a.X), "y": optInt(a.Y), "button": a.Button,
		"click_count": a.ClickCount, "modifiers": optStrs(a.Modifiers), "delivery_mode": optStr(a.DeliveryMode), "bring_to_front": a.BringToFront})
}

func (b *hostedComputerUseBackend) Drag(a DragArgs) (*ActionResult, error) {
	return b.action("drag", nil, map[string]any{"from_element": optInt(a.FromElement), "to_element": optInt(a.ToElement),
		"from_xy": optXY(a.FromXY), "to_xy": optXY(a.ToXY), "button": a.Button, "modifiers": optStrs(a.Modifiers),
		"delivery_mode": optStr(a.DeliveryMode), "bring_to_front": a.BringToFront})
}

func (b *hostedComputerUseBackend) Scroll(a ScrollArgs) (*ActionResult, error) {
	return b.action("scroll", nil, map[string]any{"direction": a.Direction, "amount": a.Amount, "element": optInt(a.Element),
		"x": optInt(a.X), "y": optInt(a.Y), "modifiers": optStrs(a.Modifiers), "delivery_mode": optStr(a.DeliveryMode), "bring_to_front": a.BringToFront})
}

func (b *hostedComputerUseBackend) TypeText(text string, d DeliveryArgs) (*ActionResult, error) {
	return b.action("type_text", []any{text}, map[string]any{"delivery_mode": optStr(d.DeliveryMode), "bring_to_front": d.BringToFront})
}

func (b *hostedComputerUseBackend) Key(keys string, d DeliveryArgs) (*ActionResult, error) {
	return b.action("key", []any{keys}, map[string]any{"delivery_mode": optStr(d.DeliveryMode), "bring_to_front": d.BringToFront})
}

func (b *hostedComputerUseBackend) ListApps() ([]any, error) {
	v, err := b.call("list_apps", nil, nil)
	if err != nil {
		return nil, err
	}
	rows, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("computer-use backend list_apps() returned %T, not a list", v)
	}
	for i, row := range rows {
		if m, isMap := hostedMap(row); isMap {
			rows[i] = hostedObject(m)
		}
	}
	return rows, nil
}

func (b *hostedComputerUseBackend) ListWindows() ([]*pyjson.Object, error) {
	v, err := b.call("list_windows", nil, nil)
	if err != nil {
		return nil, err
	}
	rows, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("computer-use backend list_windows() returned %T, not a list", v)
	}
	out := make([]*pyjson.Object, 0, len(rows))
	for _, row := range rows {
		m, isMap := hostedMap(row)
		if !isMap {
			return nil, fmt.Errorf("computer-use backend list_windows() row is %T, not a dict", row)
		}
		out = append(out, hostedObject(m))
	}
	return out, nil
}

func (b *hostedComputerUseBackend) FocusApp(app string, raiseWindow bool) (*ActionResult, error) {
	return b.action("focus_app", []any{app}, map[string]any{"raise_window": raiseWindow})
}

func (b *hostedComputerUseBackend) SetValue(value string, element *int) (*ActionResult, error) {
	return b.action("set_value", []any{value}, map[string]any{"element": optInt(element)})
}

func (b *hostedComputerUseBackend) Wait(seconds float64) (*ActionResult, error) {
	return b.action("wait", []any{seconds}, nil)
}

func optInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func optStr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func optStrs(v []string) any {
	if v == nil {
		return nil
	}
	out := make([]any, len(v))
	for i, s := range v {
		out[i] = s
	}
	return out
}

func optXY(p *[2]int) any {
	if p == nil {
		return nil
	}
	return []any{p[0], p[1]}
}

// Dict or dataclass record decoded from the plugin host wire
func hostedMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case plugins.Record:
		return map[string]any(m), true
	}
	return nil, false
}

// Ordered object over a decoded dict, keys sorted since the wire map carries no order
func hostedObject(m map[string]any) *pyjson.Object {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	o := pyjson.NewObject()
	for _, k := range keys {
		o.Set(k, m[k])
	}
	return o
}

func hostedOptStr(v any) *string {
	if v == nil {
		return nil
	}
	s := pyrepr.Str(v)
	return &s
}

func hostedOptBool(v any) *bool {
	if v == nil {
		return nil
	}
	b := config.PyTruthy(v)
	return &b
}

func hostedInt(v any) int {
	if v == nil {
		return 0
	}
	n, _ := pyIntOf(v)
	return n
}

func hostedStr(v any) string {
	if v == nil {
		return ""
	}
	return pyrepr.Str(v)
}

// CaptureResult rebuilt from the dataclass fields the child sent
func hostedCaptureResult(v any) (*CaptureResult, error) {
	m, ok := hostedMap(v)
	if !ok {
		return nil, fmt.Errorf("computer-use backend capture() returned %T, not a CaptureResult", v)
	}
	c := &CaptureResult{Mode: hostedStr(m["mode"]), Width: hostedInt(m["width"]), Height: hostedInt(m["height"]),
		PNGB64: hostedOptStr(m["png_b64"]), App: hostedStr(m["app"]), WindowTitle: hostedStr(m["window_title"]),
		PNGBytesLen: hostedInt(m["png_bytes_len"]), ImageMimeType: hostedOptStr(m["image_mime_type"]), Note: hostedStr(m["note"]),
		AXMaxElements: hostedInt(m["ax_max_elements"])}
	rows, _ := m["elements"].([]any)
	for _, row := range rows {
		e, isMap := hostedMap(row)
		if !isMap {
			return nil, fmt.Errorf("computer-use backend capture() element is %T, not a UIElement", row)
		}
		el := UIElement{Index: hostedInt(e["index"]), Role: hostedStr(e["role"]), Label: hostedStr(e["label"]), App: hostedStr(e["app"]),
			PID: hostedInt(e["pid"]), WindowID: hostedInt(e["window_id"]), ElementToken: hostedOptStr(e["element_token"])}
		if bounds, isList := e["bounds"].([]any); isList && len(bounds) == 4 {
			for i := range bounds {
				el.Bounds[i] = hostedInt(bounds[i])
			}
		}
		el.Attributes, _ = hostedMap(e["attributes"])
		if el.Attributes == nil {
			el.Attributes = map[string]any{}
		}
		c.Elements = append(c.Elements, el)
	}
	return c, nil
}

// ActionResult rebuilt from the dataclass fields the child sent
func hostedActionResult(method string, v any) (*ActionResult, error) {
	m, ok := hostedMap(v)
	if !ok {
		return nil, fmt.Errorf("computer-use backend %s() returned %T, not an ActionResult", method, v)
	}
	r := &ActionResult{OK: config.PyTruthy(m["ok"]), Action: hostedStr(m["action"]), Message: hostedStr(m["message"]),
		Verified: hostedOptBool(m["verified"]), Effect: hostedOptStr(m["effect"]), Path: hostedOptStr(m["path"]),
		Degraded: hostedOptBool(m["degraded"]), DeliveryMode: hostedOptStr(m["delivery_mode"]), Code: hostedOptStr(m["code"])}
	if m["capture"] != nil {
		c, err := hostedCaptureResult(m["capture"])
		if err != nil {
			return nil, err
		}
		r.Capture = c
	}
	meta, _ := hostedMap(m["meta"])
	r.Meta = hostedObject(meta)
	if esc, isMap := hostedMap(m["escalation"]); isMap {
		r.Escalation = hostedObject(esc)
	}
	return r, nil
}
