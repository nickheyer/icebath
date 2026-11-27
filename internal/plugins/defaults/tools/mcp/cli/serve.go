package cli

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/mcp"
	"github.com/nickheyer/icebath/internal/provider"
	mcpv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/mcp/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/mcp/v1/mcpv1connect"
)

// Name MCP clients see icebath by
const serverName = "icebath"

// Command that serves icebath to an MCP client over stdio: its conversations and, with --tools, some of the agent's own tools
func serve(service mcpv1connect.McpServiceClient) *cobra.Command {
	var verbose, tools, acceptHooks bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve icebath to an MCP client over stdio",
		Long: "Serve icebath to an MCP client over stdio: its messaging conversations, events and approvals, and with --tools its web, browser, " +
			"vision, image generation, skills and speech tools under this profile's credentials.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			level := slog.LevelWarn
			if verbose {
				level = slog.LevelDebug
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
			ctx := cmd.Context()
			listed, err := service.ServeTools(ctx, &mcpv1.ServeToolsRequest{Tools: tools, AcceptHooks: acceptHooks})
			if err != nil {
				return err
			}
			directory, err := os.Getwd()
			if err != nil {
				return err
			}
			server := &mcp.Server{Name: serverName, Version: provider.Version(), Instructions: listed.GetInstructions()}
			exposed := 0
			for _, t := range listed.GetTools() {
				var schema map[string]any
				if err := json.Unmarshal([]byte(t.GetSchema()), &schema); err != nil || schema == nil {
					schema = map[string]any{"type": "object", "properties": map[string]any{}}
				}
				name, agent := t.GetName(), t.GetAgent()
				if agent {
					exposed++
				}
				server.Tools = append(server.Tools, mcp.ServerTool{Name: name, Description: t.GetDescription(), InputSchema: schema,
					Handler: func(ctx context.Context, arguments map[string]any) (string, error) {
						data, err := json.Marshal(arguments)
						if err != nil {
							return "", err
						}
						if !agent {
							response, err := service.Bridge(ctx, &mcpv1.BridgeRequest{Name: name, Arguments: string(data)})
							return response.GetResult(), err
						}
						response, err := service.CallTool(ctx, &mcpv1.CallToolRequest{Name: name, Arguments: string(data), Directory: directory})
						if err != nil {
							return "", err
						}
						if response.GetFailed() {
							return "", errors.New(response.GetResult())
						}
						return response.GetResult(), nil
					}})
			}
			if tools {
				slog.Info("exposed icebath tools over MCP", "count", exposed)
			}
			err = server.Serve(ctx, os.Stdin, os.Stdout)
			if ctx.Err() != nil {
				return nil
			}
			return err
		},
	}
	flags := cmd.Flags()
	flags.BoolVarP(&verbose, "verbose", "v", false, "log in detail on stderr")
	flags.BoolVar(&tools, "tools", false, "also expose icebath's web, browser, vision, image generation, skills and speech tools to the MCP client, "+
		"under this profile's credentials (off by default; never terminal, file or delegation tools)")
	flags.BoolVar(&acceptHooks, "accept-hooks", false, "with --tools, run the profile's shell hooks without asking for consent on first use")
	return cmd
}
