package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/auxiliary"
	auxiliaryconfig "github.com/nickheyer/icebath/internal/auxiliary/config"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
)

// Choices read from the settings of the profile shown, the one a page or a write is about
type choices func(values *config.Config) func() []config.Option

// Registers the settings of the agent. Those whose choices are things of the profile, its providers and its personalities, are made for the
// profile whose settings are read, so the choices and the checks behind them are that profile's
func Register(cfg *config.Config, providers, personalities choices) {
	cfg.Register(sections{providers: providers, personalities: personalities})
}

// Sections of the agent, of its prompt and of its guards of tool loops, made for the profile of the values: those under the agent are made
// with it, since a fixed section can't sit under one that comes and goes
type sections struct {
	providers, personalities choices
}

func (s sections) Sections(values *config.Config) []config.Section {
	return []config.Section{s.agent(values), prompt(s.personalities(values)), loops()}
}

func (s sections) agent(values *config.Config) config.Section {
	providers := s.providers(values)
	return config.Section{
		Key:   Key,
		Title: "Agent",
		About: "The model that answers you and how it works",
		Group: config.GroupModels,
		Order: 20,
		Settings: []config.Setting{
			{Key: Provider, Title: "Provider", About: "The provider that new chats use.", Default: "", Picks: providerconfig.PickProvider, Custom: true,
				Apply: config.NewSessions, Options: func() []config.Option {
					return append([]config.Option{{Title: "First connected", About: "The first provider icebath finds an account or key for"}},
						providers()...)
				}},
			{Key: Model, Title: "Model", About: "The model that new chats use.", Default: "", Zero: "The provider's default", Picks: providerconfig.PickModel,
				Custom: true, Apply: config.NewSessions},
			{Key: ReasoningEffort, Title: "Reasoning effort", About: "How much the model thinks before it answers.", Default: "", Picks: auxiliaryconfig.PickEffort,
				Apply: config.NewSessions, Options: func() []config.Option { return provider.EffortOptions(config.Option{Title: "The model's default"}) }},
			{Key: Fallbacks, Title: "Fallback models", About: "Models to try in order when the main model fails.", Default: []string{}, Zero: "None",
				Picks: providerconfig.PickModel, Ordered: true, Entry: &config.Entry{Noun: "model"}},
			{Key: BusyInputMode, Title: "Enter while busy", About: "What happens to a message you send while the agent is working.", Default: BusyQueue,
				Apply: config.NewSessions, Options: BusyChoices},
			{Key: Thinking, Title: "Reasoning display", About: "How the reasoning of the model is shown in the chat.", Default: ThinkingFolded,
				Options: config.Options(
					config.Option{Value: ThinkingHidden, Title: "Hidden", About: "Not shown"},
					config.Option{Value: ThinkingFolded, Title: "Collapsed", About: "One line that you can click to open"},
					config.Option{Value: ThinkingOpen, Title: "Expanded", About: "Shown in full"},
				)},
			{Key: MaxTurns, Title: "Max steps", About: "The most model calls the agent may make for one reply.", Default: 0, Zero: "No limit", Counts: "calls",
				Apply: config.NextMessage},
			{Key: Aliases, Title: "Model shortcuts", About: "Short names you can use in place of a model's full name.",
				Help: "Type a shortcut with /model, such as /model fast.", Default: map[string]any{}, Custom: true,
				Entry: &config.Entry{Noun: "shortcut", Name: "Shortcut name", Fields: []config.Setting{
					{Key: "model", Title: "Model", About: "The model the shortcut stands for.", Default: "", Picks: providerconfig.PickModel, Need: true},
					{Key: "provider", Title: "Provider", About: "The provider that runs the model.", Default: "", Zero: "The provider of new chats",
						Picks: providerconfig.PickProvider},
					{Key: "base_url", Title: "Address", About: "The address of a server of your own that runs the model.",
						Help: "Such as http://localhost:8080/v1.", Default: "", Zero: "The provider's address", Starts: []string{"http://", "https://"},
						Advanced: true},
					{Key: "api_key", Title: "API key", About: "Your API key for that server.", Help: "Write ${NAME} to read it from a variable.",
						Default: "", Secret: true, Advanced: true},
					{Key: "key_env", Title: "Key variable", About: "The environment variable that holds the API key.", Help: "Such as MY_SERVER_KEY.",
						Default: "", Advanced: true},
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
	}
}

// Section of the guards of tool loops
func loops() config.Section {
	return config.Section{
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
	}
}

// Section of the agent's prompt, the personalities being those of the profile of the values
func prompt(personalities func() []config.Option) config.Section {
	guidance := config.Options(
		config.Option{Value: GuidanceAuto, Title: "Automatic", About: "On for the models that need it"},
		config.Option{Value: GuidanceAlways, Title: "Always", About: "On for every model"},
		config.Option{Value: GuidanceNever, Title: "Never", About: "Off for every model"},
	)
	return config.Section{
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
				Custom: true, Entry: &config.Entry{Noun: "personality", Name: "Personality name", Fields: []config.Setting{
					{Key: "description", Title: "Description", About: "A short line shown when you pick a personality.",
						Help: "Such as Calm and brief.", Default: ""},
					{Key: "system_prompt", Title: "Instructions", About: "What the agent is told to be and do.",
						Help: "Such as You are a calm assistant who answers in few words.", Default: ""},
					{Key: "tone", Title: "Tone", About: "The tone the agent speaks in.", Help: "Such as warm and patient.", Default: ""},
					{Key: "style", Title: "Style", About: "The style the agent replies in.", Help: "Such as short lists, no jokes.", Default: ""},
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
				Default: map[string]any{}, Apply: config.NewSessions, Advanced: true,
				Entry: &config.Entry{Noun: "app", Names: Apps, Fields: []config.Setting{
					{Key: "replace", Title: "Replacement", About: "Text used in place of the built-in advice.", Default: "", Zero: "The built-in advice"},
					{Key: "append", Title: "Addition", About: "Text added after the advice.", Default: ""},
				}}},
			{Key: PassSessionID, Title: "Share session ID", About: "Tell the agent the ID of its session.", Default: false, Apply: config.NextMessage,
				Advanced: true},
			{Key: PrefillMessagesFile, Title: "Example messages file", About: "A JSON file of example messages added to the start of every session.",
				Default: "", Env: []string{"ICEBATH_PREFILL_MESSAGES_FILE"}, Apply: config.NewSessions, Advanced: true,
				Path: &config.Path{Kind: config.PathFile, Exists: true}},
		},
	}
}

// Apps icebath runs sessions in, by the name a session's app has, with what a person calls each: those the agent gets formatting advice
// for, then the chat apps
func Apps() []config.Option {
	return []config.Option{
		{Value: "tui", Title: "This app", About: "The terminal app"},
		{Value: "cli", Title: "Command line", About: "icebath chat in a plain terminal"},
		{Value: "cron", Title: "Scheduled jobs", About: "Jobs that run on a schedule, with nobody watching"},
		{Value: "api_server", Title: "API server", About: "Apps that reach icebath through its API"},
		{Value: "mcp", Title: "MCP clients", About: "Programs that use icebath as an MCP server"},
		{Value: "telegram", Title: "Telegram"},
		{Value: "discord", Title: "Discord"},
		{Value: "matrix", Title: "Matrix"},
		{Value: "whatsapp", Title: "WhatsApp"},
		{Value: "signal", Title: "Signal"},
		{Value: "feishu", Title: "Feishu"},
	}
}

// What happens to a message you send while a reply is being written: each choice of Enter while busy
func BusyChoices() []config.Option {
	return []config.Option{
		{Value: BusyQueue, Title: "Sent after the reply", About: "Your message waits for the current reply to end"},
		{Value: BusySteer, Title: "Added to the reply", About: "The agent reads it at the next step of the current reply"},
		{Value: BusyInterrupt, Title: "Sent at once", About: "The current reply stops and your message goes now"},
	}
}

// Title of a choice of Enter while busy, every page, note and command saying it so; the first one's for a value no choice has
func BusyTitle(mode string) string {
	choices := BusyChoices()
	for _, choice := range choices {
		if choice.Value == mode {
			return choice.Title
		}
	}
	return choices[0].Title
}
