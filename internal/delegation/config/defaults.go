package config

import (
	"time"

	auxiliaryconfig "github.com/nickheyer/icebath/internal/auxiliary/config"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
)

// Methods that can run for the profile of the values, the choices of Method
type methods func(values *config.Config) func() []config.Option

// Registers the settings of subagents, the choice of method made for the profile whose settings are read, and the sections of the methods'
// own settings
func Register(cfg *config.Config, backends methods, own config.Registrant) {
	cfg.Register(sections(backends))
	cfg.Register(own)
}

// Section of the subagents, made for the profile of the values
type sections methods

func (backends sections) Sections(values *config.Config) []config.Section {
	efforts := func() []config.Option {
		return provider.EffortOptions(config.Option{Title: "Same as the agent"})
	}
	return []config.Section{{
		Key:   Key,
		Title: "Subagents",
		About: "Helper agents that split up a big task",
		Group: config.GroupAbilities,
		Order: 70,
		Settings: []config.Setting{
			{Key: Provider, Title: "Provider", About: "The provider that subagents use instead of the agent's.", Default: "", Zero: "Same as the agent",
				Picks: providerconfig.PickProvider},
			{Key: Model, Title: "Model", About: "The model that subagents use instead of the agent's.", Default: "", Zero: "Same as the agent",
				Picks: providerconfig.PickModel},
			{Key: ReasoningEffort, Title: "Reasoning effort", About: "How much subagents think before they answer.", Default: "", Options: efforts,
				Picks: auxiliaryconfig.PickEffort},
			{Key: Fallbacks, Title: "Fallback models", About: "Models that subagents try in order when their own model fails.", Default: []string{},
				Zero: "Same as the agent", Picks: providerconfig.PickModel, Ordered: true, Entry: &config.Entry{Noun: "model"}},
			{Key: MaxConcurrent, Title: "Running at once", About: "How many subagents may run at the same time.",
				Help: "Each one uses model tokens, so more at once costs more.", Default: 10, Counts: "subagents", Least: config.Num(1)},
			{Key: MaxSpawnDepth, Title: "Nesting depth", About: "How many levels deep subagents may start subagents of their own.",
				Help: "Each level multiplies the cost.", Default: 1, Counts: "levels", Least: config.Num(1)},
			{Key: OrchestratorEnabled, Title: "Delegate further", About: "Let subagents start and lead subagents of their own.", Default: true},
			{Key: SubagentAutoApprove, Title: "Run risky commands", About: "Let subagents run risky commands without asking.",
				Help: "When this is off, risky commands from subagents are refused.", Default: false},
			{Key: WorktreeIsolation, Title: "Separate worktrees", About: "Give each subagent a git worktree of its own, made from your current commit.",
				Default: false},
			{Key: Backend, Title: "Method", About: "Which method decides where each subagent runs.", Default: "default", Options: backends(values),
				Slash: "delegation", Picks: PickMethod, Advanced: true},
			{Key: Backends, Title: "Method settings", About: "Settings of each method, listed by the name of the method.", Default: map[string]any{},
				Internal: true},
			{Key: BaseURL, Title: "Base URL", About: "The address of a server of your own for subagents.", Help: "Such as http://localhost:8080/v1.",
				Default: "", Zero: "The provider's address", Starts: []string{"http://", "https://"}, Advanced: true},
			{Key: APIKey, Title: "API key", About: "Your API key for the server at Base URL.", Default: "", Secret: true, Advanced: true},
			{Key: APIMode, Title: "Request format", About: "Which API the server at Base URL speaks.", Default: "", Advanced: true,
				Options: config.Options(
					config.Option{Title: "From the address", About: "Told from the Base URL"},
					config.Option{Value: "chat_completions", Title: "OpenAI chat completions"},
					config.Option{Value: "codex_responses", Title: "OpenAI responses"},
					config.Option{Value: "anthropic_messages", Title: "Anthropic messages"},
				)},
			{Key: RequestOverrides, Title: "Extra request fields", About: "Fields added as they are to every model request of a subagent.",
				Default: map[string]any{}, Advanced: true, Entry: &config.Entry{Noun: "field", Name: "Field name", Fields: []config.Setting{auxiliaryconfig.RequestField}}},
			{Key: MaxIterations, Title: "Step limit", About: "The most model calls one subagent may make.", Default: 250, Counts: "calls",
				Least: config.Num(1), Advanced: true},
			{Key: OneshotMaxChildren, Title: "Single-question limit", About: "The most subagents that a single-question run may start.", Default: 2,
				Zero: "No limit", Counts: "subagents", Least: config.Num(0), Advanced: true},
			{Key: ChildTimeout, Title: "Idle limit", About: "How long a subagent may go without progress before it is stopped.",
				Help: "A limit under 30 seconds is raised to 30 seconds.", Default: time.Duration(0), Zero: "No limit", Advanced: true},
			{Key: CompressionThreshold, Title: "Summarize at", About: "The session size at which a subagent summarizes its older messages.", Default: 0,
				Zero: "The usual share", Counts: "tokens", Least: config.Num(16000), Advanced: true},
			{Key: MaxSummaryChars, Title: "Report size limit", About: "The longest report that a subagent hands back whole.",
				Help: "A longer report is cut short and saved to a file.", Default: 24000, Zero: "No fixed limit", Counts: "characters",
				Least: config.Num(0), Advanced: true},
			{Key: GatewayMode, Title: "In chat apps", About: "Whether subagents in chat apps finish within the reply or in the background.",
				Default: GatewayAuto, Advanced: true, Options: config.Options(
					config.Option{Value: GatewayAuto, Title: "Automatic", About: "Decided by the chat app"},
					config.Option{Value: GatewayInline, Title: "Within the reply", About: "Wait for subagents before the reply ends"},
					config.Option{Value: GatewayBackground, Title: "In the background", About: "Report each subagent when it finishes"},
				)},
			{Key: IndependentCompletions, Title: "Report one by one", About: "Report each background subagent on its own, not as a group.",
				Default: false, Advanced: true},
			{Key: InheritMCPToolsets, Title: "Keep MCP tools", About: "Give the MCP tools of the agent to subagents that are limited to some tools.",
				Default: true, Advanced: true},
			{Key: SurfaceChildProcessNotifications, Title: "Process updates", About: "Show updates from background processes that subagents start.",
				Default: false, Advanced: true},
		},
	}}
}
