package main

import (
	"context"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tui"
)

// Widgets the plugins of the profile registered, in the shape the terminal UI shows, the plugins loaded on the first ask
func pluginWidgets(ctx context.Context) []tui.Widget {
	plugins.DiscoverPlugins(ctx, false)
	registered := plugins.Widgets(ctx)
	out := make([]tui.Widget, 0, len(registered))
	for _, w := range registered {
		spec := w.Spec
		widget := tui.Widget{ID: spec.ID, Help: spec.Help, Usage: spec.Usage, Mode: spec.Mode, Zone: spec.Zone, Width: spec.Width, Open: spec.Open,
			Every: spec.Refresh, Tick: spec.Tick}
		if spec.Input != nil {
			widget.Input = func(ctx context.Context, state any, in tui.WidgetInput) (any, bool, error) {
				return spec.Input(ctx, state, plugins.WidgetInput{Text: in.Text, Key: in.Key, Ctrl: in.Ctrl, Alt: in.Alt, Shift: in.Shift})
			}
		}
		widget.Render = func(ctx context.Context, frame tui.WidgetFrame) (tui.WidgetView, error) {
			view, err := spec.Render(ctx, plugins.WidgetFrame{Cols: frame.Cols, Rows: frame.Rows, State: frame.State})
			if err != nil {
				return tui.WidgetView{}, err
			}
			lines := make([]tui.WidgetLine, len(view.Lines))
			for i, line := range view.Lines {
				spans := make([]tui.WidgetSpan, len(line.Spans))
				for j, s := range line.Spans {
					spans[j] = tui.WidgetSpan{Text: s.Text, Role: s.Role, Bold: s.Bold, Dim: s.Dim, Italic: s.Italic, Underline: s.Underline, Inverse: s.Inverse}
				}
				lines[i] = tui.WidgetLine{Spans: spans}
			}
			return tui.WidgetView{Lines: lines}, nil
		}
		out = append(out, widget)
	}
	return out
}

// Widgets of the plugins after a scan of the plugins, forced anew when asked and else run only the first time, with the plugins that failed to load
func scanPluginWidgets(ctx context.Context, force bool) ([]tui.Widget, []tui.WidgetLoadFailure) {
	plugins.DiscoverPlugins(ctx, force)
	var failed []tui.WidgetLoadFailure
	for _, failure := range plugins.GetPluginManager(ctx).LoadFailures() {
		failed = append(failed, tui.WidgetLoadFailure{Plugin: failure[0], Message: failure[1]})
	}
	return pluginWidgets(ctx), failed
}
