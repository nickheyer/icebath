package tools

import (
	"context"
	"fmt"
	"strings"
)

// TourRequest sends one tour request to the renderer and returns its raw reply, installed by the desktop gateway
var TourRequest func(env *Env, payload map[string]any) (string, error)

var (
	tourActions  = []string{"targets", "show", "start", "next", "prev", "stop"}
	tourSurfaces = []string{"app", "preview"}
	tourPresets  = []string{"quick", "full"}
)

// Dispatches one tour action to the desktop renderer and returns its outcome
func handleTour(ctx context.Context, call *Call) string {
	args := call.Args
	if TourRequest == nil {
		return toolErrorJSON("tour is only available in the Icebath desktop app.", nil)
	}
	verb := strings.ToLower(strings.TrimSpace(argS(args, "action")))
	if !containsString(tourActions, verb) {
		return toolErrorJSON(fmt.Sprintf("action must be one of: %s.", joinEnum(tourActions)), nil)
	}
	where := "app"
	if s := strings.ToLower(strings.TrimSpace(argS(args, "surface"))); s != "" {
		where = s
	}
	if !containsString(tourSurfaces, where) {
		return toolErrorJSON(fmt.Sprintf("surface must be one of: %s.", joinEnum(tourSurfaces)), nil)
	}
	side, preset := optString(args, "side"), optString(args, "preset")
	if side != nil && !containsString(tipSides, *side) {
		return toolErrorJSON(fmt.Sprintf("side must be one of: %s.", joinEnum(tipSides)), nil)
	}
	if preset != nil && !containsString(tourPresets, *preset) {
		return toolErrorJSON(fmt.Sprintf("preset must be one of: %s.", joinEnum(tourPresets)), nil)
	}
	steps, hasSteps := args["steps"]
	hasSteps = hasSteps && steps != nil
	if preset != nil && hasSteps {
		return toolErrorJSON("preset picks a built-in tour; pass steps or preset, not both.", nil)
	}
	selector, title, text := optString(args, "selector"), optString(args, "title"), optString(args, "text")
	if verb == "show" && derefEmpty(selector) && derefEmpty(title) && derefEmpty(text) {
		return toolErrorJSON("show needs a selector (and/or title/text for the popover).", nil)
	}
	if verb == "start" && !hasSteps && where != "app" {
		return toolErrorJSON("The built-in tour runs on surface='app'; pass steps for a preview tour.", nil)
	}
	if verb == "start" && hasSteps {
		list, ok := steps.([]any)
		if !ok || len(list) == 0 {
			return toolErrorJSON("start needs a non-empty steps array, or no steps for the built-in tour.", nil)
		}
		for i, step := range list {
			m, ok := step.(map[string]any)
			if !ok {
				return toolErrorJSON(fmt.Sprintf("steps[%d] must be an object.", i), nil)
			}
			if !truthy(m["selector"]) && !truthy(m["title"]) && !truthy(m["text"]) {
				return toolErrorJSON(fmt.Sprintf("steps[%d] needs a selector and/or title/text.", i), nil)
			}
		}
	}
	payload := map[string]any{"action": verb, "surface": where}
	for name, v := range map[string]*string{"selector": selector, "title": title, "text": text, "side": side, "preset": preset} {
		if v != nil {
			payload[name] = *v
		}
	}
	if hasSteps {
		payload["steps"] = steps
	}
	if argHas(args, "step_index") {
		payload["step_index"] = args["step_index"]
	}
	raw, err := TourRequest(call.Env, payload)
	if err != nil {
		return toolErrorJSON("Tour action failed: "+err.Error(), nil)
	}
	if raw == "" {
		return toolErrorJSON("The tour request timed out, or no GUI window answered. "+
			"For surface='preview' open a page in the preview pane first.", nil)
	}
	return desktopPassthroughJSON(raw)
}

var tourStepSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"selector": map[string]any{"type": "string", "description": "Element to highlight; omit = centered narration."},
		"title":    map[string]any{"type": "string", "description": "Popover title."},
		"text":     map[string]any{"type": "string", "description": "Popover body."},
		"side":     map[string]any{"type": "string", "enum": enumValues(tipSides), "description": "Popover side; omit to auto-place."},
	},
}

var tourSchema = map[string]any{
	"name": "gui_tour",
	"description": "Run a guided desktop GUI tour: dim the screen, highlight elements, add popovers. Surfaces: 'app' (Icebath itself) " +
		"or 'preview' (the page in the preview pane). A general look around " +
		"the app is ONE call: action='start' with no steps runs the app's " +
		"built-in tour (preset 'quick' or 'full', default full). For a " +
		"custom tour, call action='targets' first — prefer targets marked " +
		"stable:true (their selectors survive re-renders); re-scan if one " +
		"stops matching. Then narrate with action='show' (one highlight per " +
		"call, replaces the last — pair each with a chat message) or hand " +
		"over with action='start' + steps (user gets Next/Prev; " +
		"'next'/'prev' also page it). 'stop' clears. Use for " +
		"how-does-X-work / where-is-Y walkthroughs.",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":     map[string]any{"type": "string", "enum": enumValues(tourActions), "description": "start alone runs the built-in tour; targets first for custom steps."},
			"surface":    map[string]any{"type": "string", "enum": enumValues(tourSurfaces), "description": "'app' (default) or 'preview'."},
			"selector":   map[string]any{"type": "string", "description": "show: selector from targets (prefer stable). Omit = centered narration."},
			"title":      map[string]any{"type": "string", "description": "show: popover title."},
			"text":       map[string]any{"type": "string", "description": "show: popover body."},
			"side":       map[string]any{"type": "string", "enum": enumValues(tipSides), "description": "show: popover side; omit to auto-place."},
			"steps":      map[string]any{"type": "array", "items": tourStepSchema, "description": "start: ordered steps; omit for the built-in tour."},
			"preset":     map[string]any{"type": "string", "enum": enumValues(tourPresets), "description": "start without steps: 'quick' (four essentials) or 'full'."},
			"step_index": map[string]any{"type": "integer", "description": "start: 0-indexed first step."},
		},
		"required": []any{"action"},
	},
}
