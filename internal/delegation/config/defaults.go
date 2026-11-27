package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
)

func Register(cfg *config.Config, backends func() []config.Option) {
	efforts := func() []config.Option {
		options := []config.Option{{Title: "The main agent's level"}}
		for _, effort := range provider.Efforts {
			options = append(options, config.Option{Value: effort, Title: effort})
		}
		return options
	}
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Subagents",
		About: "Where the subagents delegate_task starts run, how many and how deep, and what they may do",
		Settings: []config.Setting{
			{Key: Backend, Title: "Subagent backend", About: "Which backend runs subagents; default runs them in this app, plugins can add others", Default: "default",
				Options: backends},
			{Key: Backends, Title: "Backend settings", About: "Settings for each subagent backend, by backend name", Default: map[string]any{}},
			{Key: Model, Title: "Subagent model", About: "Model subagents use instead of the main one", Default: ""},
			{Key: Provider, Title: "Subagent provider", About: "Provider subagents use instead of the main one, as named in this profile", Default: ""},
			{Key: BaseURL, Title: "Subagent endpoint", About: "Own model endpoint address for subagents; empty uses the subagent provider or the main model", Default: ""},
			{Key: APIKey, Title: "Subagent API key", About: "Key used by subagents that use their own endpoint", Default: "", Secret: true},
			{Key: APIMode, Title: "Subagent API style", About: "Request format of the subagent endpoint; empty detects it from the endpoint address", Default: "",
				Options: config.Options(
					config.Option{Title: "Detected from the address"},
					config.Option{Value: "chat_completions", Title: "Chat completions"},
					config.Option{Value: "codex_responses", Title: "Responses"},
					config.Option{Value: "anthropic_messages", Title: "Anthropic messages"},
				)},
			{Key: RequestOverrides, Title: "Subagent request fields", About: "Extra request fields sent with subagent model calls", Default: map[string]any{}},
			{Key: ReasoningEffort, Title: "Subagent effort", About: "How long subagents think before answering", Default: "", Options: efforts},
			{Key: Fallbacks, Title: "Subagent fallbacks", About: "Models subagents try, in order, when their own fails, each provider:model; unset uses the main agent's",
				Default: []string{}},
			{Key: MaxConcurrent, Title: "Parallel subagents", About: "Most subagents that run at the same time, at least 1; each uses model tokens, so high values raise cost",
				Default: 10},
			{Key: MaxSpawnDepth, Title: "Subagent depth", About: "How many levels deep subagents may start more subagents, at least 1; each level multiplies cost",
				Default: 1},
			{Key: OrchestratorEnabled, Title: "Subagent orchestrators", About: "Lets subagents coordinate subagents of their own", Default: true},
			{Key: MaxIterations, Title: "Subagent turn limit", About: "Most tool turns one subagent may take", Default: 250},
			{Key: OneshotMaxChildren, Title: "One-shot subagent limit", About: "Most subagents one single-run session may start; 0 for no limit", Default: 2},
			{Key: ChildTimeout, Title: "Subagent idle limit", About: "Stops a subagent inactive this many seconds; 0 for no limit, anything set raised to at least 30",
				Default: time.Duration(0)},
			{Key: CompressionThreshold, Title: "Subagent compress at", About: "Token size at which a subagent compresses its chat; 0 uses the normal share of the context window, values under 16000 are ignored",
				Default: 0},
			{Key: MaxSummaryChars, Title: "Subagent summary size", About: "Longest subagent report returned to the parent in characters; longer reports are trimmed and saved to disk. 0 sets no fixed ceiling, leaving only the share of the parent's free context",
				Default: 24000},
			{Key: GatewayMode, Title: "Subagents in chats", About: "Whether chat subagents finish within the turn or in the background", Default: GatewayAuto,
				Options: config.Options(
					config.Option{Value: GatewayAuto, Title: "Auto", About: "Decide from the chat surface"},
					config.Option{Value: GatewayInline, Title: "Inline", About: "Wait for subagents inside the turn"},
					config.Option{Value: GatewayBackground, Title: "Background", About: "Run subagents in the background"},
				)},
			{Key: IndependentCompletions, Title: "Separate completions", About: "Reports each background subagent on its own, not as a group", Default: false},
			{Key: WorktreeIsolation, Title: "Subagent worktrees", About: "Give each subagent its own git worktree off your current commit", Default: false},
			{Key: InheritMCPToolsets, Title: "Subagents keep MCP", About: "Narrowed subagents still get the parent's MCP tools", Default: true},
			{Key: SubagentAutoApprove, Title: "Auto-approve subagents", About: "Lets subagents run risky commands without asking; when off, risky commands from subagents are refused",
				Default: false},
			{Key: SurfaceChildProcessNotifications, Title: "Subagent process news", About: "Shows background process updates started by subagents", Default: false},
		},
	}})
}
