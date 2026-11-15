package commands

import "github.com/spf13/cobra"

func init() { BuiltinSubcommands["mcp-serve"] = true }

// Adds icebath mcp-serve with its verbose, tools and accept-hooks flags and argparse option abbreviations
func (c *CLI) addMCPServe(root *cobra.Command) {
	serve := c.newCmd(root, "mcp-serve", "Run Icebath as an MCP server (expose conversations, optionally tools, to other agents)",
		"Run Icebath as a stdio MCP server. MCP client config: "+
			`{"command": "icebath", "args": ["mcp-serve"]}. Add --tools to also expose Icebath's `+
			"web, browser, vision, image generation, skills and TTS tools.")
	bf(serve, "verbose", "v", "Enable verbose logging on stderr")
	bf(serve, "tools", "", "Also expose Icebath's web, browser, vision, image generation, skills and TTS tools to the "+
		"MCP client, under this profile's credentials (off by default; never terminal, file or "+
		"kanban tools)")
	bf(serve, "accept-hooks", "", "With --tools: run the shell hooks in config.yaml without the first-use consent check")
	c.argparseAbbrev(serve)
}
