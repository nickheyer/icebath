package app

import (
	mcpconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/config"
	mcpv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/mcp/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/mcp/v1/mcpv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
)

// Pages inside MCP servers that a search of Settings finds: the page of each server
func serverPages(kit *tui.Kit, service mcpv1connect.McpServiceClient, name string) []tui.Page {
	response, err := service.List(kit.Env.Ctx, &mcpv1.ListRequest{Profile: name})
	if err != nil {
		return nil
	}
	var pages []tui.Page
	for _, server := range response.GetServers() {
		pages = append(pages, tui.Page{Title: server.GetName(), About: "An MCP server reached by " + server.GetTransport(),
			Words: server.GetPlugin(), Argument: server.GetName(), Keys: []string{mcpconfig.Servers + "." + server.GetName() + "."}})
	}
	return pages
}
