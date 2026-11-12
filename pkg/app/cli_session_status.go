package app

import (
	"os"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
)

// Prints the compact startup status line of key state, model, tool count, toolsets and provider
func (c *CLI) showStatus() {
	var toolStatus string
	store := c.gatewayStore()
	var inventory map[string]any
	if store != nil {
		inventory = store.BannerInventory()
	}
	if os.Getenv("ICEBATH_DEFER_AGENT_STARTUP") == "1" || (store != nil && inventory == nil) {
		toolStatus = c.t("cli.session.status_tools_deferred", nil)
	} else if inventory != nil {
		listed, _ := inventory["tools"].([]any)
		toolStatus = c.t("cli.session.status_tools_count", map[string]any{"count": len(listed)})
	} else {
		toolStatus = c.t("cli.session.status_tools_count", map[string]any{"count": len(toolDefinitionsFor(c, false))})
	}
	modelShort := c.model
	if strings.Contains(modelShort, "/") {
		modelShort = modelShort[strings.LastIndex(modelShort, "/")+1:]
	}
	if r := []rune(modelShort); len(r) > 30 {
		modelShort = string(r[:27]) + "..."
	}
	apiIndicator := "[red bold]●[/]"
	if c.apiKey != "" {
		apiIndicator = "[green bold]●[/]"
	}
	separatorColor, accentColor, labelColor := "#B8860B", "#FFBF00", "cyan"
	if skin := config.GetActiveSkin(c.ctx); skin != nil {
		separatorColor, accentColor, labelColor = skin.GetColor("banner_dim", "#B8860B"), skin.GetColor("ui_accent", "#FFBF00"), skin.GetColor("ui_label", "#DAA520")
	}
	sep := " [dim " + separatorColor + "]·[/] "
	toolsetsInfo := ""
	if len(c.enabledToolsets) > 0 && !slices.Contains(c.enabledToolsets, "all") {
		toolsetsInfo = sep + "[" + labelColor + "]" + pyrich.Escape(c.t("cli.session.status_toolsets", map[string]any{"toolsets": strings.Join(c.enabledToolsets, ", ")})) + "[/]"
	}
	providerInfo := sep + "[dim]" + pyrich.Escape(c.t("cli.session.status_provider", map[string]any{"provider": c.provider})) + "[/]"
	if c.providerSource != "" {
		providerInfo += sep + "[dim]" + pyrich.Escape(c.t("cli.session.status_auth", map[string]any{"source": c.providerSource})) + "[/]"
	}
	c.consolePrint("  " + apiIndicator + " [" + accentColor + "]" + modelShort + "[/]" + sep + "[bold " + labelColor + "]" + toolStatus + "[/]" + toolsetsInfo + providerInfo)
}
