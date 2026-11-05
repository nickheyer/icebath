package tools

import (
	"context"
	"fmt"
)

// ReadTerminalPane reads the desktop in-app terminal through the gateway bridge, installed by the desktop gateway
var ReadTerminalPane func(env *Env, window map[string]int) (string, error)

// Desktop answers with a JSON object, passed through, else the raw text wrapped
func termPassthroughJSON(raw string) string {
	if v, err := jsonDecodeWTF8([]byte(raw)); err == nil {
		return JSON(v)
	}
	return JSON(map[string]any{"text": raw})
}

// paneWindow is one optional integer argument of a pane read with its floor
type paneWindow struct {
	key   string
	value any
	floor int
}

// Shared body of the read_terminal, read_preview and read_window bridges
func readPane(env *Env, callback func(env *Env, window map[string]int) (string, error), window []paneWindow, errors [4]string) string {
	if callback == nil {
		return toolErrorJSON(errors[0], nil)
	}
	kwargs := map[string]int{}
	for _, w := range window {
		if w.value == nil {
			continue
		}
		n, ok := argInt(w.value)
		if !ok {
			if s, isString := w.value.(string); isString {
				if _, err := fmt.Sscanf(s, "%d", &n); err == nil {
					ok = true
				}
			}
		}
		if !ok {
			return toolErrorJSON(errors[1], nil)
		}
		if n < w.floor {
			n = w.floor
		}
		kwargs[w.key] = n
	}
	raw, err := callback(env, kwargs)
	if err != nil {
		return toolErrorJSON(errors[2]+err.Error(), nil)
	}
	if raw == "" {
		return toolErrorJSON(errors[3], nil)
	}
	return termPassthroughJSON(raw)
}

// ReadTerminalTool returns the in-app terminal's contents with line metadata as a JSON string
func ReadTerminalTool(env *Env, startLine, count any) string {
	return readPane(env, ReadTerminalPane, []paneWindow{{"start", startLine, 0}, {"count", count, 1}}, [4]string{
		"read_terminal is only available in the Icebath desktop app.",
		"start_line and count must be integers.",
		"Failed to read terminal: ",
		"No in-app terminal answered: nothing is open, or the bridge timed out. " +
			"If the terminal pane IS open, the desktop app may be older than this " +
			"backend \u2014 update it and retry.",
	})
}

var readTerminalSchema = map[string]any{
	"name": "read_terminal",
	"description": "Read the in-app terminal pane beside this chat. No args = visible " +
		"screen + total_lines; page scrollback with start_line (0 = oldest) " +
		"+ count. JSON: {total_lines, start, end, viewport_rows, cursor_row, " +
		"text}.",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"start_line": map[string]any{"type": "integer", "description": "0-indexed first line (0 = oldest). Omit for the visible screen."},
			"count":      map[string]any{"type": "integer", "description": "Lines to read from start_line. Defaults to the visible row count."},
		},
	},
}

func init() {
	Register(Def{
		Name:    "read_terminal",
		Toolset: "desktop_ui",
		Schema:  readTerminalSchema,
		Handler: func(ctx context.Context, call *Call) string {
			return ReadTerminalTool(call.Env, call.Args["start_line"], call.Args["count"])
		},
		Emoji: "\U0001F5A5\ufe0f",
	})
}
