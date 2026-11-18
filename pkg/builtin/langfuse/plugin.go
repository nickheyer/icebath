// Package langfuse traces icebath turns, model calls, tool calls and subagents to Langfuse over its OpenTelemetry ingestion endpoint
package langfuse

import (
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/plugins"
)

var hookNames = []string{"pre_api_request", "post_api_request", "api_request_error", "pre_llm_call", "post_llm_call",
	"pre_tool_call", "post_tool_call", "on_session_finalize", "on_session_end", "subagent_start", "subagent_stop"}

var registered atomic.Pointer[plugins.Context]

func init() { plugins.RegisterPort(Plugin{}) }

// Langfuse catalog plugin port, active once icebath plugins install langfuse lands it in a home
type Plugin struct{}

// Manifest of the langfuse catalog plugin at its compiled-in pin
func (Plugin) Manifest() plugins.Manifest { return plugins.CatalogPortManifest("langfuse") }

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
