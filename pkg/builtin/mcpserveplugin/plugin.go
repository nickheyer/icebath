// Package mcpserveplugin is the mcp-serve catalog plugin port, the icebath mcp-serve command
package mcpserveplugin

import (
	"context"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/mcpserve"
	"github.com/nickheyer/icebath/pkg/plugins"
)

const (
	serveHelp        = "Run Icebath as an MCP server (expose conversations, optionally tools, to other agents)"
	serveDescription = "Run Icebath as a stdio MCP server. MCP client config: " +
		`{"command": "icebath", "args": ["mcp-serve"]}. Add --tools to also expose Icebath's ` +
		"web, browser, vision, image generation, skills and TTS tools."
)

func init() { plugins.RegisterPort(Plugin{}) }

// Mcp-serve catalog plugin port, active once icebath plugins install mcp-serve lands it in a home
type Plugin struct{}

// Manifest of the mcp-serve catalog plugin at its compiled-in pin
func (Plugin) Manifest() plugins.Manifest { return plugins.CatalogPortManifest("mcp-serve") }

// Registers icebath mcp-serve
func (Plugin) Register(ctx *plugins.Context) error {
	ctx.RegisterCLICommand("mcp-serve", serveHelp, func(p any) { commands.UsePluginParser(p, "mcp-serve") }, handleServe, serveDescription)
	return nil
}

// Runs the stdio MCP server with the parsed verbose, tools and accept-hooks flags
func handleServe(args any) error {
	ns, _ := args.(map[string]any)
	verbose, _ := ns["verbose"].(bool)
	withTools, _ := ns["tools"].(bool)
	acceptHooks, _ := ns["accept_hooks"].(bool)
	if code := mcpserve.RunServer(context.Background(), verbose, withTools, acceptHooks); code != 0 {
		return &commands.ExitError{Code: code}
	}
	return nil
}
