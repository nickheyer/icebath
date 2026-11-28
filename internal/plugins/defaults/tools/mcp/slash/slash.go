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
	"github.com/nickheyer/icebath/pkg/strs"
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
	return []slash.Command{{Name: "reload-mcp", Aliases: []string{"reload_mcp"}, Usage: "[now]", Help: "Connect this session's MCP servers again", Run: c.reload}}
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
	// The one rule of asking, which Connect again on the page of MCP servers follows too: ask first while Confirm tool reload is on
	if argument != "now" && cfg.GetBool(mcpconfig.ReloadConfirm) {
		return "Connecting the MCP servers again gives this session your changes. Your next message then sends the whole session again, " +
			"which can cost a lot on a long session.\n\nType /reload-mcp now to connect them again.", nil
	}
	r := c.manager.Reload(ctx, cfg)
	lines := []string{"Connected the MCP servers again.", ""}
	for _, change := range []struct {
		label string
		names []string
	}{{"Connected again", r.Reconnected}, {"Added", r.Added}, {"Removed", r.Removed}} {
		if len(change.names) > 0 {
			lines = append(lines, change.label+": "+strings.Join(change.names, ", "))
		}
	}
	if r.Servers == 0 {
		lines = append(lines, "No MCP server is connected now.")
	} else {
		lines = append(lines, "", strs.Count(r.Tools, "tool")+" from "+strs.Count(r.Servers, "server")+" are ready.")
	}
	if len(r.Failed) > 0 {
		lines = append(lines, "⚠ Could not connect to "+strings.Join(r.Failed, ", ")+". Run icebath mcp test <name> to see why.")
	}
	return strings.Join(lines, "\n"), nil
}
