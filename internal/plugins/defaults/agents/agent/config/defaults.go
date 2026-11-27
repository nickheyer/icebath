package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/auxiliary"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
)

func Register(cfg *config.Config, providers, personalities func() []config.Option) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Agent",
		About: "The model that answers you and how it works",
		Group: config.GroupModels,
		Order: 20,
		Settings: []config.Setting{
			{Key: Provider, Title: "Provider", About: "The provider the agent uses unless a session picks its own.", Default: "", Slash: "model",
				Options: func() []config.Option {
					return append([]config.Option{{Title: "First connected", About: "The first provider icebath finds an account or key for"}},
						providers()...)
				}},
			{Key: Model, Title: "Model", About: "The model the agent uses unless a session picks its own.", Help: "A model name or one of your model shortcuts.",
				Default: "", Zero: "The provider's default", Slash: "model"},
			{Key: ReasoningEffort, Title: "Reasoning effort", About: "How much the model thinks before it answers.", Default: "", Slash: "reasoning",
				Options: func() []config.Option { return provider.EffortOptions(config.Option{Title: "The model's default"}) }},
			{Key: Thinking, Title: "Reasoning display", About: "How the reasoning of the model is shown in the chat.", Default: ThinkingFolded,
				Slash: "reasoning", Options: config.Options(
					config.Option{Value: ThinkingHidden, Title: "Hidden", About: "Not shown"},
					config.Option{Value: ThinkingFolded, Title: "Collapsed", About: "One line that you can click to open"},
					config.Option{Value: ThinkingOpen, Title: "Expanded", About: "Shown in full"},
				)},
			{Key: MaxTurns, Title: "Max steps", About: "The most model calls the agent may make for one reply.", Default: 0, Zero: "No limit", Counts: "calls",
				Apply: config.NextMessage},
			{Key: Fallbacks, Title: "Fallback models", About: "Models to try in order when the main model fails.",
				Help: "Write each as provider:model, with the name the provider has in this profile.", Default: []string{}},
			{Key: BusyInputMode, Title: "Enter while busy", About: "What happens to a message you send while the agent is working.", Default: BusyQueue,
				Options: config.Options(
					config.Option{Value: BusyQueue, Title: "Queue it", About: "Send it after the current reply ends"},
					config.Option{Value: BusySteer, Title: "Steer", About: "Add it to the current reply"},
					config.Option{Value: BusyInterrupt, Title: "Interrupt", About: "Stop the reply and send it now"},
				)},
			{Key: Aliases, Title: "Model shortcuts", About: "Short names you can use in place of a model's full name.",
				Help: "Use a shortcut in the Model setting, with /model and for subagents of a profile.", Default: map[string]any{}, Advanced: true,
				Entry: &config.Entry{Noun: "shortcut", Name: "Shortcut name", Fields: []config.Setting{
					{Key: "model", Title: "Model", About: "The model the shortcut stands for.", Default: ""},
					{Key: "provider", Title: "Provider", About: "The provider that runs the model.", Default: "", Zero: "The profile's provider"},
					{Key: "base_url", Title: "Base URL", About: "The address of a server of your own that runs the model.", Default: "",
						Zero: "The provider's address"},
					{Key: "api_key", Title: "API key", About: "Your API key for that server.", Help: "Write ${NAME} to read it from a variable.",
						Default: "", Secret: true},
					{Key: "key_env", Title: "Key variable", About: "The environment variable that holds the API key.", Default: ""},
				}}},
			{Key: ContextLength, Title: "Context window", About: "How much text the model can read at once.",
				Help: "Set this only when icebath gets the size wrong.", Default: 0, Zero: "Automatic", Counts: "tokens", Least: config.Num(0), Advanced: true},
			{Key: ImageInput, Title: "Image input", About: "How the images you attach reach the model.", Default: auxiliary.ImagesAuto, Advanced: true,
				Options: config.Options(
					config.Option{Value: auxiliary.ImagesAuto, Title: "Automatic", About: "Sent as they are to a model that sees images, else described"},
					config.Option{Value: auxiliary.ImagesNative, Title: "Send as they are", About: "Sent to the model itself"},
					config.Option{Value: auxiliary.ImagesText, Title: "Describe", About: "Described in words by the image analysis model"},
				)},
			{Key: SupportsVision, Title: "Model sees images", About: "Whether the model can look at images.", Default: "", Advanced: true,
				Options: config.Options(
					config.Option{Title: "Automatic", About: "Found from what icebath knows of the model"},
					config.Option{Value: Sees, Title: "Yes"},
					config.Option{Value: Blind, Title: "No"},
				)},
			{Key: ReplayImages, Title: "Images shown again", About: "How many earlier images the model is shown again with each request.", Default: 3,
				Counts: "images", Least: config.Num(0), Advanced: true},
			{Key: APIMaxRetries, Title: "Retries", About: "How many times a failed request to the provider is tried again.", Default: 3,
				Counts: "retries", Advanced: true},
			{Key: FallbackStayPut, Title: "Rate limit wait", About: "The longest rate limit on the main model that the agent waits out.",
				Help: "A rate limit that lasts longer moves the agent to the fallback models.", Default: time.Duration(0), Zero: "Switch at once",
				Advanced: true},
			{Key: RunBudget, Title: "Reply time limit", About: "How long one reply may take before the agent is told to wrap up.",
				Help: "The agent is warned when 80% of the time has passed.", Default: time.Duration(0), Null: "no limit", Zero: "No limit",
				Apply: config.NextMessage, Advanced: true},
			{Key: BudgetWarning, Title: "Step warning point", About: "The part of the step limit at which the agent is told to save its progress.",
				Default: 0.0, Zero: "Off", Counts: config.Share, Least: config.Num(0), Most: config.Num(1), Apply: config.NextMessage, Advanced: true},
			{Key: StallGuards, Title: "Stop when stuck", About: "Stop the agent when it repeats the same calls and gets nowhere.", Default: true,
				Apply: config.NextMessage, Advanced: true},
			{Key: ToolBatchTimeout, Title: "Tool batch limit", About: "The longest a batch of tool calls that run at the same time may take.",
				Default: 7 * time.Minute, Zero: "No limit", Min: config.Bound(0), Apply: config.NextMessage, Advanced: true},
			{Key: ToolCallTimeout, Title: "Single tool limit", About: "The longest one tool call that runs on its own may take.", Default: time.Duration(0),
				Null: "the batch limit", Zero: "No limit", Min: config.Bound(0), Apply: config.NextMessage, Advanced: true},
			{Key: ClarifyTimeout, Title: "Question wait", About: "How long the agent waits when it asks you a question.", Default: time.Hour,
				Zero: "Forever", Advanced: true},
		},
	}})
	cfg.Register(config.Sections{{
		Key:    LoopKey,
		Parent: Key,
		Title:  "Tool loops",
		About:  "How the agent is warned and stopped when it loops on tools",
		Order:  20,
		Settings: []config.Setting{
			{Key: LoopWarnings, Title: "Warn the agent", About: "Warn the agent when it seems stuck calling the same tools.", Default: true,
				Apply: config.NextMessage},
			{Key: LoopHardStop, Title: "Stop the reply", About: "Stop a reply that is stuck in a tool loop.", Default: false, Apply: config.NextMessage},
			{Key: LoopUnattendedHardStop, Title: "Always stop unattended", About: "Stop tool loops in chat apps and scheduled jobs, where nobody watches.",
				Default: true, Apply: config.NextMessage},
			{Key: LoopMaxWebSearches, Title: "Searches per reply", About: "The most web searches the agent may make in one reply.", Default: 50,
				Zero: "No limit", Counts: "searches", Least: config.Num(0), Apply: config.NextMessage},
			{Key: LoopMaxSubagents, Title: "Subagents per reply", About: "The most subagents the agent may start in one reply.", Default: 50,
				Zero: "No limit", Counts: "subagents", Least: config.Num(0), Apply: config.NextMessage},
			{Key: LoopWarnExactFailure, Title: "Warn after same failure", About: "How many times the same failing call repeats before the agent is warned.",
				Default: 2, Counts: "times", Least: config.Num(1), Apply: config.NextMessage, Advanced: true},
			{Key: LoopWarnSameToolFailure, Title: "Warn after tool failures", About: "How many times one tool fails before the agent is warned.",
				Default: 3, Counts: "times", Least: config.Num(1), Apply: config.NextMessage, Advanced: true},
			{Key: LoopWarnNoProgress, Title: "Warn after no progress", About: "How many times a read-only call brings nothing new before the agent is warned.",
				Default: 2, Counts: "times", Least: config.Num(1), Apply: config.NextMessage, Advanced: true},
			{Key: LoopStopExactFailure, Title: "Stop after same failure", About: "How many times the same failing call repeats before the reply is stopped.",
				Default: 5, Counts: "times", Least: config.Num(1), Apply: config.NextMessage, Advanced: true},
			{Key: LoopStopSameToolFailure, Title: "Stop after tool failures", About: "How many times one tool fails before the reply is stopped.",
				Default: 8, Counts: "times", Least: config.Num(1), Apply: config.NextMessage, Advanced: true},
			{Key: LoopStopNoProgress, Title: "Stop after no progress", About: "How many times a read-only call brings nothing new before the reply is stopped.",
				Default: 5, Counts: "times", Least: config.Num(1), Apply: config.NextMessage, Advanced: true},
		},
	}})
	registerPrompt(cfg, personalities)
}

func registerPrompt(cfg *config.Config, personalities func() []config.Option) {
	guidance := config.Options(
		config.Option{Value: GuidanceAuto, Title: "Automatic", About: "On for the models that need it"},
		config.Option{Value: GuidanceAlways, Title: "Always", About: "On for every model"},
		config.Option{Value: GuidanceNever, Title: "Never", About: "Off for every model"},
	)
	cfg.Register(config.Sections{{
		Key:    PromptKey,
		Parent: Key,
		Title:  "Prompt",
		About:  "The instructions and advice the agent is given",
		Order:  10,
		Settings: []config.Setting{
			{Key: Personality, Title: "Personality", About: "Which personality sets the tone the agent replies in.", Default: "", Slash: "personality",
				Apply: config.NextMessage, Options: func() []config.Option {
					return append([]config.Option{{Title: "None", About: "The agent's own tone"}}, personalities()...)
				}},
			{Key: Personalities, Title: "Your personalities", About: "Personalities of your own that you can pick by name.", Default: map[string]any{},
				Entry: &config.Entry{Noun: "personality", Name: "Personality name", Fields: []config.Setting{
					{Key: "description", Title: "Description", About: "A short line shown when you pick a personality.", Default: ""},
					{Key: "system_prompt", Title: "Instructions", About: "What the agent is told to be and do.", Default: ""},
					{Key: "tone", Title: "Tone", About: "The tone the agent speaks in.", Default: ""},
					{Key: "style", Title: "Style", About: "The style the agent replies in.", Default: ""},
				}}},
			{Key: SystemPrompt, Title: "Your instructions", About: "Instructions of your own that are added to every session.",
				Help: "A personality you pick is used in place of these.", Default: "", Apply: config.NextMessage},
			{Key: CodingInstructions, Title: "Coding rules", About: "Rules of your own that the agent follows when it works on code.", Default: []string{},
				Apply: config.NewSessions},
			{Key: IgnoreRules, Title: "Skip project rules", About: "Leave project instruction files and SOUL.md out of what the agent is told.",
				Default: false, Env: []string{"ICEBATH_IGNORE_RULES"}, Apply: config.NextMessage},
			{Key: CodingContext, Title: "Coding mode", About: "Whether the agent follows coding advice when it works in a code folder.",
				Default: CodingAuto, Advanced: true, Options: config.Options(
					config.Option{Value: CodingAuto, Title: "Automatic", About: "Coding advice inside code projects"},
					config.Option{Value: CodingFocus, Title: "Focused", About: "Lean coding advice inside code projects"},
					config.Option{Value: CodingOn, Title: "Always", About: "Coding advice everywhere"},
					config.Option{Value: CodingOff, Title: "Never", About: "No coding advice"},
				)},
			{Key: ToolUseEnforcement, Title: "Tool use nudge", About: "Whether the model is pushed to use tools rather than only describe steps.",
				Default: GuidanceAuto, Options: guidance, Apply: config.NewSessions, Advanced: true},
			{Key: ExecutionGuidance, Title: "Action advice", About: "Whether the agent gets extra advice on acting and finishing work.",
				Default: GuidanceAuto, Options: guidance, Apply: config.NewSessions, Advanced: true},
			{Key: TaskCompletionGuidance, Title: "Finish tasks advice", About: "Remind the agent to finish a task before it stops.", Default: true,
				Apply: config.NewSessions, Advanced: true},
			{Key: ParallelToolCallGuidance, Title: "Parallel tools advice", About: "Tell the agent to run tool calls that don't depend on each other together.",
				Default: true, Apply: config.NewSessions, Advanced: true},
			{Key: EnvironmentProbe, Title: "Check Python setup", About: "Let the agent look at the Python tools installed before its first reply.",
				Default: true, Apply: config.NewSessions, Advanced: true},
			{Key: ContextFileMaxChars, Title: "Project file limit", About: "The most text of a project instruction file the agent is given.", Default: 0,
				Zero: "Automatic", Counts: "characters", Least: config.Num(0), Apply: config.NewSessions, Advanced: true},
			{Key: ContextFileReadTimeout, Title: "Project file wait", About: "How long icebath waits while it reads a project instruction file.",
				Default: 5 * time.Second, Zero: "5 seconds", Apply: config.NewSessions, Advanced: true},
			{Key: PlatformHints, Title: "Advice by app", About: "Changes to the formatting advice the agent gets in each app.",
				Help: "An app name such as tui, cli, cron or a chat app's name.", Default: map[string]any{}, Apply: config.NewSessions, Advanced: true,
				Entry: &config.Entry{Noun: "app", Name: "App name", Fields: []config.Setting{
					{Key: "replace", Title: "Replacement", About: "Text used in place of the built-in advice.", Default: "", Zero: "The built-in advice"},
					{Key: "append", Title: "Addition", About: "Text added after the advice.", Default: ""},
				}}},
			{Key: PassSessionID, Title: "Share session ID", About: "Tell the agent the ID of its session.", Default: false, Apply: config.NextMessage,
				Advanced: true},
			{Key: PrefillMessagesFile, Title: "Example messages file", About: "A JSON file of example messages added to the start of every session.",
				Help: "A relative path starts from the profile folder.", Default: "", Env: []string{"ICEBATH_PREFILL_MESSAGES_FILE"},
				Apply: config.NewSessions, Advanced: true},
		},
	}})
}
