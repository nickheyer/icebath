package teamspipeline

import "github.com/nickheyer/icebath/pkg/plugins"

func init() { plugins.RegisterBundled(Plugin{}) }

// Compiled-in Teams meeting pipeline plugin
type Plugin struct{}

// Manifest of the teams_pipeline plugin from its plugin.yaml
func (Plugin) Manifest() plugins.Manifest {
	return plugins.Manifest{
		Name:        "teams_pipeline",
		Version:     "0.1.0",
		Description: "Microsoft Teams meeting pipeline plugin with durable runtime state and operator CLI flows for Graph-backed transcript-first meeting summaries.",
		Author:      "NousResearch",
		Kind:        "standalone",
		Raw: map[string]any{"name": "teams_pipeline", "version": "0.1.0", "author": "NousResearch", "kind": "standalone",
			"description": "Microsoft Teams meeting pipeline plugin with durable runtime state and operator CLI flows for Graph-backed transcript-first meeting summaries.",
			"platforms":   []any{"linux", "macos", "windows"}},
	}
}

// Registers the operator teams-pipeline CLI command, the plugin's only surface
func (Plugin) Register(ctx *plugins.Context) error {
	ctx.RegisterCLICommand("teams-pipeline", "Inspect and operate the Microsoft Teams meeting pipeline", registerCLI, teamsPipelineCommand,
		"Operator CLI for the Microsoft Teams meeting pipeline. Lists jobs, inspects stored runs, replays jobs, validates Graph setup, and maintains Graph subscriptions.")
	return nil
}
