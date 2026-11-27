package config

const (
	Key                 = "agent"
	Provider            = Key + ".provider"
	Model               = Key + ".model"
	ReasoningEffort     = Key + ".reasoning_effort"
	MaxTurns            = Key + ".max_turns"
	APIMaxRetries       = Key + ".api_max_retries"
	ContextFileMaxChars = Key + ".prompt.context_file_max_chars"
	Thinking            = Key + ".thinking"
	ThinkingHidden      = "hidden"
	ThinkingFolded      = "folded"
	ThinkingOpen        = "open"
	ClarifyTimeout      = Key + ".clarify_timeout"
	ImageInput          = Key + ".image_input_mode"
	SupportsVision      = Key + ".supports_vision"
	Sees                = "true"
	Blind               = "false"
	ReplayImages        = Key + ".replay_recent_images"
	// Models tried in turn when the main one fails, each provider:model with the provider named as in the profile
	Fallbacks = Key + ".fallback_providers"
	// Seconds within which a rate limited main model's limit must end for the session to stay on it rather than fall back
	FallbackStayPut = Key + ".fallback_min_switch_reset_seconds"
	// What a message sent while the agent still works does
	BusyInputMode = Key + ".busy_input_mode"
	// Longest one run may take before the agent is told to wrap up, in seconds
	RunBudget = Key + ".run_budget_seconds"
	// Share of the step limit at which the agent is told to checkpoint its progress
	BudgetWarning = Key + ".budget_warning_ratio"
	// Longest a batch of tool calls run together, and one call run alone, may take, in seconds
	ToolBatchTimeout = Key + ".tool_timeouts.concurrent_batch"
	ToolCallTimeout  = Key + ".tool_timeouts.sequential_call"
	// Stops the agent when it repeats the same calls without progress
	StallGuards = Key + ".stall_guards"
	// Tokens the main model takes in at once, when what icebath knows of it is wrong
	ContextLength = Key + ".context_length"
	// Short names for models, each giving a model and its provider
	Aliases = Key + ".aliases"
)

// Guardrails of tool loops
const (
	LoopKey                 = Key + ".tool_loop_guardrails"
	LoopWarnings            = LoopKey + ".warnings_enabled"
	LoopHardStop            = LoopKey + ".hard_stop_enabled"
	LoopUnattendedHardStop  = LoopKey + ".non_interactive_hard_stop_enabled"
	LoopWarnExactFailure    = LoopKey + ".warn_after.exact_failure"
	LoopWarnSameToolFailure = LoopKey + ".warn_after.same_tool_failure"
	LoopWarnNoProgress      = LoopKey + ".warn_after.idempotent_no_progress"
	LoopStopExactFailure    = LoopKey + ".hard_stop_after.exact_failure"
	LoopStopSameToolFailure = LoopKey + ".hard_stop_after.same_tool_failure"
	LoopStopNoProgress      = LoopKey + ".hard_stop_after.idempotent_no_progress"
	LoopMaxWebSearches      = LoopKey + ".loop_caps.max_web_searches"
	LoopMaxSubagents        = LoopKey + ".loop_caps.max_subagents"
)

const (
	PromptKey                = Key + ".prompt"
	Personality              = PromptKey + ".personality"
	Personalities            = PromptKey + ".personalities"
	SystemPrompt             = PromptKey + ".system_prompt"
	CodingContext            = PromptKey + ".coding_context"
	CodingInstructions       = PromptKey + ".coding_instructions"
	ContextFileReadTimeout   = PromptKey + ".context_file_read_timeout"
	IgnoreRules              = PromptKey + ".ignore_rules"
	TaskCompletionGuidance   = PromptKey + ".task_completion_guidance"
	ParallelToolCallGuidance = PromptKey + ".parallel_tool_call_guidance"
	ToolUseEnforcement       = PromptKey + ".tool_use_enforcement"
	ExecutionGuidance        = PromptKey + ".execution_guidance"
	EnvironmentProbe         = PromptKey + ".environment_probe"
	PlatformHints            = PromptKey + ".platform_hints"
	PassSessionID            = PromptKey + ".pass_session_id"
	PrefillMessagesFile      = PromptKey + ".prefill_messages_file"
)

// Values of coding_context
const (
	CodingAuto  = "auto"
	CodingFocus = "focus"
	CodingOn    = "on"
	CodingOff   = "off"
)

// Values of the guidance that turns itself on for the model families that need it
const (
	GuidanceAuto   = "auto"
	GuidanceAlways = "always"
	GuidanceNever  = "never"
)

// Values of busy_input_mode
const (
	BusyQueue     = "queue"
	BusySteer     = "steer"
	BusyInterrupt = "interrupt"
)
