package app

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
)

// Layers skin colors over the base style, hex tokens remapped on a light terminal unless the style paints a background
func (c *CLI) buildTUIStyleDict() *styleDict {
	d := c.tui.styleBase.clone()
	d.update(config.GetPromptToolkitStyleOverrides(c.ctx))
	if !detectLightMode() {
		return d
	}
	for _, key := range d.keys {
		tokens := strings.Fields(d.values[key])
		paintsBackground := false
		for _, tok := range tokens {
			paintsBackground = paintsBackground || strings.HasPrefix(tok, "bg:")
		}
		if paintsBackground || d.values[key] == "" {
			continue
		}
		for i, tok := range tokens {
			if strings.HasPrefix(tok, "#") {
				tokens[i] = maybeRemapForLightMode(tok)
			}
		}
		d.values[key] = strings.Join(tokens, " ")
	}
	return d
}

// Refreshes the style of a running prompt after a skin change
func (c *CLI) applyTUISkinStyle() bool {
	if c.tui.app == nil || c.tui.styleBase == nil {
		return false
	}
	if err := c.tui.app.setStyle(c.buildTUIStyleDict()); err != nil {
		panic(err)
	}
	_ = c.invalidateWithin(0)
	return true
}

// tuiLayoutChildren names every widget the root column orders
type tuiLayoutChildren struct {
	sudo, secret, connection, approval, slashConfirm, clarify, modelPicker, commandPalette *ptWindow
	spinner, spacer, statusBar, inputRuleTop, imageBar, inputArea, inputRuleBot            *ptWindow
	voiceStatusBar, completionsMenu                                                        *ptWindow
}

// Connection phases to the catalog keys of their hints
var tuiConnectionHintKeys = map[string]string{"form": "cli.tui.hint_connection_form", "failed": "cli.tui.hint_connection_form",
	"url": "cli.tui.hint_connection_url", "authorized": "cli.tui.hint_connection_authorized", "waiting": "cli.tui.hint_connection_waiting"}
