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
		About: "Model, reasoning and how turns run",
		Settings: []config.Setting{
			{Key: Provider, Title: "Provider", About: "Provider of this profile new sessions use", Default: "", Options: func() []config.Option {
				return append([]config.Option{{Title: "First connected"}}, providers()...)
			}},
			{Key: Model, Title: "Model", About: "Model for new sessions; empty uses the provider's default", Default: ""},
			{Key: Aliases, Title: "Model shortcuts", About: "Short names for models: each gives a model and its provider. A name holds model, provider (the " +
				"profile's provider when unset, else custom), and for an endpoint of its own base_url with api_key (a key, or ${VAR}) or key_env. The " +
				"model setting, /model and a profile's subagents take a shortcut's name, and /model one of their models", Default: map[string]any{}},
			{Key: ReasoningEffort, Title: "Reasoning effort", About: "How much the model reasons before answering", Default: "", Options: func() []config.Option {
				options := []config.Option{{Title: "Model default"}}
				for _, effort := range provider.Efforts {
					options = append(options, config.Option{Value: effort, Title: effort})
				}
				return options
			}},
			{Key: Thinking, Title: "Reasoning display", About: "How model reasoning is shown in chats", Default: ThinkingFolded, Options: config.Options(
				config.Option{Value: ThinkingHidden, Title: "Hidden"},
				config.Option{Value: ThinkingFolded, Title: "Collapsed", About: "One line, click to expand"},
				config.Option{Value: ThinkingOpen, Title: "Expanded"},
			)},
			{Key: ImageInput, Title: "Image input", About: "How pictures reach the model", Default: auxiliary.ImagesAuto, Options: config.Options(
				config.Option{Value: auxiliary.ImagesAuto, Title: "Auto", About: "As they are when the model sees images and no vision model is set, else described"},
				config.Option{Value: auxiliary.ImagesNative, Title: "Native", About: "As they are, to the model itself"},
				config.Option{Value: auxiliary.ImagesText, Title: "Text", About: "Described by the vision helper model"},
			)},
			{Key: SupportsVision, Title: "Model sees images", About: "Whether the main model can look at pictures", Default: "", Options: config.Options(
				config.Option{Title: "Detected from the model"},
				config.Option{Value: Sees, Title: "Yes"},
				config.Option{Value: Blind, Title: "No"},
			)},
			{Key: ReplayImages, Title: "Replayed images", About: "How many earlier images are shown to the model again; 0 stops it", Default: 3},
			{Key: ClarifyTimeout, Title: "Question timeout", About: "Seconds the agent waits for your answers to its questions; 0 waits as long as it takes", Default: time.Hour},
			{Key: MaxTurns, Title: "Max steps", About: "Max model calls per reply; 0 for no limit", Default: 0},
			{Key: APIMaxRetries, Title: "Retries", About: "Times to retry a failed provider request", Default: 3},
			{Key: ContextLength, Title: "Context window", About: "How many tokens the main model can hold, when icebath guesses wrong; 0 uses the size icebath knows", Default: 0},
			{Key: Fallbacks, Title: "Fallback models", About: "Models to try, in order, when the main one fails, each provider:model with the provider named as in this profile", Default: []string{}},
			{Key: BusyInputMode, Title: "Enter while busy", About: "What a message sent while the agent is still working does", Default: BusyQueue,
				Options: config.Options(
					config.Option{Value: BusyQueue, Title: "Queue it", About: "Send after the current turn ends"},
					config.Option{Value: BusySteer, Title: "Steer", About: "Add it to the current turn"},
					config.Option{Value: BusyInterrupt, Title: "Interrupt", About: "Stop the turn and send it now"},
				)},
			{Key: FallbackStayPut, Title: "Stay-put window", About: "Seconds: stay on the main model if its rate limit ends within this time; 0 switches to the fallback on any rate limit",
				Default: time.Duration(0)},
			{Key: RunBudget, Title: "Run time limit", About: "Seconds one reply may take before the agent is told to wrap up, at 80% of them; 0 for no limit", Default: time.Duration(0),
				Null: "no limit"},
			{Key: BudgetWarning, Title: "Budget warning point", About: "Share of the step limit, such as 0.8, at which the agent is told to checkpoint its progress; 0 for no warning", Default: 0.0},
			{Key: StallGuards, Title: "Stall guards", About: "Stops the agent when it repeats the same calls without progress", Default: true},
			{Key: ToolBatchTimeout, Title: "Tool batch limit", About: "Longest a batch of tool calls run at the same time may take, in seconds; 0 for no limit", Default: 7 * time.Minute,
				Min: config.Bound(0)},
			{Key: ToolCallTimeout, Title: "Single tool limit", About: "Longest one tool call run on its own may take, in seconds; empty uses the tool batch limit, 0 for no limit",
				Default: time.Duration(0), Null: "the tool batch limit", Min: config.Bound(0)},
		},
	}})
	cfg.Register(config.Sections{{
		Key:    LoopKey,
		Parent: Key,
		Title:  "Tool loops",
		About:  "How the agent is warned, then stopped, when it calls tools in a loop",
		Settings: []config.Setting{
			{Key: LoopWarnings, Title: "Loop warnings", About: "Warns the agent when it seems stuck in a tool loop", Default: true},
			{Key: LoopHardStop, Title: "Stop looping tools", About: "Stops a turn that is stuck in a tool loop", Default: false},
			{Key: LoopUnattendedHardStop, Title: "Stop loops unattended", About: "Always stops tool loops in chats and scheduled runs", Default: true},
			{Key: LoopWarnExactFailure, Title: "Warn: same failure", About: "Warns the agent after the same failing call repeats this often", Default: 2},
			{Key: LoopWarnSameToolFailure, Title: "Warn: tool failures", About: "Warns the agent after one tool keeps failing this often", Default: 3},
			{Key: LoopWarnNoProgress, Title: "Warn: no progress", About: "Warns after a read-only call repeats with no new result", Default: 2},
			{Key: LoopStopExactFailure, Title: "Stop: same failure", About: "Stops after the same failing call repeats this many times", Default: 5},
			{Key: LoopStopSameToolFailure, Title: "Stop: tool failures", About: "Stops after one tool keeps failing this many times", Default: 8},
			{Key: LoopStopNoProgress, Title: "Stop: no progress", About: "Stops after a read-only call repeats with no new result", Default: 5},
			{Key: LoopMaxWebSearches, Title: "Searches per turn", About: "Most web searches one turn may make; 0 for no limit", Default: 50},
			{Key: LoopMaxSubagents, Title: "Subagents per turn", About: "Most subagents one turn may start; 0 for no limit", Default: 50},
		},
	}})
	registerPrompt(cfg, personalities)
}

func registerPrompt(cfg *config.Config, personalities func() []config.Option) {
	guidance := config.Options(
		config.Option{Value: GuidanceAuto, Title: "Auto", About: "On for model families that need it"},
		config.Option{Value: GuidanceAlways, Title: "Always", About: "Always on"},
		config.Option{Value: GuidanceNever, Title: "Never", About: "Always off"},
	)
	cfg.Register(config.Sections{{
		Key:    PromptKey,
		Parent: Key,
		Title:  "Prompt",
		About:  "What the agent is told before a chat: identity, guidance, project files",
		Settings: []config.Setting{
			{Key: Personality, Title: "Personality", About: "Tone and style preset the agent speaks with, for every session of this profile", Default: "", Options: func() []config.Option {
				return append([]config.Option{{Title: "None"}}, personalities()...)
			}},
			{Key: Personalities, Title: "Personalities", About: "Custom personalities you can switch to, by name: the instructions, or system_prompt, tone, style and description", Default: map[string]any{}},
			{Key: SystemPrompt, Title: "Own instructions", About: "Your own instructions added to every conversation; a chosen personality takes their place", Default: ""},
			{Key: CodingContext, Title: "Coding mode", About: "Whether the agent switches to coding guidance in code folders", Default: CodingAuto, Options: config.Options(
				config.Option{Value: CodingAuto, Title: "Auto", About: "Use coding guidance inside code projects"},
				config.Option{Value: CodingFocus, Title: "Focus", About: "Lean coding guidance in code projects"},
				config.Option{Value: CodingOn, Title: "Always", About: "Always use coding guidance"},
				config.Option{Value: CodingOff, Title: "Never", About: "Never use coding guidance"},
			)},
			{Key: CodingInstructions, Title: "Coding instructions", About: "Your own rules the agent follows when it works on code", Default: []string{}},
			{Key: ContextFileMaxChars, Title: "Context file limit", About: "Longest project instruction file text sent, in characters; 0 sizes it to the model", Default: 0},
			{Key: ContextFileReadTimeout, Title: "Context file wait", About: "Seconds to wait while reading a project instruction file", Default: 5 * time.Second},
			{Key: IgnoreRules, Title: "Ignore rules", About: "Leave project instruction files and SOUL.md out of new sessions", Default: false, Env: []string{"ICEBATH_IGNORE_RULES"}},
			{Key: TaskCompletionGuidance, Title: "Finish tasks advice", About: "Reminds the agent to finish a task before it stops", Default: true},
			{Key: ParallelToolCallGuidance, Title: "Parallel tool advice", About: "Tells the agent to run independent tool calls together", Default: true},
			{Key: ToolUseEnforcement, Title: "Tool use nudge", About: "Pushes the model to call tools instead of only describing steps", Default: GuidanceAuto, Options: guidance},
			{Key: ExecutionGuidance, Title: "Execution guidance", About: "Adds extra advice on acting and finishing work", Default: GuidanceAuto, Options: guidance},
			{Key: EnvironmentProbe, Title: "Check environment", About: "Lets the agent look at the installed Python toolchain before its first turn", Default: true},
			{Key: PlatformHints, Title: "Surface hints", About: "By surface, such as tui: text that replaces the agent's formatting hint, or a table of replace and append", Default: map[string]any{}},
			{Key: PassSessionID, Title: "Session ID in prompt", About: "Tells the agent the id of its session", Default: false},
			{Key: PrefillMessagesFile, Title: "Prefill messages file", About: "JSON file of example messages added to every conversation; a relative path starts from the profile folder", Default: "", Env: []string{"ICEBATH_PREFILL_MESSAGES_FILE"}},
		},
	}})
}
