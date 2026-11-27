package slash

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	mcpconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/config"
	mcptools "github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/tools"
	"github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/slash"
)

type commands struct {
	config   *config.Config
	manager  *mcptools.Manager
	sessions *sessions.Registry
}

func Register(registry *slash.Registry, cfg *config.Config, manager *mcptools.Manager, stores *sessions.Registry) {
	registry.Register(commands{config: cfg, manager: manager, sessions: stores})
}

func (c commands) Commands() []slash.Command {
	return []slash.Command{{Name: "reload-mcp", Aliases: []string{"reload_mcp"}, Usage: "[now]", Help: "Reload MCP servers in the live session", Run: c.reload}}
}

// Stops the session's MCP servers and connects them again from the settings, asking first unless told now or the setting says not to
func (c commands) reload(ctx context.Context, call *slash.Call) (string, error) {
	cfg, err := c.sessions.Profile(ctx, c.config, call.SessionID)
	if err != nil {
		return "", err
	}
	argument := strings.ToLower(strings.TrimSpace(call.Argument))
	if argument != "" && argument != "now" {
		return "", fmt.Errorf("Usage: /reload-mcp [now]")
	}
	if argument != "now" && cfg.GetBool(mcpconfig.ReloadConfirm) {
		return "**Confirm /reload-mcp**\n\nReloading MCP servers rebuilds the tool set for this session and **invalidates the provider prompt cache** — the " +
			"next message will re-send full input tokens. On long-context or high-reasoning models this can be expensive.\n\n" +
			"• `/reload-mcp now` — reload now\n• turn off **Confirm tool reload** in /settings (" + mcpconfig.ReloadConfirm + ") — reload without this prompt " +
			"from now on\n\nOtherwise MCP tools stay unchanged.", nil
	}
	r := c.manager.Reload(ctx, cfg)
	lines := []string{"**MCP Servers Reloaded**", ""}
	for _, change := range []struct {
		label string
		names []string
	}{{"Reconnected", r.Reconnected}, {"Added", r.Added}, {"Removed", r.Removed}} {
		if len(change.names) > 0 {
			lines = append(lines, change.label+": "+strings.Join(change.names, ", "))
		}
	}
	if r.Servers == 0 {
		lines = append(lines, "No MCP servers connected.")
	} else {
		lines = append(lines, "", fmt.Sprintf("%d tool(s) available from %d server(s)", r.Tools, r.Servers))
	}
	if len(r.Failed) > 0 {
		lines = append(lines, "MCP reload failed: did not connect: "+strings.Join(r.Failed, ", "))
	}
	return strings.Join(lines, "\n"), nil
}
