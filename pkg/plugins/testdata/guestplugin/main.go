package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/pluginsdk"
)

var hooks, cancelled atomic.Int64

func object(properties map[string]any) map[string]any {
	return map[string]any{"type": "object", "properties": properties}
}

func schema(name, description string, properties map[string]any) map[string]any {
	return map[string]any{"name": name, "description": description, "parameters": object(properties)}
}

func reply(fields map[string]any) (string, error) {
	data, err := json.Marshal(fields)
	return string(data), err
}

func register(h pluginsdk.Host) error {
	text := map[string]any{"text": map[string]any{"type": "string"}}
	path := map[string]any{"path": map[string]any{"type": "string"}}
	tools := []pluginsdk.Tool{
		{Name: "guest_echo", Schema: schema("guest_echo", "Returns the text in upper case", text),
			Handler: func(_ context.Context, call pluginsdk.ToolCall) (string, error) {
				value, _ := call.Args["text"].(string)
				return reply(map[string]any{"echo": strings.ToUpper(value), "plugin": h.PluginID()})
			}},
		{Name: "guest_relay", Schema: schema("guest_relay", "Calls guest_echo through icebath", text),
			Handler: func(ctx context.Context, call pluginsdk.ToolCall) (string, error) {
				return h.DispatchTool(ctx, "guest_echo", call.Args)
			}},
		{Name: "guest_state", Schema: schema("guest_state", "Reports what the plugin observed", nil),
			Handler: func(context.Context, pluginsdk.ToolCall) (string, error) {
				return reply(map[string]any{"hooks": hooks.Load(), "cancelled": cancelled.Load()})
			}},
		{Name: "guest_wait", Schema: schema("guest_wait", "Waits until the caller gives up", nil),
			Handler: func(ctx context.Context, _ pluginsdk.ToolCall) (string, error) {
				for ctx.Err() == nil {
					time.Sleep(5 * time.Millisecond)
				}
				cancelled.Add(1)
				return "", ctx.Err()
			}},
		{Name: "guest_spin", Schema: schema("guest_spin", "Never returns", nil),
			Handler: func(context.Context, pluginsdk.ToolCall) (string, error) {
				for {
					hooks.Store(hooks.Load())
				}
			}},
		{Name: "guest_note", Schema: schema("guest_note", "Writes a note file and reads it back", text),
			Handler: func(ctx context.Context, call pluginsdk.ToolCall) (string, error) {
				value, _ := call.Args["text"].(string)
				if err := h.WriteFile(ctx, "$PLUGIN_DATA/note.txt", []byte(value)); err != nil {
					return "", err
				}
				data, err := h.ReadFile(ctx, "$PLUGIN_DATA/note.txt")
				return string(data), err
			}},
		{Name: "guest_peek", Schema: schema("guest_peek", "Reads a file", path),
			Handler: func(ctx context.Context, call pluginsdk.ToolCall) (string, error) {
				name, _ := call.Args["path"].(string)
				data, err := h.ReadFile(ctx, name)
				return string(data), err
			}},
	}
	for _, tool := range tools {
		tool.Toolset = "guest"
		if _, err := h.RegisterTool(tool); err != nil {
			return err
		}
	}
	if _, err := h.RegisterHook("post_tool_call", func(context.Context, map[string]any) (any, error) {
		hooks.Add(1)
		return nil, nil
	}, false); err != nil {
		return err
	}
	_, err := h.RegisterWidget(pluginsdk.Widget{ID: "guest-clock", Help: "Shows the state it was opened with", Mode: "ambient", Zone: "top-right",
		Open: func(_ context.Context, arg string) (any, bool, error) { return map[string]any{"label": arg}, true, nil },
		Render: func(_ context.Context, frame pluginsdk.WidgetFrame) (pluginsdk.WidgetView, error) {
			state, _ := frame.State.(map[string]any)
			label, _ := state["label"].(string)
			return pluginsdk.WidgetView{Lines: []pluginsdk.WidgetLine{{Spans: []pluginsdk.WidgetSpan{{Text: label, Role: "accent", Bold: true}}}}}, nil
		}})
	return err
}

func init() { pluginsdk.Register(register) }

func main() { pluginsdk.Main() }
