package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// DesktopEmitter delivers one renderer event to the window or session identified by sid
type DesktopEmitter func(sid, event string, payload map[string]any) error

var (
	desktopEmitMu sync.RWMutex
	desktopEmit   DesktopEmitter
)

// Events keyed on the chat session rather than the window so the transcript stream receives them
var desktopSessionScopedEvents = map[string]bool{"message.reaction": true}

// SetDesktopEmitter installs or clears the renderer event sink, called by the desktop gateway
func SetDesktopEmitter(fn DesktopEmitter) {
	desktopEmitMu.Lock()
	desktopEmit = fn
	desktopEmitMu.Unlock()
}

// DesktopAvailable reports whether a renderer event sink is wired, true only under the desktop app
func DesktopAvailable() bool {
	desktopEmitMu.RLock()
	defer desktopEmitMu.RUnlock()
	return desktopEmit != nil
}

// Reads a desktop Appearance switch from display.<setting>, the default when the config is unreadable
func desktopUserEnabled(env *Env, setting string, fallback bool) bool {
	doc, err := readRawConfig(env)
	if err != nil {
		return fallback
	}
	display, ok := doc["display"].(map[string]any)
	if !ok {
		return fallback
	}
	v, present := display[setting]
	if !present {
		return fallback
	}
	return truthy(v)
}

// Routes the event to the window owning the current turn, false when no emitter is installed
func desktopEmitEvent(env *Env, event string, payload map[string]any) (bool, error) {
	desktopEmitMu.RLock()
	fn := desktopEmit
	desktopEmitMu.RUnlock()
	if fn == nil {
		return false, nil
	}
	var sid string
	if desktopSessionScopedEvents[event] {
		sid = sessionEnvValue(env, "ICEBATH_SESSION_ID")
		if sid == "" {
			sid = sessionEnvValue(env, "ICEBATH_UI_SESSION_ID")
		}
	} else {
		sid = sessionEnvValue(env, "ICEBATH_UI_SESSION_ID")
	}
	if err := fn(sid, event, payload); err != nil {
		return false, err
	}
	return true, nil
}

// Emits the event and returns the result JSON, the failure text, or the desktop only text
func desktopEmitOrError(env *Env, event string, payload map[string]any, failPrefix, desktopOnly string, result map[string]any) string {
	ok, err := desktopEmitEvent(env, event, payload)
	if err != nil {
		return toolErrorJSON(failPrefix+err.Error(), nil)
	}
	if !ok {
		return toolErrorJSON(desktopOnly, nil)
	}
	return jsonCompact(result)
}

// Desktop answers with a JSON object, passed through, else the raw text wrapped
func desktopPassthroughJSON(raw string) string {
	if v, err := pyjson.DecodeWTF8([]byte(raw)); err == nil {
		return jsonCompact(v)
	}
	return jsonCompact(map[string]any{"text": raw})
}

// Comma joined list for enum error texts
func joinEnum(values []string) string { return strings.Join(values, ", ") }

var tipSides = []string{"top", "right", "bottom", "left"}

// Shows one tip bubble anchored to the selector
func tipTool(env *Env, text, selector, title, side string) string {
	text = strings.TrimSpace(text)
	selector = strings.TrimSpace(selector)
	if text == "" {
		return toolErrorJSON("tip needs text — the one line the bubble says.", nil)
	}
	if selector == "" {
		return toolErrorJSON("tip needs a selector to point at. Call tour(action='targets') to see "+
			"what's on screen and prefer a target reporting stable: true.", nil)
	}
	if side != "" && !containsString(tipSides, side) {
		return toolErrorJSON(fmt.Sprintf("side must be one of: %s.", joinEnum(tipSides)), nil)
	}
	payload := map[string]any{"selector": selector, "text": text}
	if title != "" {
		payload["title"] = title
	}
	if side != "" {
		payload["side"] = side
	}
	ok, err := desktopEmitEvent(env, "tip.show", payload)
	if err != nil {
		return toolErrorJSON("Failed to show the tip: "+err.Error(), nil)
	}
	if !ok {
		return toolErrorJSON("tip is only available in the Icebath desktop app.", nil)
	}
	return jsonCompact(map[string]any{"success": true, "selector": selector})
}

var tipSchema = map[string]any{
	"name": "show_tip",
	"description": "Point at one desktop UI element with a small arrow bubble (no dimming or tour). " +
		"For when a sentence is clearer with a finger on its subject. Get selectors from tour(action='targets'), " +
		"prefer stable:true, never guess. One tip at a time (new replaces " +
		"last); say the same thing in chat too — the bubble is a pointer, " +
		"not the message. Sparingly: a bubble every turn stops being read.",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text":     map[string]any{"type": "string", "description": "The one-sentence bubble text."},
			"selector": map[string]any{"type": "string", "description": "Selector from tour targets."},
			"title":    map[string]any{"type": "string", "description": "Optional heading."},
			"side":     map[string]any{"type": "string", "enum": enumValues(tipSides), "description": "Omit for 'top'; flips at screen edges."},
		},
		"required": []any{"text", "selector"},
	},
}

var panes = []string{"chat", "files", "terminal", "review", "sessions"}

// Asks the desktop GUI to reveal and focus the pane
func focusPaneTool(env *Env, pane string) string {
	name := strings.ToLower(strings.TrimSpace(pane))
	if !containsString(panes, name) {
		return toolErrorJSON(fmt.Sprintf("pane must be one of: %s.", joinEnum(panes)), nil)
	}
	return desktopEmitOrError(env, "pane.reveal", map[string]any{"pane": name}, fmt.Sprintf("Failed to focus the %s pane: ", name),
		"Pane focus is only available in the Icebath desktop app.", map[string]any{"success": true, "pane": name})
}

var focusPaneSchema = map[string]any{
	"name": "focus_pane",
	"description": "Show a desktop pane: chat, files, terminal, review (git diff) or sessions. " +
		"Use when the user asks to see one; for URLs/files use desktop_preview.",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pane": map[string]any{"type": "string", "enum": enumValues(panes), "description": "Which pane to reveal."},
		},
		"required": []any{"pane"},
	},
}

// ReadWindowBelowPane asks the desktop main process which OS window sits under the app window
var ReadWindowBelowPane func(env *Env, window map[string]int) (string, error)

// Returns the window underneath the desktop window as a JSON string
func readWindowBelowTool(env *Env) string {
	return readPane(env, ReadWindowBelowPane, nil, [4]string{
		"read_window_below is only available in the Icebath desktop app.",
		"",
		"Failed to read the window below: ",
		"Could not determine the window underneath (the desktop app did " +
			"not answer, or window enumeration is unavailable on this system).",
	})
}

var readWindowBelowSchema = map[string]any{
	"name": "read_window_below",
	"description": "Identify the app window behind the Icebath desktop window (what the user is in). JSON: {window: {app, title, bounds, " +
		"id}, frontmost, platform}. title may be empty when the OS withholds " +
		"it (noted in `note`); where windows cannot be enumerated at all, " +
		"{error, platform} says what would fix it — relay that instead of " +
		"retrying. Metadata only; never captures pixels.",
	"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
}

// Strings as a JSON list value
func enumValues(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// Registers every desktop_ui and onboarding adjacent tool the desktop renderer serves
func registerDesktopUITools() {
	Register(Def{Name: "show_tip", Toolset: "desktop_ui", Schema: tipSchema, Emoji: "\U0001F4A1",
		Check: func(env *Env) bool { return desktopUserEnabled(env, "in_app_tips", true) },
		Handler: func(ctx context.Context, call *Call) string {
			return tipTool(call.Env, argS(call.Args, "text"), argS(call.Args, "selector"), argS(call.Args, "title"), argS(call.Args, "side"))
		}})
	Register(Def{Name: "focus_pane", Toolset: "desktop_ui", Schema: focusPaneSchema, Emoji: "\U0001FA9F",
		Handler: func(ctx context.Context, call *Call) string { return focusPaneTool(call.Env, argS(call.Args, "pane")) }})
	Register(Def{Name: "read_window_below", Toolset: "desktop_ui", Schema: readWindowBelowSchema, Emoji: "\U0001FA9F",
		Handler: func(ctx context.Context, call *Call) string { return readWindowBelowTool(call.Env) }})
	Register(Def{Name: "desktop_preview", Toolset: "desktop_ui", Schema: previewSchema, Emoji: "\U0001F5BC️", Handler: handlePreview})
	Register(Def{Name: "annotate_preview", Toolset: "desktop_ui", Schema: annotatePreviewSchema, Emoji: "\U0001F516",
		Handler: func(ctx context.Context, call *Call) string {
			return annotatePreviewTool(call.Env, argS(call.Args, "action"), optString(call.Args, "ref"), optString(call.Args, "selector"), optString(call.Args, "label"))
		}})
	Register(Def{Name: "drive_preview", Toolset: "desktop_ui", Schema: drivePreviewSchema, Emoji: "\U0001F5B1️", Handler: handleDrivePreview})
	Register(Def{Name: "gui_tour", Toolset: "desktop_ui", Schema: tourSchema, Emoji: "\U0001F9ED",
		Check:   func(env *Env) bool { return desktopUserEnabled(env, "in_app_tours", true) },
		Handler: handleTour})
	Register(Def{Name: "set_theme", Toolset: "desktop_ui", Schema: setThemeSchema, Emoji: "\U0001F3A8",
		Handler: func(ctx context.Context, call *Call) string {
			return setThemeTool(call.Env, optString(call.Args, "accent"), optString(call.Args, "mode"), optString(call.Args, "layout"), optString(call.Args, "picker"))
		}})
}

// String argument pointer, nil when absent or null, so optional and empty stay distinct
func optString(args map[string]any, key string) *string {
	v, ok := args[key]
	if !ok || v == nil {
		return nil
	}
	s := argS(args, key)
	return &s
}
