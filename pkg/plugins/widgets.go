package plugins

import (
	"context"
	"fmt"
	"strings"
)

// Named view a plugin provides and the terminal UI renders, its state a JSON value the UI holds between calls
type WidgetSpec struct {
	ID     string
	Help   string
	Usage  string
	Mode   string
	Zone   string
	Width  int
	Open   func(ctx context.Context, arg string) (state any, ok bool, err error)
	Input  func(ctx context.Context, state any, in WidgetInput) (next any, open bool, err error)
	Render func(ctx context.Context, frame WidgetFrame) (WidgetView, error)
}

// One key press or text input the UI delivers to a widget
type WidgetInput struct {
	Text  string
	Key   string
	Ctrl  bool
	Alt   bool
	Shift bool
}

// Size and state a widget renders for
type WidgetFrame struct {
	Cols  int
	Rows  int
	State any
}

// Rendered lines of a widget
type WidgetView struct {
	Lines []WidgetLine
}

// One rendered line
type WidgetLine struct {
	Spans []WidgetSpan
}

// Run of text in one theme role and style
type WidgetSpan struct {
	Text      string
	Role      string
	Bold      bool
	Dim       bool
	Italic    bool
	Underline bool
	Inverse   bool
}

// One registered widget with the plugin that provides it
type Widget struct {
	Plugin string
	Spec   WidgetSpec
}

// Widget modes and the zones an ambient widget may dock in
var (
	widgetModes = map[string]bool{"ambient": true, "modal": true}
	widgetZones = map[string]bool{"top-left": true, "top-right": true, "bottom-left": true, "bottom-right": true, "dock-top": true, "dock-bottom": true}
)

// Registers a terminal UI widget under its id, a modal one by default and an ambient one in the top right by default
func (c *Context) RegisterWidget(spec WidgetSpec) (*Registration, error) {
	if c.ignoredAfterAbandonedLoad("register_widget") {
		return nil, nil
	}
	spec.ID = strings.TrimSpace(spec.ID)
	if spec.ID == "" {
		return nil, c.refuse("a widget with an empty id")
	}
	if spec.Render == nil {
		return nil, c.refuse("widget " + spec.ID + " without a render function")
	}
	if spec.Mode == "" {
		spec.Mode = "modal"
	}
	if !widgetModes[spec.Mode] {
		return nil, fmt.Errorf("widget %s mode must be ambient or modal, got %q", spec.ID, spec.Mode)
	}
	if spec.Mode == "ambient" && spec.Zone == "" {
		spec.Zone = "top-right"
	}
	if spec.Zone != "" && !widgetZones[spec.Zone] {
		return nil, fmt.Errorf("widget %s zone %q is not one of top-left, top-right, bottom-left, bottom-right, dock-top, dock-bottom", spec.ID, spec.Zone)
	}
	m := c.manager
	m.mu.RLock()
	existing := m.widgets[spec.ID]
	m.mu.RUnlock()
	if existing != nil {
		return nil, fmt.Errorf("widget %s is already registered by plugin %s", spec.ID, existing.Plugin)
	}
	return registerEntry(c, "widget", spec.ID, m.widgets, &Widget{Plugin: c.manifest.Name, Spec: spec}, nil,
		fmt.Sprintf("Plugin %s registered widget: %s", c.manifest.Name, spec.ID)), nil
}

// Registered widgets of this manager sorted by id
func (m *Manager) Widgets() []Widget {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Widget, 0, len(m.widgets))
	for _, id := range sortedKeys(m.widgets) {
		out = append(out, *m.widgets[id])
	}
	return out
}

// Registered widgets of the profile the context names, sorted by id
func Widgets(ctx context.Context) []Widget { return GetPluginManager(ctx).Widgets() }

// Widget whose functions run in a guest through its wire callbacks
func hostedWidget(a map[string]any) (WidgetSpec, error) {
	width, _ := wireInt(a["width"])
	spec := WidgetSpec{ID: argStr(a["id"]), Help: argStr(a["help"]), Usage: argStr(a["usage"]), Mode: argStr(a["mode"]),
		Zone: argStr(a["zone"]), Width: int(width)}
	render, err := argCallable("register_widget", "render", a["render"])
	if err != nil {
		return spec, err
	}
	spec.Render = func(ctx context.Context, frame WidgetFrame) (WidgetView, error) {
		raw, err := render.Call(ctx, nil, map[string]any{"cols": int64(frame.Cols), "rows": int64(frame.Rows), "state": frame.State})
		if err != nil {
			return WidgetView{}, err
		}
		return widgetViewFrom(raw), nil
	}
	if open, ok := a["open"].(*HostedCallable); ok && open != nil {
		spec.Open = func(ctx context.Context, arg string) (any, bool, error) {
			raw, err := open.Call(ctx, nil, map[string]any{"arg": arg})
			reply := argMap(raw)
			return reply["state"], reply["ok"] == true, err
		}
	}
	if input, ok := a["input"].(*HostedCallable); ok && input != nil {
		spec.Input = func(ctx context.Context, state any, in WidgetInput) (any, bool, error) {
			raw, err := input.Call(ctx, nil, map[string]any{"state": state, "input": map[string]any{"text": in.Text, "key": in.Key,
				"ctrl": in.Ctrl, "alt": in.Alt, "shift": in.Shift}})
			reply := argMap(raw)
			return reply["state"], reply["open"] == true, err
		}
	}
	return spec, nil
}

// View decoded from the lines and spans a guest render function answered
func widgetViewFrom(raw any) WidgetView {
	view := WidgetView{}
	for _, rawLine := range argList(argMap(raw)["lines"]) {
		line := WidgetLine{}
		for _, rawSpan := range argList(argMap(rawLine)["spans"]) {
			s := argMap(rawSpan)
			line.Spans = append(line.Spans, WidgetSpan{Text: argStr(s["text"]), Role: argStr(s["role"]), Bold: s["bold"] == true,
				Dim: s["dim"] == true, Italic: s["italic"] == true, Underline: s["underline"] == true, Inverse: s["inverse"] == true})
		}
		view.Lines = append(view.Lines, line)
	}
	return view
}

func init() {
	hostCtxDispatch["register_widget"] = hostMethod("register_widget", []pyParam{kwReq("id"), kwReq("render"), kwo("help", ""), kwo("usage", ""),
		kwo("mode", ""), kwo("zone", ""), kwo("width", int64(0)), kwo("open", nil), kwo("input", nil)},
		func(_ context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
			spec, err := hostedWidget(a)
			if err != nil {
				return nil, err
			}
			return c.RegisterWidget(spec)
		})
}
