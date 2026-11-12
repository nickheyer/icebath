package tools

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	accentSwatches  = []string{"mono", "green", "cyan", "nous", "violet", "pink", "red", "orange"}
	themeAccent     = "theme"
	themeModes      = []string{"light", "dark", "system"}
	themePickers    = []string{"accent", "layout"}
	simpleLayouts   = []string{"sidebar-left", "sidebar-right"}
	advancedLayouts = []string{"default", "basic", "focus", "terminal-deck", "quad"}
	hexColorRe      = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

// Error JSON for an accent that is not a swatch id, theme, or hex colour, empty when valid
func accentError(accent string) string {
	if containsString(accentSwatches, accent) || accent == themeAccent || hexColorRe.MatchString(accent) {
		return ""
	}
	return JSON(Dict("success", false, "error", fmt.Sprintf("Unknown accent %s: use a swatch id, 'theme' or #rrggbb.", pyRepr(accent)),
		"available", append(enumValues(accentSwatches), themeAccent)))
}

// Applies the given appearance values, or shows one picker card and waits for the pick
func SetThemeTool(accent, mode, layout, picker *string, request DesktopAnswer) string {
	if request == nil {
		return toolErrorJSON("set_theme is only available in the Icebath desktop app.", nil)
	}
	values := map[string]any{}
	for key, v := range map[string]*string{"accent": accent, "mode": mode, "layout": layout} {
		if v != nil && strings.TrimSpace(*v) != "" {
			values[key] = strings.TrimSpace(*v)
		}
	}
	if picker != nil {
		if !containsString(themePickers, *picker) {
			return toolErrorJSON(fmt.Sprintf("picker must be one of: %s.", joinEnum(themePickers)), nil)
		}
		if len(values) > 0 {
			return toolErrorJSON("picker shows a card for the user to choose; pass picker alone, without values.", nil)
		}
		return themeReply(request(map[string]any{"picker": *picker}))
	}
	if len(values) == 0 {
		return toolErrorJSON("Pass at least one of accent, mode or layout, or picker to let the user choose.", nil)
	}
	if m, ok := values["mode"].(string); ok && !containsString(themeModes, m) {
		return JSON(Dict("success", false, "error", fmt.Sprintf("Unknown mode %s.", pyRepr(m)), "available", enumValues(themeModes)))
	}
	if a, ok := values["accent"].(string); ok {
		if msg := accentError(a); msg != "" {
			return msg
		}
	}
	return themeReply(request(values))
}

// Renderer answer as JSON, the error as a tool error
func themeReply(reply map[string]any, err error) string {
	if err != nil {
		return toolErrorJSON(err.Error(), nil)
	}
	return JSON(reply)
}

var setThemeSchema = map[string]any{
	"name": "set_theme",
	"description": "Change how the Icebath desktop app looks: accent colour, light/dark mode, layout. Pass the values " +
		"to apply them now; the result says what was applied. Pass picker instead to show the user an " +
		"accent or layout picker card in the chat and wait for their pick. To reveal ONE pane, use " +
		"focus_pane instead.",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"accent": map[string]any{"type": "string", "description": fmt.Sprintf("A swatch id (%s), 'theme' for the theme's own accent, or a hex colour '#rrggbb'.", joinEnum(accentSwatches))},
			"mode":   map[string]any{"type": "string", "enum": enumValues(themeModes)},
			"layout": map[string]any{"type": "string", "description": fmt.Sprintf("Layout preset id. Simple mode: %s. Advanced mode: %s. The user's saved layouts by id.", joinEnum(simpleLayouts), joinEnum(advancedLayouts))},
			"picker": map[string]any{"type": "string", "enum": enumValues(themePickers), "description": "Show that picker card and wait for the user (up to 5 minutes). Pass it alone."},
		},
	},
}
