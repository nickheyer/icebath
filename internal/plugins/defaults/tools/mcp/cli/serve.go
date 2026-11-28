package cli

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/mcp"
	picked "github.com/nickheyer/icebath/internal/profile"
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
		Short: "Let another app use icebath as an MCP server",
		Long: "Runs icebath as an MCP server on standard input and output, for an app that speaks MCP. The app can read and send messages in " +
			"your chat apps, follow events and answer approvals. With --tools, the app can also use the agent's tools for the web, the browser, " +
			"images, skills and speech, with this profile's API keys. The app never gets the terminal, file or subagent tools.",
		Example: "  icebath mcp serve\n  icebath mcp serve --tools",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			level := slog.LevelWarn
			if verbose {
				level = slog.LevelDebug
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
			ctx := cmd.Context()
			listed, err := service.ServeTools(ctx, &mcpv1.ServeToolsRequest{Profile: picked.Picked(ctx), Tools: tools, AcceptHooks: acceptHooks})
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
							response, err := service.Bridge(ctx, &mcpv1.BridgeRequest{Profile: picked.Picked(ctx), Name: name, Arguments: string(data)})
							return response.GetResult(), err
						}
						response, err := service.CallTool(ctx, &mcpv1.CallToolRequest{Profile: picked.Picked(ctx), Name: name, Arguments: string(data), Directory: directory})
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
	flags.BoolVarP(&verbose, "verbose", "v", false, "Log in detail on standard error")
	flags.BoolVar(&tools, "tools", false, "Also let the app use the agent's web, browser, image, skills and speech tools")
	flags.BoolVar(&acceptHooks, "accept-hooks", false, "With --tools, run the hooks of this profile without asking first")
	return cmd
}
