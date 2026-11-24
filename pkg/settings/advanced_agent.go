package settings

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// Tells the agent how to behave when relayed through a bot
var AgentBotModeProtocol = Register(&Bool{Meta: Meta{Key: "agent.bot_mode_protocol", Label: "Bot mode rules", Short: "Tells the agent how to behave when relayed through a bot.", Group: GroupAgent, Order: 1000, Advanced: true, Apply: NewSession}, Default: true})

// Warns the agent when this share of its turns is used
var AgentBudgetWarningRatio = Register(&Float{Meta: Meta{Key: "agent.budget_warning_ratio", Label: "Budget warning point", Short: "Warns the agent when this share of its turns is used.", Help: "A number between 0 and 1, such as 0.8 for 80 percent. Other values turn the warning off.", Group: GroupAgent, Order: 1010, Advanced: true, Apply: NewSession}, Null: "no warning"})

// Whether the agent switches to coding guidance in code folders
var AgentCodingContext = Register(&Enum{Meta: Meta{Key: "agent.coding_context", Label: "Coding mode", Short: "Whether the agent switches to coding guidance in code folders.", Group: GroupAgent, Order: 1020, Advanced: true, Apply: NewSession}, Default: "auto", Values: []Choice{{Value: "auto", Label: "Auto", Meaning: "Use coding guidance inside code projects."}, {Value: "focus", Label: "Focus", Meaning: "Lean coding guidance in code projects."}, {Value: "on", Label: "Always", Meaning: "Always use coding guidance."}, {Value: "off", Label: "Never", Meaning: "Never use coding guidance."}},
	Also: map[string]string{"strict": "focus", "lean": "focus", "true": "on", "yes": "on", "1": "on", "always": "on", "false": "off", "no": "off", "0": "off", "never": "off"}})

// Spend in dollars above which empty replies are not retried
var AgentEmptyResponseGuardCostThresholdUsd = Register(&Float{Meta: Meta{Key: "agent.empty_response_guard.cost_threshold_usd", Label: "Retry cost limit", Short: "Spend in dollars above which empty replies are not retried.", Help: "Must be above zero or the built-in value is used.", Group: GroupAgent, Order: 1030, Advanced: true, Apply: NewSession}, Default: 0.25})

// Retries when the model sends back an empty reply
var AgentEmptyResponseGuardEnabled = Register(&Bool{Meta: Meta{Key: "agent.empty_response_guard.enabled", Label: "Empty reply guard", Short: "Retries when the model sends back an empty reply.", Group: GroupAgent, Order: 1040, Advanced: true, Apply: NewSession}, Default: true})

// Extra text describing your machine, added to the agent's prompt
var AgentEnvironmentHint = Register(&String{Meta: Meta{Key: "agent.environment_hint", Label: "Environment note", Short: "Extra text describing your machine, added to the agent's prompt.", Group: GroupAgent, Order: 1050, Advanced: true, Apply: NewSession}, Default: ""})

// Lets the agent look at installed tools before its first turn
var AgentEnvironmentProbe = Register(&Bool{Meta: Meta{Key: "agent.environment_probe", Label: "Check environment", Short: "Lets the agent look at installed tools before its first turn.", Group: GroupAgent, Order: 1060, Advanced: true, Apply: NewSession}, Default: true})

// Adds extra advice on acting and finishing work to the prompt
var AgentExecutionGuidance = Register(&Enum{Meta: Meta{Key: "agent.execution_guidance", Label: "Execution guidance", Short: "Adds extra advice on acting and finishing work to the prompt.", Help: "Auto turns it on for model families that need it.", Group: GroupAgent, Order: 1070, Advanced: true, Apply: NewSession}, Default: "auto", Values: []Choice{{Value: "auto", Label: "Auto", Meaning: "On for model families that need it."}, {Value: "always", Label: "Always", Meaning: "Always on."}, {Value: "never", Label: "Never", Meaning: "Always off."}}})

// Keeps going when the model only says what it will do
var AgentIntentAckContinuation = Register(&Enum{Meta: Meta{Key: "agent.intent_ack_continuation", Label: "Continue after intent", Short: "Keeps going when the model only says what it will do.", Help: "Auto turns it on only for Codex style models.", Group: GroupAgent, Order: 1080, Advanced: true, Apply: NewSession}, Default: "auto", Values: []Choice{{Value: "auto", Label: "Auto", Meaning: "On only for Codex style models."}, {Value: "always", Label: "Always", Meaning: "Always on."}, {Value: "never", Label: "Never", Meaning: "Always off."}}})

// Most times per turn the agent is told to verify its work
var AgentMaxVerifyNudges = Register(&Int{Meta: Meta{Key: "agent.max_verify_nudges", Label: "Verify reminders", Short: "Most times per turn the agent is told to verify its work.", Help: "Zero turns the reminders off.", Group: GroupAgent, Order: 1090, Advanced: true}, Default: 3})

// Tells the agent to run independent tool calls together
var AgentParallelToolCallGuidance = Register(&Bool{Meta: Meta{Key: "agent.parallel_tool_call_guidance", Label: "Parallel tool advice", Short: "Tells the agent to run independent tool calls together.", Group: GroupAgent, Order: 1100, Advanced: true, Apply: NewSession}, Default: true})

// Longest time one run may take before the agent is told to wrap up
var AgentRunBudgetSeconds = Register(&Duration{Meta: Meta{Key: "agent.run_budget_seconds", Label: "Run time limit", Short: "Longest time one run may take before the agent is told to wrap up.", Group: GroupAgent, Order: 1110, Advanced: true, Apply: NewSession}, Null: "no limit"})

// Stops the agent when it repeats the same calls without progress
var AgentStallGuards = Register(&Bool{Meta: Meta{Key: "agent.stall_guards", Label: "Stall guards", Short: "Stops the agent when it repeats the same calls without progress.", Group: GroupAgent, Order: 1120, Advanced: true, Apply: NewSession}, Default: true})

// Reminds the agent to finish a task before it stops
var AgentTaskCompletionGuidance = Register(&Bool{Meta: Meta{Key: "agent.task_completion_guidance", Label: "Finish tasks advice", Short: "Reminds the agent to finish a task before it stops.", Group: GroupAgent, Order: 1130, Advanced: true, Apply: NewSession}, Default: true})

// Pushes the model to call tools instead of only describing steps
var AgentToolUseEnforcement = Register(&Enum{Meta: Meta{Key: "agent.tool_use_enforcement", Label: "Tool use nudge", Short: "Pushes the model to call tools instead of only describing steps.", Help: "Auto turns it on for model families that need it.", Group: GroupAgent, Order: 1140, Advanced: true, Apply: NewSession}, Default: "auto", Values: []Choice{{Value: "auto", Label: "Auto", Meaning: "On for model families that need it."}, {Value: "always", Label: "Always", Meaning: "Always on."}, {Value: "never", Label: "Never", Meaning: "Always off."}}})

// Adds how-to-verify advice to the reminders sent before stopping
var AgentVerifyGuidance = Register(&Bool{Meta: Meta{Key: "agent.verify_guidance", Label: "Verify advice", Short: "Adds how-to-verify advice to the reminders sent before stopping.", Group: GroupAgent, Order: 1150, Advanced: true}, Default: true})

// Asks the agent to check its edits before it finishes
var AgentVerifyOnStop = Register(&Enum{Meta: Meta{Key: "agent.verify_on_stop", Label: "Verify before stop", Short: "Asks the agent to check its edits before it finishes.", Group: GroupAgent, Order: 1160, Advanced: true}, Default: "off", Off: "off", On: "on", Values: []Choice{{Value: "auto", Label: "Auto", Meaning: "Check edits everywhere but in chat apps."}, {Value: "on", Label: "Always", Meaning: "Always check edits before finishing."}, {Value: "off", Label: "Never", Meaning: "Never ask for a check."}}})

// How long a queued bot message waits for the running turn
var BotModeTurnWaitSeconds = Register(&Duration{Meta: Meta{Key: "bot_mode.turn_wait_seconds", Label: "Bot turn wait", Short: "How long a queued bot message waits for the running turn.", Group: GroupAgent, Order: 1170, Advanced: true}, Default: 2 * time.Minute})

// Stops the turn if the summary of old messages fails
var CompressionAbortOnSummaryFailure = Register(&Bool{Meta: Meta{Key: "compression.abort_on_summary_failure", Label: "Stop on failure", Short: "Stops the turn if the summary of old messages fails.", Group: GroupAgent, Order: 1180, Advanced: true}, Default: false})

// Refuses to compress until a checkpoint of the chat is saved
var CompressionCheckpointRequired = Register(&Bool{Meta: Meta{Key: "compression.checkpoint_required", Label: "Require checkpoint", Short: "Refuses to compress until a checkpoint of the chat is saved.", Help: "Not allowed with the Codex app server mode.", Group: GroupAgent, Order: 1190, Advanced: true}, Default: false})

// Who compacts a long Codex app server conversation
var CompressionCodexAppServerAuto = Register(&Enum{Meta: Meta{Key: "compression.codex_app_server_auto", Label: "Codex compaction", Short: "Who compacts a long Codex app server conversation.", Group: GroupAgent, Order: 1200, Advanced: true}, Default: "native", Values: []Choice{{Value: "native", Label: "Codex", Meaning: "Codex compacts its own thread."}, {Value: "icebath", Label: "Icebath", Meaning: "Icebath summarises the conversation."}, {Value: "off", Label: "Off", Meaning: "No automatic compaction."}}})

// Compresses later for the newest Codex models
var CompressionCodexGpt55Autoraise = Register(&Bool{Meta: Meta{Key: "compression.codex_gpt55_autoraise", Label: "Raise Codex trigger", Short: "Compresses later for the newest Codex models.", Group: GroupAgent, Order: 1210, Advanced: true}, Default: true})

// Shows a note when the Codex compression trigger is raised
var CompressionCodexGpt55AutoraiseNotice = Register(&Bool{Meta: Meta{Key: "compression.codex_gpt55_autoraise_notice", Label: "Codex raise notice", Short: "Shows a note when the Codex compression trigger is raised.", Group: GroupAgent, Order: 1220, Advanced: true}, Default: true})

// Size in tokens at which native Codex compaction starts
var CompressionCodexResponsesCompactThreshold = Register(&Int{Meta: Meta{Key: "compression.codex_responses_compact_threshold", Label: "Codex compact size", Short: "Size in tokens at which native Codex compaction starts.", Help: "Only used when native Codex compaction is on. Must be above zero.", Group: GroupAgent, Order: 1230, Advanced: true}, Null: "chosen automatically", Unit: "tokens"})

// Lets Codex compact long conversations itself
var CompressionCodexResponsesNative = Register(&Bool{Meta: Meta{Key: "compression.codex_responses_native", Label: "Native Codex compaction", Short: "Lets Codex compact long conversations itself.", Group: GroupAgent, Order: 1240, Advanced: true}, Default: false})

// Longest the summary step may stay silent before it is given up
var CompressionContextTimeoutSeconds = Register(&Duration{Meta: Meta{Key: "compression.context_timeout_seconds", Label: "Summary idle limit", Short: "Longest the summary step may stay silent before it is given up.", Help: "Raised automatically to fit the summary model's own timeout.", Group: GroupAgent, Order: 1250, Advanced: true}, Default: 2 * time.Minute})

// Longest a whole summary step may take
var CompressionContextTotalCeilingSeconds = Register(&Duration{Meta: Meta{Key: "compression.context_total_ceiling_seconds", Label: "Summary time limit", Short: "Longest a whole summary step may take.", Group: GroupAgent, Order: 1260, Advanced: true}, Default: 10 * time.Minute})

// Wait before retrying a failed automatic cleanup of a long chat
var CompressionHygieneFailureCooldownSeconds = Register(&Duration{Meta: Meta{Key: "compression.hygiene_failure_cooldown_seconds", Label: "Cleanup retry pause", Short: "Wait before retrying a failed automatic cleanup of a long chat.", Group: GroupAgent, Order: 1270, Advanced: true}, Default: 5 * time.Minute})

// Message count that forces a cleanup of a long chat
var CompressionHygieneHardMessageLimit = Register(&Int{Meta: Meta{Key: "compression.hygiene_hard_message_limit", Label: "Cleanup message limit", Short: "Message count that forces a cleanup of a long chat.", Group: GroupAgent, Order: 1280, Advanced: true}, Default: 5000, Min: at(1), Unit: "messages", DefaultBelow: true})

// Longest a new message waits while a chat is being cleaned up
var CompressionHygieneMaxTurnHoldSeconds = Register(&Duration{Meta: Meta{Key: "compression.hygiene_max_turn_hold_seconds", Label: "Cleanup hold time", Short: "Longest a new message waits while a chat is being cleaned up.", Group: GroupAgent, Order: 1290, Advanced: true}, Default: 10 * time.Second})

// Longest an automatic cleanup may stay silent before it fails
var CompressionHygieneTimeoutSeconds = Register(&Duration{Meta: Meta{Key: "compression.hygiene_timeout_seconds", Label: "Cleanup idle limit", Short: "Longest an automatic cleanup may stay silent before it fails.", Group: GroupAgent, Order: 1300, Advanced: true}, Default: 30 * time.Second})

// Longest a whole automatic cleanup may take
var CompressionHygieneTotalCeilingSeconds = Register(&Duration{Meta: Meta{Key: "compression.hygiene_total_ceiling_seconds", Label: "Cleanup time limit", Short: "Longest a whole automatic cleanup may take.", Group: GroupAgent, Order: 1310, Advanced: true}, Default: 10 * time.Minute})

// Compresses a chat after it has sat idle this long
var CompressionIdleCompactAfterSeconds = Register(&Duration{Meta: Meta{Key: "compression.idle_compact_after_seconds", Label: "Compact when idle", Short: "Compresses a chat after it has sat idle this long.", Help: "Zero turns it off.", Group: GroupAgent, Order: 1320, Advanced: true}, Default: 0, Min: at(time.Duration(0))})

// Keeps the same session when old messages are summarised
var CompressionInPlace = Register(&Bool{Meta: Meta{Key: "compression.in_place", Label: "Compress in place", Short: "Keeps the same session when old messages are summarised.", Group: GroupAgent, Order: 1330, Advanced: true}, Default: true})

// How many times a failed summary is retried
var CompressionMaxAttempts = Register(&Int{Meta: Meta{Key: "compression.max_attempts", Label: "Summary attempts", Short: "How many times a failed summary is retried.", Help: "Between 1 and 10.", Group: GroupAgent, Order: 1340, Advanced: true}, Default: 3, Min: at(1), Max: at(10), DefaultBelow: true})

// Trims old tool output a little at a time instead of all at once
var CompressionMicroCompact = Register(&Bool{Meta: Meta{Key: "compression.micro_compact", Label: "Micro compaction", Short: "Trims old tool output a little at a time instead of all at once.", Group: GroupAgent, Order: 1350, Advanced: true}, Default: false})

// Size in tokens that triggers a repack of trimmed output
var CompressionMicroCompactDefragThresholdTokens = Register(&Int{Meta: Meta{Key: "compression.micro_compact_defrag_threshold_tokens", Label: "Micro compact repack", Short: "Size in tokens that triggers a repack of trimmed output.", Group: GroupAgent, Order: 1360, Advanced: true}, Default: 2000, Min: at(1), Unit: "tokens"})

// Trims old tool output once every this many turns
var CompressionMicroCompactEveryNTurns = Register(&Int{Meta: Meta{Key: "compression.micro_compact_every_n_turns", Label: "Micro compact pace", Short: "Trims old tool output once every this many turns.", Group: GroupAgent, Order: 1370, Advanced: true}, Default: 1, Min: at(1), Unit: "turns"})

// Fewest of your latest messages always kept as written
var CompressionMinTailUserMessages = Register(&Int{Meta: Meta{Key: "compression.min_tail_user_messages", Label: "Keep user messages", Short: "Fewest of your latest messages always kept as written.", Group: GroupAgent, Order: 1380, Advanced: true}, Default: 1, Min: at(1)})

// Compression trigger share for chosen models, by model name
var CompressionModelThresholds = Register(&Map{Meta: Meta{Key: "compression.model_thresholds", Label: "Per model trigger", Short: "Compression trigger share for chosen models, by model name.", Help: "Each value is a fraction of the model's context window, such as 0.6.", Group: GroupAgent, Order: 1390, Advanced: true}})

// Prepares the summary before the trigger is reached
var CompressionPrepareAhead = Register(&Bool{Meta: Meta{Key: "compression.prepare_ahead", Label: "Summarise ahead", Short: "Prepares the summary before the trigger is reached.", Group: GroupAgent, Order: 1400, Advanced: true}, Default: false})

// Least tokens a trim must free to be worth doing
var CompressionProactivePruneMinReclaimTokens = Register(&Int{Meta: Meta{Key: "compression.proactive_prune_min_reclaim_tokens", Label: "Prune minimum gain", Short: "Least tokens a trim must free to be worth doing.", Group: GroupAgent, Order: 1410, Advanced: true}, Default: 4096, Min: at(0), Unit: "tokens"})

// Tool output shorter than this is never trimmed
var CompressionProactivePruneMinResultChars = Register(&Int{Meta: Meta{Key: "compression.proactive_prune_min_result_chars", Label: "Prune result size", Short: "Tool output shorter than this is never trimmed.", Group: GroupAgent, Order: 1420, Advanced: true}, Default: 8000, Min: at(0), Unit: "chars"})

// Token size at which old tool output is trimmed early
var CompressionProactivePruneTokens = Register(&Int{Meta: Meta{Key: "compression.proactive_prune_tokens", Label: "Early trim size", Short: "Token size at which old tool output is trimmed early.", Help: "Zero turns early trimming off.", Group: GroupAgent, Order: 1430, Advanced: true}, Default: 0, Min: at(0), Unit: "tokens"})

// Shows status messages while a chat is being compressed
var CompressionProgressNotices = Register(&Bool{Meta: Meta{Key: "compression.progress_notices", Label: "Compression notices", Short: "Shows status messages while a chat is being compressed.", Group: GroupAgent, Order: 1440, Advanced: true}, Default: false})

// Number of opening messages never summarised
var CompressionProtectFirstN = Register(&Int{Meta: Meta{Key: "compression.protect_first_n", Label: "Keep first messages", Short: "Number of opening messages never summarised.", Group: GroupAgent, Order: 1450, Advanced: true}, Default: 3, Min: at(0), Unit: "messages"})

// Number of latest messages never summarised
var CompressionProtectLastN = Register(&Int{Meta: Meta{Key: "compression.protect_last_n", Label: "Keep last messages", Short: "Number of latest messages never summarised.", Group: GroupAgent, Order: 1460, Advanced: true}, Default: 20, Min: at(0), Unit: "messages"})

// How recent messages are kept when compressing
var CompressionTailMode = Register(&Enum{Meta: Meta{Key: "compression.tail_mode", Label: "Recent history mode", Short: "How recent messages are kept when compressing.", Group: GroupAgent, Order: 1470, Advanced: true}, Default: "lean", Values: []Choice{{Value: "lean", Label: "Lean", Meaning: "Keep a trimmed recent tail."}, {Value: "legacy", Label: "Legacy", Meaning: "Keep the older fixed-size tail."}}})

// Share of the trigger size the summary is squeezed to
var CompressionTargetRatio = Register(&Float{Meta: Meta{Key: "compression.target_ratio", Label: "Summary size", Short: "Share of the trigger size the summary is squeezed to.", Help: "Held between 0.1 and 0.8.", Group: GroupAgent, Order: 1480, Advanced: true}, Default: 0.2, Min: at(0.1), Max: at(0.8)})

// Hard cap in tokens that triggers compression
var CompressionThresholdTokens = Register(&Int{Meta: Meta{Key: "compression.threshold_tokens", Label: "Trigger token cap", Short: "Hard cap in tokens that triggers compression.", Group: GroupAgent, Order: 1490, Advanced: true}, Null: "share of context window", Unit: "tokens"})

// Which engine manages long conversations
var ContextEngine = Register(&String{Meta: Meta{Key: "context.engine", Label: "Context engine", Short: "Which engine manages long conversations.", Help: "Compressor is the built-in one. Plugins can add others.", Group: GroupAgent, Order: 1500, Advanced: true, Apply: NewSession}, Default: "compressor"})

// Longest project instruction file text sent, in characters
var ContextFileMaxChars = Register(&Int{Meta: Meta{Key: "context_file_max_chars", Label: "Context file limit", Short: "Longest project instruction file text sent, in characters.", Group: GroupAgent, Order: 1510, Advanced: true}, Null: "sized to the model", Unit: "chars"})

// Longest wait while reading a project instruction file
var ContextFileReadTimeout = Register(&Duration{Meta: Meta{Key: "context_file_read_timeout", Label: "Context file wait", Short: "Longest wait while reading a project instruction file.", Group: GroupAgent, Order: 1520, Advanced: true}, Default: 5 * time.Second})

// Key used by subagents that use their own endpoint
var DelegationApiKey = Register(&String{Meta: Meta{Key: "delegation.api_key", Label: "Subagent API key", Short: "Key used by subagents that use their own endpoint.", Group: GroupAgent, Order: 1530, Advanced: true}, Masked: true, Default: ""})

// Request format of the subagent endpoint
var DelegationApiMode = Register(&String{Meta: Meta{Key: "delegation.api_mode", Label: "Subagent API style", Short: "Request format of the subagent endpoint.", Help: "Leave empty to detect it from the endpoint address.", Group: GroupAgent, Order: 1540, Advanced: true}, Default: ""})

// Which backend runs subagents
var DelegationBackend = Register(&String{Meta: Meta{Key: "delegation.backend", Label: "Subagent backend", Short: "Which backend runs subagents.", Help: "Default runs them in this app. Plugins can add others.", Group: GroupAgent, Order: 1550, Advanced: true, Apply: NewSession}, Default: "default"})

// Settings for each subagent backend, by backend name
var DelegationBackends = Register(&Map{Meta: Meta{Key: "delegation.backends", Label: "Backend settings", Short: "Settings for each subagent backend, by backend name.", Group: GroupAgent, Order: 1560, Advanced: true}})

// Own model endpoint address for subagents
var DelegationBaseUrl = Register(&String{Meta: Meta{Key: "delegation.base_url", Label: "Subagent endpoint", Short: "Own model endpoint address for subagents.", Help: "Leave empty to use the subagent provider or the main model.", Group: GroupAgent, Order: 1570, Advanced: true}, Default: ""})

// Stops a subagent that has been inactive this long
var DelegationChildTimeoutSeconds = Register(&Duration{Meta: Meta{Key: "delegation.child_timeout_seconds", Label: "Subagent idle limit", Short: "Stops a subagent that has been inactive this long.", Help: "Zero means no limit. Anything set is raised to at least 30 seconds.", Group: GroupAgent, Order: 1580, Advanced: true}, Default: 0})

// Token size at which a subagent compresses its chat
var DelegationCompressionThresholdTokens = Register(&Int{Meta: Meta{Key: "delegation.compression_threshold_tokens", Label: "Subagent compress at", Short: "Token size at which a subagent compresses its chat.", Help: "Zero uses the normal share of the context window. Values under 16000 are ignored.", Group: GroupAgent, Order: 1590, Advanced: true}, Default: 0, Unit: "tokens"})

// Providers subagents try when their own fails
var DelegationFallbackProviders = Register(&List{Meta: Meta{Key: "delegation.fallback_providers", Label: "Subagent fallbacks", Short: "Providers subagents try when their own fails.", Group: GroupAgent, Order: 1600, Advanced: true}, Null: "same as main agent"})

// Whether chat subagents finish within the turn or in the background
var DelegationGatewayMode = Register(&Enum{Meta: Meta{Key: "delegation.gateway_mode", Label: "Subagents in chats", Short: "Whether chat subagents finish within the turn or in the background.", Group: GroupAgent, Order: 1610, Advanced: true}, Default: "auto", Values: []Choice{{Value: "auto", Label: "Auto", Meaning: "Decide from the chat surface."}, {Value: "inline", Label: "Inline", Meaning: "Wait for subagents inside the turn."}, {Value: "background", Label: "Background", Meaning: "Run subagents in the background."}}})

// Reports each background subagent on its own, not as a group
var DelegationIndependentCompletions = Register(&Bool{Meta: Meta{Key: "delegation.independent_completions", Label: "Separate completions", Short: "Reports each background subagent on its own, not as a group.", Group: GroupAgent, Order: 1620, Advanced: true}, Default: false})

// Narrowed subagents still get the parent's MCP tools
var DelegationInheritMcpToolsets = Register(&Bool{Meta: Meta{Key: "delegation.inherit_mcp_toolsets", Label: "Subagents keep MCP", Short: "Narrowed subagents still get the parent's MCP tools.", Group: GroupAgent, Order: 1630, Advanced: true}, Default: true})

// Most subagents that run at the same time
var DelegationMaxConcurrentChildren = Register(&Int{Meta: Meta{Key: "delegation.max_concurrent_children", Label: "Parallel subagents", Short: "Most subagents that run at the same time.", Help: "Each one uses model tokens, so high values raise cost.", Group: GroupAgent, Order: 1640, Advanced: true}, Default: 10, Min: at(1)})

// Most tool turns one subagent may take
var DelegationMaxIterations = Register(&Int{Meta: Meta{Key: "delegation.max_iterations", Label: "Subagent turn limit", Short: "Most tool turns one subagent may take.", Group: GroupAgent, Order: 1650, Advanced: true}, Default: 250, Unit: "turns"})

// How many levels deep subagents may start more subagents
var DelegationMaxSpawnDepth = Register(&Int{Meta: Meta{Key: "delegation.max_spawn_depth", Label: "Subagent depth", Short: "How many levels deep subagents may start more subagents.", Help: "Each level multiplies cost.", Group: GroupAgent, Order: 1660, Advanced: true}, Default: 1, Min: at(1)})

// Longest subagent report returned to the parent
var DelegationMaxSummaryChars = Register(&Int{Meta: Meta{Key: "delegation.max_summary_chars", Label: "Subagent summary size", Short: "Longest subagent report returned to the parent.", Help: "Longer reports are trimmed and saved to disk.", Group: GroupAgent, Order: 1670, Advanced: true}, Default: 24000, Unit: "chars"})

// Model subagents use instead of the main one
var DelegationModel = Register(&String{Meta: Meta{Key: "delegation.model", Label: "Subagent model", Short: "Model subagents use instead of the main one.", Group: GroupAgent, Order: 1680, Advanced: true}, Default: ""})

// Most subagents one single-run session may start
var DelegationOneshotMaxChildren = Register(&Int{Meta: Meta{Key: "delegation.oneshot_max_children", Label: "One-shot subagent limit", Short: "Most subagents one single-run session may start.", Help: "Zero means no limit.", Group: GroupAgent, Order: 1690, Advanced: true}, Default: 2, Min: at(0)})

// Lets subagents coordinate subagents of their own
var DelegationOrchestratorEnabled = Register(&Bool{Meta: Meta{Key: "delegation.orchestrator_enabled", Label: "Subagent orchestrators", Short: "Lets subagents coordinate subagents of their own.", Group: GroupAgent, Order: 1700, Advanced: true}, Default: true})

// Provider subagents use instead of the main one
var DelegationProvider = Register(&String{Meta: Meta{Key: "delegation.provider", Label: "Subagent provider", Short: "Provider subagents use instead of the main one.", Group: GroupAgent, Order: 1710, Advanced: true}, Default: ""})

// How long subagents think before answering
var DelegationReasoningEffort = Register(&Enum{Meta: Meta{Key: "delegation.reasoning_effort", Label: "Subagent effort", Short: "How long subagents think before answering.", Group: GroupAgent, Order: 1720, Advanced: true}, Values: reasoningChoices, Null: "the main agent's level"})

// Extra request fields sent with subagent model calls
var DelegationRequestOverrides = Register(&Map{Meta: Meta{Key: "delegation.request_overrides", Label: "Subagent request fields", Short: "Extra request fields sent with subagent model calls.", Group: GroupAgent, Order: 1730, Advanced: true}})

// Lets subagents run risky commands without asking
var DelegationSubagentAutoApprove = Register(&Bool{Meta: Meta{Key: "delegation.subagent_auto_approve", Label: "Auto-approve subagents", Short: "Lets subagents run risky commands without asking.", Help: "When off, risky commands from subagents are refused.", Group: GroupAgent, Order: 1740, Advanced: true}, Default: false})

// Shows background process updates started by subagents
var DelegationSurfaceChildProcessNotifications = Register(&Bool{Meta: Meta{Key: "delegation.surface_child_process_notifications", Label: "Subagent process news", Short: "Shows background process updates started by subagents.", Group: GroupAgent, Order: 1750, Advanced: true}, Default: false})

// Most turns an active goal may use
var GoalsMaxTurns = Register(&Int{Meta: Meta{Key: "goals.max_turns", Label: "Goal turn limit", Short: "Most turns an active goal may use.", Group: GroupAgent, Order: 1760, Advanced: true}, Default: 20, Unit: "turns"})

// Splits new board tasks into smaller ones automatically
var KanbanAutoDecompose = Register(&Bool{Meta: Meta{Key: "kanban.auto_decompose", Label: "Auto split tasks", Short: "Splits new board tasks into smaller ones automatically.", Group: GroupAgent, Order: 1770, Advanced: true}, Default: true})

// Most tasks split in one check of the board
var KanbanAutoDecomposePerTick = Register(&Int{Meta: Meta{Key: "kanban.auto_decompose_per_tick", Label: "Splits per pass", Short: "Most tasks split in one check of the board.", Group: GroupAgent, Order: 1780, Advanced: true}, Default: 3, Min: at(1)})

// Moves new subtasks forward as soon as they are made
var KanbanAutoPromoteChildren = Register(&Bool{Meta: Meta{Key: "kanban.auto_promote_children", Label: "Auto start subtasks", Short: "Moves new subtasks forward as soon as they are made.", Group: GroupAgent, Order: 1790, Advanced: true}, Default: true})

// Follows a task for you when you create it
var KanbanAutoSubscribeOnCreate = Register(&Bool{Meta: Meta{Key: "kanban.auto_subscribe_on_create", Label: "Follow new tasks", Short: "Follows a task for you when you create it.", Group: GroupAgent, Order: 1800, Advanced: true}, Default: true})

// How long a board database call waits while another holds the lock
var KanbanBusyTimeoutMs = Register(&Duration{Meta: Meta{Key: "kanban.busy_timeout_ms", Label: "Board lock wait", Short: "How long a board database call waits while another holds the lock.", Group: GroupAgent, Order: 1802, Advanced: true, Env: "ICEBATH_KANBAN_BUSY_TIMEOUT_MS"}, Default: 120 * time.Second, Unit: time.Millisecond, Min: at(time.Millisecond)})

// How long a worker's claim on a task lasts before the worker must renew it
var KanbanClaimTtlSeconds = Register(&Duration{Meta: Meta{Key: "kanban.claim_ttl_seconds", Label: "Task claim time", Short: "How long a worker's claim on a task lasts before it must renew it.", Group: GroupAgent, Order: 1804, Advanced: true, Env: "ICEBATH_KANBAN_CLAIM_TTL_SECONDS"}, Default: 15 * time.Minute, Min: at(time.Second)})

// How long a new worker gets before a missing process counts as a crash
var KanbanCrashGraceSeconds = Register(&Duration{Meta: Meta{Key: "kanban.crash_grace_seconds", Label: "Crash grace", Short: "How long a new worker gets before a missing process counts as a crash.", Help: "Zero counts it at once.", Group: GroupAgent, Order: 1806, Advanced: true, Env: "ICEBATH_KANBAN_CRASH_GRACE_SECONDS"}, Default: 30 * time.Second, Min: at(time.Duration(0))})

// Profile that takes tasks nobody was assigned
var KanbanDefaultAssignee = Register(&String{Meta: Meta{Key: "kanban.default_assignee", Label: "Default assignee", Short: "Profile that takes tasks nobody was assigned.", Group: GroupAgent, Order: 1810, Advanced: true}, Default: ""})

// Blocks after an unblock that raise a board health warning
var KanbanDiagnosticsBlockCycleThreshold = Register(&Int{Meta: Meta{Key: "kanban.diagnostics.block_cycle_threshold", Label: "Block cycle warning", Short: "Times a task is blocked again after an unblock before a warning.", Group: GroupAgent, Order: 1812, Advanced: true}, Default: 3, Min: at(1), DefaultBelow: true})

// Time span the repeated blocks of one task are counted in
var KanbanDiagnosticsBlockCycleWindowSeconds = Register(&Duration{Meta: Meta{Key: "kanban.diagnostics.block_cycle_window_seconds", Label: "Block cycle window", Short: "Time span in which repeated blocks of one task are counted.", Group: GroupAgent, Order: 1814, Advanced: true}, Default: 24 * time.Hour, Min: at(time.Second), DefaultBelow: true})

// How long a task may stay blocked with no reply before a board health warning
var KanbanDiagnosticsBlockedStaleHours = Register(&Duration{Meta: Meta{Key: "kanban.diagnostics.blocked_stale_hours", Label: "Blocked task warning", Short: "How long a task may stay blocked with no reply before a warning.", Group: GroupAgent, Order: 1816, Advanced: true}, Default: 24 * time.Hour, Unit: time.Hour, Min: at(time.Duration(0)), DefaultBelow: true})

// Crashed runs in a row that raise a board health warning
var KanbanDiagnosticsCrashThreshold = Register(&Int{Meta: Meta{Key: "kanban.diagnostics.crash_threshold", Label: "Crash warning", Short: "Crashed runs in a row before a task gets a crash warning.", Group: GroupAgent, Order: 1818, Advanced: true}, Default: 2, Min: at(1), DefaultBelow: true})

// Failed runs in a row that raise a board health warning
var KanbanDiagnosticsFailureThreshold = Register(&Int{Meta: Meta{Key: "kanban.diagnostics.failure_threshold", Label: "Failure warning", Short: "Failed runs in a row before a task gets a failure warning.", Group: GroupAgent, Order: 1820, Advanced: true}, Min: at(1), Null: "the task failure limit", DefaultBelow: true})

// Time past an expired claim before a running task with no worker gets a board health warning
var KanbanDiagnosticsStaleRunningGraceSeconds = Register(&Duration{Meta: Meta{Key: "kanban.diagnostics.stale_running_grace_seconds", Label: "Lost worker warning", Short: "Time past an expired claim before a task with no worker is flagged.", Group: GroupAgent, Order: 1822, Advanced: true}, Default: time.Hour, Min: at(time.Duration(0)), DefaultBelow: true})

// How long an assigned ready task may wait unclaimed before a board health warning
var KanbanDiagnosticsStrandedThresholdSeconds = Register(&Duration{Meta: Meta{Key: "kanban.diagnostics.stranded_threshold_seconds", Label: "Waiting task warning", Short: "How long an assigned ready task may wait unclaimed before a warning.", Group: GroupAgent, Order: 1824, Advanced: true}, Default: 30 * time.Minute, Min: at(time.Duration(0)), DefaultBelow: true})

// Starts board tasks from the background service
var KanbanDispatchInGateway = Register(&Bool{Meta: Meta{Key: "kanban.dispatch_in_gateway", Label: "Board in service", Short: "Starts board tasks from the background service.", Group: GroupAgent, Order: 1830, Advanced: true, Apply: Restart}, Default: true})

// How often the board looks for tasks to start
var KanbanDispatchIntervalSeconds = Register(&Duration{Meta: Meta{Key: "kanban.dispatch_interval_seconds", Label: "Board check interval", Short: "How often the board looks for tasks to start.", Help: "At least one second.", Group: GroupAgent, Order: 1840, Advanced: true}, Default: time.Minute, Min: at(time.Second)})

// Profiles allowed to pick up board tasks
var KanbanDispatchProfiles = Register(&List{Meta: Meta{Key: "kanban.dispatch_profiles", Label: "Allowed profiles", Short: "Profiles allowed to pick up board tasks.", Group: GroupAgent, Order: 1850, Advanced: true}, Null: "any profile"})

// Marks a running task stale after this long without progress
var KanbanDispatchStaleTimeoutSeconds = Register(&Duration{Meta: Meta{Key: "kanban.dispatch_stale_timeout_seconds", Label: "Stale task timeout", Short: "Marks a running task stale after this long without progress.", Help: "Zero turns stale detection off.", Group: GroupAgent, Order: 1860, Advanced: true}, Default: 4 * time.Hour})

// How long follows on finished tasks are kept
var KanbanDoneSubRetentionDays = Register(&Duration{Meta: Meta{Key: "kanban.done_sub_retention_days", Label: "Keep done follows", Short: "How long follows on finished tasks are kept.", Group: GroupAgent, Order: 1870, Advanced: true}, Default: 30 * 24 * time.Hour, Unit: 24 * time.Hour})

// Failures after which a task stops being retried
var KanbanFailureLimit = Register(&Int{Meta: Meta{Key: "kanban.failure_limit", Label: "Task failure limit", Short: "Failures after which a task stops being retried.", Group: GroupAgent, Order: 1880, Advanced: true}, Default: 2, Min: at(1), DefaultBelow: true})

// Most board tasks running at the same time
var KanbanMaxInProgress = Register(&Int{Meta: Meta{Key: "kanban.max_in_progress", Label: "Tasks at once", Short: "Most board tasks running at the same time.", Help: "Must be at least 1.", Group: GroupAgent, Order: 1890, Advanced: true}, Null: "sized to free memory"})

// Most running board tasks per profile
var KanbanMaxInProgressPerProfile = Register(&Int{Meta: Meta{Key: "kanban.max_in_progress_per_profile", Label: "Tasks per profile", Short: "Most running board tasks per profile.", Group: GroupAgent, Order: 1900, Advanced: true}, Null: "no limit"})

// Most tasks started in one check of the board
var KanbanMaxSpawn = Register(&Int{Meta: Meta{Key: "kanban.max_spawn", Label: "Starts per pass", Short: "Most tasks started in one check of the board.", Group: GroupAgent, Order: 1910, Advanced: true}, Null: "no limit"})

// Sends board updates from the background service
var KanbanNotifyInGateway = Register(&Bool{Meta: Meta{Key: "kanban.notify_in_gateway", Label: "Board alerts", Short: "Sends board updates from the background service.", Group: GroupAgent, Order: 1920, Advanced: true, Apply: Restart}, Default: true})

// Profile that plans and splits board tasks
var KanbanOrchestratorProfile = Register(&String{Meta: Meta{Key: "kanban.orchestrator_profile", Label: "Planner profile", Short: "Profile that plans and splits board tasks.", Group: GroupAgent, Order: 1930, Advanced: true}, Default: ""})

// How long a task stopped by a rate limit waits before it starts again
var KanbanRateLimitCooldownSeconds = Register(&Duration{Meta: Meta{Key: "kanban.rate_limit_cooldown_seconds", Label: "Rate limit pause", Short: "How long a task stopped by a rate limit waits before it starts again.", Help: "Zero starts it on the next check.", Group: GroupAgent, Order: 1935, Advanced: true, Env: "ICEBATH_KANBAN_RATE_LIMIT_COOLDOWN_SECONDS"}, Default: 5 * time.Minute, Min: at(time.Duration(0))})

// Recovers running tasks whose worker has vanished
var KanbanReconcileOrphans = Register(&Bool{Meta: Meta{Key: "kanban.reconcile_orphans", Label: "Recover lost tasks", Short: "Recovers running tasks whose worker has vanished.", Group: GroupAgent, Order: 1940, Advanced: true}, Default: true})

// Starts review tasks automatically
var KanbanReviewDispatch = Register(&Bool{Meta: Meta{Key: "kanban.review_dispatch", Label: "Auto start reviews", Short: "Starts review tasks automatically.", Group: GroupAgent, Order: 1950, Advanced: true}, Default: true})

// Reminds a task worker to report its result before it stops
var KanbanStopNudge = Register(&Bool{Meta: Meta{Key: "kanban.stop_nudge", Label: "Finish task reminder", Short: "Reminds a task worker to mark its task done before it stops.", Group: GroupAgent, Order: 1955, Advanced: true, Env: "ICEBATH_KANBAN_STOP_NUDGE"}, Default: true})

// Most tokens the triage step may write for one task spec
var KanbanSpecifyMaxTokens = Register(&Int{Meta: Meta{Key: "kanban.specify_max_tokens", Label: "Spec reply limit", Short: "Most tokens the triage step may write for one task spec.", Group: GroupAgent, Order: 1955, Advanced: true, Env: "ICEBATH_KANBAN_SPECIFY_MAX_TOKENS"}, Default: 6000, Min: at(1500), Unit: "tokens"})

// Words that switch turn retries off
var turnRecoveryOff = map[string]bool{"0": true, "false": true, "no": true, "off": true}

// Times a board worker retries a turn that failed on a passing error
var KanbanTurnRecovery = Register(&Enum{Meta: Meta{Key: "kanban.turn_recovery", Label: "Turn retries", Short: "Times a board worker retries a turn that failed on a passing error.", Help: "Off never retries; more than 10 counts as 10.", Group: GroupAgent, Order: 1957, Advanced: true, Env: "ICEBATH_KANBAN_TURN_RECOVERY",
	Validate: func(v any) error {
		w, _ := v.(string)
		w = strings.ToLower(strings.TrimSpace(w))
		if turnRecoveryOff[w] {
			return nil
		}
		if _, err := strconv.Atoi(w); err == nil || errors.Is(err, strconv.ErrRange) {
			return nil
		}
		return errors.New("takes off or a number of retries")
	}}, Default: "3", Values: []Choice{{Value: "off", Label: "Off", Meaning: "A failed turn is not retried."}}, Open: true})

// Retries a kanban.turn_recovery word allows, 0 for off, clamped to 0 through 10
func TurnRecoveryAttempts(word string) int {
	w := strings.ToLower(strings.TrimSpace(word))
	if turnRecoveryOff[w] {
		return 0
	}
	n, err := strconv.Atoi(w)
	switch {
	case errors.Is(err, strconv.ErrRange) && !strings.HasPrefix(w, "-"):
		return 10
	case err != nil:
		return 0
	}
	return max(0, min(10, n))
}

// Old task log files kept after a log rolls over
var KanbanWorkerLogBackupCount = Register(&Int{Meta: Meta{Key: "kanban.worker_log_backup_count", Label: "Task log copies", Short: "Old task log files kept after a log rolls over.", Group: GroupAgent, Order: 1960, Advanced: true}, Default: 1, Min: at(0), DefaultBelow: true})

// Size at which a task log rolls over
var KanbanWorkerLogRotateBytes = Register(&Int{Meta: Meta{Key: "kanban.worker_log_rotate_bytes", Label: "Task log size", Short: "Size at which a task log rolls over.", Group: GroupAgent, Order: 1970, Advanced: true}, Default: 2097152, Min: at(1), Unit: "bytes", DefaultBelow: true})

// Most runs one loop may make
var LoopsMaxTicks = Register(&Int{Meta: Meta{Key: "loops.max_ticks", Label: "Loop run limit", Short: "Most runs one loop may make.", Help: "Zero means no limit.", Group: GroupAgent, Order: 1980, Advanced: true}, Default: 100, Min: at(0)})

// Shortest time allowed between loop runs
var LoopsMinIntervalSeconds = Register(&Duration{Meta: Meta{Key: "loops.min_interval_seconds", Label: "Loop shortest gap", Short: "Shortest time allowed between loop runs.", Group: GroupAgent, Order: 1990, Advanced: true}, Default: 30 * time.Second, Min: at(5 * time.Second)})

// Longest wait a self-paced loop may choose
var LoopsSelfPacedCeilingSeconds = Register(&Duration{Meta: Meta{Key: "loops.self_paced_ceiling_seconds", Label: "Self-paced longest gap", Short: "Longest wait a self-paced loop may choose.", Group: GroupAgent, Order: 2000, Advanced: true}, Default: 15 * time.Minute})

// Shortest wait a self-paced loop may choose
var LoopsSelfPacedFloorSeconds = Register(&Duration{Meta: Meta{Key: "loops.self_paced_floor_seconds", Label: "Self-paced shortest gap", Short: "Shortest wait a self-paced loop may choose.", Group: GroupAgent, Order: 2010, Advanced: true}, Default: time.Minute, Min: at(10 * time.Second)})

// Mixture preset currently in use
var MoaActivePreset = Register(&String{Meta: Meta{Key: "moa.active_preset", Label: "Active mix", Short: "Mixture preset currently in use.", Group: GroupAgent, Order: 2020, Advanced: true}, Default: ""})

// Mixture preset used when none is chosen
var MoaDefaultPreset = Register(&String{Meta: Meta{Key: "moa.default_preset", Label: "Default mix", Short: "Mixture preset used when none is chosen.", Group: GroupAgent, Order: 2030, Advanced: true}, Default: "default"})

// Cadences a mixture preset may ask its answering models at
func checkMoaFanout(v any) error {
	text, _ := v.(string)
	if text == "user_turn" || text == "per_iteration" {
		return nil
	}
	if count, isEvery := strings.CutPrefix(text, "every_n:"); isEvery {
		if n, err := strconv.Atoi(count); err == nil && n >= 2 {
			return nil
		}
	}
	return errors.New("takes user_turn, per_iteration or every_n:<count> with a count of 2 or more")
}

// Fields of one model a mixture preset names, the answering models also carrying their own switch
func moaSlotFields(what string, withSwitch bool) []Entry {
	fields := []Entry{
		&String{Meta: Meta{Key: "provider", Label: "Provider", Short: "Provider " + what + " is reached through."}},
		&String{Meta: Meta{Key: "model", Label: "Model", Short: "Name of " + what + "."}},
		&Enum{Meta: Meta{Key: "reasoning_effort", Label: "Thinking effort", Short: "How long " + what + " thinks before answering."},
			Values: reasoningChoices, Null: "the model's own level"},
	}
	if withSwitch {
		fields = append(fields, &Bool{Meta: Meta{Key: "enabled", Label: "In use", Short: "Whether " + what + " takes part."}, Default: true})
	}
	return fields
}

// Named groups of models that answer together, with one combining model
var MoaPresets = Register(&Map{Meta: Meta{Key: "moa.presets", Label: "Mixture presets", Short: "Named groups of models that answer together, with one combining model.", Group: GroupAgent, Order: 2040, Advanced: true},
	Default: map[string]any{"default": map[string]any{"aggregator": map[string]any{"model": "anthropic/claude-opus-4.8", "provider": "openrouter"}, "enabled": true, "reference_models": []any{map[string]any{"model": "gpt-5.5", "provider": "openai-codex"}, map[string]any{"model": "deepseek/deepseek-v4-pro", "provider": "openrouter"}}}},
	Replace: true,
	Value: &Object{Meta: Meta{Key: "moa.presets.*", Label: "Mixture preset", Short: "Models that answer together and the model that combines them."}, Fields: []Entry{
		&Bool{Meta: Meta{Key: "enabled", Label: "In use", Short: "Whether the preset can be picked."}, Default: true},
		&List{Meta: Meta{Key: "reference_models", Label: "Answering models", Short: "Models that each answer first, each with a provider and a model."},
			Of: KindObject, Item: &Object{Meta: Meta{Key: "moa.presets.*.reference_models.*", Label: "Answering model", Short: "One model that answers before the combining model."},
				Fields: moaSlotFields("the answering model", true)}},
		&Object{Meta: Meta{Key: "aggregator", Label: "Combining model", Short: "Model that writes the final answer from the others."},
			Fields: moaSlotFields("the combining model", false)},
		&Float{Meta: Meta{Key: "reference_temperature", Label: "Answering temperature", Short: "How varied the answering models may be."},
			Null: "each provider's default", Min: at(0.0), Max: at(2.0)},
		&Float{Meta: Meta{Key: "aggregator_temperature", Label: "Combining temperature", Short: "How varied the combining model may be."},
			Null: "the provider's default", Min: at(0.0), Max: at(2.0)},
		&Duration{Meta: Meta{Key: "reference_timeout", Label: "Answering time limit", Short: "Longest wait for one answering model."},
			Null: "the reference answer time limit", Min: at(time.Second)},
		&Enum{Meta: Meta{Key: "degraded_reference_policy", Label: "Missing answers", Short: "Whether you are told when an answering model fails."},
			Default: "loud", Values: []Choice{
				{"loud", "Tell me", "Say which models gave no answer."},
				{"silent", "Stay quiet", "Combine the answers that arrived without a note."},
			}},
		&String{Meta: Meta{Key: "fanout", Label: "Asking cadence", Short: "How often the answering models are asked.",
			Help: "user_turn asks once per prompt, per_iteration every step, every_n:<count> every few steps.", Validate: checkMoaFanout},
			Default: "user_turn"},
	}}})

// Hides private details sent to the mixture's models
var MoaPrivacyFilter = Register(&Enum{Meta: Meta{Key: "moa.privacy_filter", Label: "Mixture privacy", Short: "Hides private details sent to the mixture's models.", Group: GroupAgent, Order: 2050, Advanced: true}, Default: "", Values: []Choice{{Value: "", Label: "Off", Meaning: "No filtering."}, {Value: "display", Label: "Display", Meaning: "Hide details on screen only."}, {Value: "full", Label: "Full", Meaning: "Hide details everywhere."}}})

// Saves a record of each mixture run to disk
var MoaSaveTraces = Register(&Bool{Meta: Meta{Key: "moa.save_traces", Label: "Save mixture traces", Short: "Saves a record of each mixture run to disk.", Group: GroupAgent, Order: 2060, Advanced: true}, Default: false})

// Where mixture records are saved
var MoaTraceDir = Register(&String{Meta: Meta{Key: "moa.trace_dir", Label: "Trace folder", Short: "Where mixture records are saved.", Help: "Leave empty to use a folder in your profile.", Group: GroupAgent, Order: 2070, Advanced: true}, Default: ""})

// Custom personalities you can switch to, by name
var Personalities = Register(&Map{Meta: Meta{Key: "personalities", Label: "Personalities", Short: "Custom personalities you can switch to, by name.", Group: GroupAgent, Order: 2080, Advanced: true}})

// Your own instructions added to every conversation while no personality is chosen
var AgentSystemPrompt = Register(&String{Meta: Meta{Key: "agent.system_prompt", Label: "Own instructions", Short: "Your own instructions added to every conversation.", Help: "A chosen personality takes their place.", Group: GroupAgent, Order: 2085, Advanced: true, Apply: NewSession}, Default: ""})

// JSON file of example messages added to every conversation
var PrefillMessagesFile = Register(&String{Meta: Meta{Key: "prefill_messages_file", Label: "Prefill messages file", Short: "JSON file of example messages added to every conversation.", Help: "A relative path starts from your profile folder.", Group: GroupAgent, Order: 2090, Advanced: true, Env: "ICEBATH_PREFILL_MESSAGES_FILE"}, Default: ""})

// Time zone for dates, times and schedules, such as Europe/Berlin
var Timezone = Register(&String{Meta: Meta{Key: "timezone", Label: "Timezone", Short: "Time zone for dates, times and schedules, such as Europe/Berlin.", Help: "Leave empty to use this computer's time zone.", Group: GroupAgent, Order: 2100, Advanced: true, Env: "ICEBATH_TIMEZONE"}, Default: ""})

// Stops after the same failing call repeats this many times
var ToolLoopGuardrailsHardStopAfterExactFailure = Register(&Int{Meta: Meta{Key: "tool_loop_guardrails.hard_stop_after.exact_failure", Label: "Stop: same failure", Short: "Stops after the same failing call repeats this many times.", Group: GroupAgent, Order: 2110, Advanced: true}, Default: 5, Min: at(1)})

// Stops after a read-only call repeats with no new result
var ToolLoopGuardrailsHardStopAfterIdempotentNoProgress = Register(&Int{Meta: Meta{Key: "tool_loop_guardrails.hard_stop_after.idempotent_no_progress", Label: "Stop: no progress", Short: "Stops after a read-only call repeats with no new result.", Group: GroupAgent, Order: 2120, Advanced: true}, Default: 5, Min: at(1)})

// Stops after one tool keeps failing this many times
var ToolLoopGuardrailsHardStopAfterSameToolFailure = Register(&Int{Meta: Meta{Key: "tool_loop_guardrails.hard_stop_after.same_tool_failure", Label: "Stop: tool failures", Short: "Stops after one tool keeps failing this many times.", Group: GroupAgent, Order: 2130, Advanced: true}, Default: 8, Min: at(1)})

// Stops a turn that is stuck in a tool loop
var ToolLoopGuardrailsHardStopEnabled = Register(&Bool{Meta: Meta{Key: "tool_loop_guardrails.hard_stop_enabled", Label: "Stop looping tools", Short: "Stops a turn that is stuck in a tool loop.", Group: GroupAgent, Order: 2140, Advanced: true}, Default: false})

// Most subagents one turn may start
var ToolLoopGuardrailsLoopCapsMaxSubagents = Register(&Int{Meta: Meta{Key: "tool_loop_guardrails.loop_caps.max_subagents", Label: "Subagents per turn", Short: "Most subagents one turn may start.", Help: "Zero means no limit.", Group: GroupAgent, Order: 2150, Advanced: true}, Default: 50, Min: at(0)})

// Most web searches one turn may make
var ToolLoopGuardrailsLoopCapsMaxWebSearches = Register(&Int{Meta: Meta{Key: "tool_loop_guardrails.loop_caps.max_web_searches", Label: "Searches per turn", Short: "Most web searches one turn may make.", Help: "Zero means no limit.", Group: GroupAgent, Order: 2160, Advanced: true}, Default: 50, Min: at(0)})

// Always stops tool loops in chats and scheduled runs
var ToolLoopGuardrailsNonInteractiveHardStopEnabled = Register(&Bool{Meta: Meta{Key: "tool_loop_guardrails.non_interactive_hard_stop_enabled", Label: "Stop loops unattended", Short: "Always stops tool loops in chats and scheduled runs.", Group: GroupAgent, Order: 2170, Advanced: true}, Default: true})

// Warns the agent after the same failing call repeats this often
var ToolLoopGuardrailsWarnAfterExactFailure = Register(&Int{Meta: Meta{Key: "tool_loop_guardrails.warn_after.exact_failure", Label: "Warn: same failure", Short: "Warns the agent after the same failing call repeats this often.", Group: GroupAgent, Order: 2180, Advanced: true}, Default: 2, Min: at(1)})

// Warns after a read-only call repeats with no new result
var ToolLoopGuardrailsWarnAfterIdempotentNoProgress = Register(&Int{Meta: Meta{Key: "tool_loop_guardrails.warn_after.idempotent_no_progress", Label: "Warn: no progress", Short: "Warns after a read-only call repeats with no new result.", Group: GroupAgent, Order: 2190, Advanced: true}, Default: 2, Min: at(1)})

// Warns the agent after one tool keeps failing this often
var ToolLoopGuardrailsWarnAfterSameToolFailure = Register(&Int{Meta: Meta{Key: "tool_loop_guardrails.warn_after.same_tool_failure", Label: "Warn: tool failures", Short: "Warns the agent after one tool keeps failing this often.", Group: GroupAgent, Order: 2200, Advanced: true}, Default: 3, Min: at(1)})

// Warns the agent when it seems stuck in a tool loop
var ToolLoopGuardrailsWarningsEnabled = Register(&Bool{Meta: Meta{Key: "tool_loop_guardrails.warnings_enabled", Label: "Loop warnings", Short: "Warns the agent when it seems stuck in a tool loop.", Group: GroupAgent, Order: 2210, Advanced: true}, Default: true})

// Each subagent working in its own git worktree off the parent's commit
var DelegationWorktreeIsolation = Register(&Bool{Meta: Meta{Key: "delegation.worktree_isolation", Label: "Subagent worktrees", Short: "Give each subagent its own git worktree off your current commit.", Group: GroupAgent, Order: 1700, Advanced: true, Apply: NextTurn}, Default: false})
