// Package langfuse traces icebath turns, model calls, tool calls and subagents to Langfuse over its OpenTelemetry ingestion endpoint
package langfuse

import (
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/plugins"
)

const (
	pluginName        = "langfuse"
	pluginVersion     = "2.0.0"
	pluginDescription = "Langfuse observability for icebath: one trace per turn, one generation per LLM call (usage " +
		"and cost from icebath's own pricing, including cache and reasoning tokens), one observation per tool call and " +
		"subagent. Content capture is sanitized by default (metadata and full are opt-in). Built into icebath; the old " +
		"key observability/langfuse still selects it."
	endUserLabel       = "Attribute traces to the sender"
	endUserDescription = "Set each gateway trace's Langfuse userId to the sender's platform ID (for example a Matrix, " +
		"Slack or Telegram user ID), so per-user views work. That ID is exported to your Langfuse project. Turns with " +
		"no sender, such as CLI and cron runs, never get a userId."
)

var hookNames = []string{"pre_api_request", "post_api_request", "api_request_error", "pre_llm_call", "post_llm_call",
	"pre_tool_call", "post_tool_call", "on_session_finalize", "on_session_end", "subagent_start", "subagent_stop"}

var registered atomic.Pointer[plugins.Context]

func init() { plugins.RegisterBundled(Plugin{}) }

// Compiled-in Langfuse plugin
type Plugin struct{}

// Manifest of the Langfuse plugin from its plugin.yaml
func (Plugin) Manifest() plugins.Manifest {
	configSchema := map[string]map[string]any{endUserSetting: {"type": "bool", "default": false, "label": endUserLabel,
		"description": endUserDescription}}
	return plugins.Manifest{
		Name:            pluginName,
		Version:         pluginVersion,
		Description:     pluginDescription,
		Author:          "NousResearch",
		Kind:            "standalone",
		ManifestVersion: 2,
		ProvidesHooks:   append([]string(nil), hookNames...),
		RequiresEnv: []plugins.EnvSpec{
			{Name: publicKeyEnv, Description: "Langfuse project public key (pk-lf-...)", URL: "https://cloud.langfuse.com"},
			{Name: secretKeyEnv, Description: "Langfuse project secret key (sk-lf-...)", URL: "https://cloud.langfuse.com", Secret: true},
		},
		ConfigSchema: configSchema,
		Raw: map[string]any{"name": pluginName, "label": "Langfuse", "kind": "standalone", "version": pluginVersion,
			"manifest_version": 2, "author": "NousResearch", "description": pluginDescription,
			"provides_hooks": stringsAny(hookNames),
			"requires_env": []any{
				map[string]any{"name": publicKeyEnv, "description": "Langfuse project public key (pk-lf-...)", "url": "https://cloud.langfuse.com", "secret": false},
				map[string]any{"name": secretKeyEnv, "description": "Langfuse project secret key (sk-lf-...)", "url": "https://cloud.langfuse.com", "secret": true},
			},
			"optional_env": []any{map[string]any{"name": "ICEBATH_LANGFUSE_BASE_URL",
				"description": "Langfuse server URL for a self-hosted instance (default: https://cloud.langfuse.com)",
				"prompt":      "Langfuse server URL", "password": false}},
			"config_schema": map[string]any{endUserSetting: map[string]any{"type": "bool", "default": false, "label": endUserLabel,
				"description": endUserDescription}}},
	}
}

// Strings as a list value
func stringsAny(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// Registers both hook name variants so request and turn scoped callers reach the same handlers
func (Plugin) Register(ctx *plugins.Context) error {
	registered.Store(ctx)
	handlers := map[string]plugins.HookFunc{
		"pre_api_request": onPreLLMRequest, "post_api_request": onPostLLMCall, "api_request_error": onAPIRequestError,
		"pre_llm_call": onPreLLMCall, "post_llm_call": onPostLLMCall, "pre_tool_call": onPreToolCall,
		"post_tool_call": onPostToolCall, "on_session_finalize": onSessionFinalize, "on_session_end": onSessionFinalize,
		"subagent_start": onSubagentStart, "subagent_stop": onSubagentStop,
	}
	for _, name := range hookNames {
		ctx.RegisterHook(name, handlers[name], false)
	}
	return nil
}

// Registration context the profile name, cron identity and settings come from, nil before registration
func pluginContext() *plugins.Context { return registered.Load() }

// Process exit finalizer ending open roots, flushing and shutting every client and its exporter down
func AtExit() {
	if !finalizerArmed.Load() {
		return
	}
	finalizeAllTraces()
	for _, c := range settledClients() {
		failsafe("langfuse shutdown", c.shutdown)
		failsafe("tracer provider shutdown", c.shutdownProvider)
	}
}
