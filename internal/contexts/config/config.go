package config

const (
	Key = "context"
	// Engine that manages the long conversations of new sessions
	Engine = Key + ".engine"
	// Engine icebath ships, summarising the middle of a conversation
	Compressor = "compressor"
)

// How conversations near the model's limit are shortened, for whichever engine runs them
const (
	CompressionKey = Key + ".compression"
	Enabled        = CompressionKey + ".enabled"
	Threshold      = CompressionKey + ".threshold"
	// Hard cap in tokens of the trigger, unset for the share of the window alone
	ThresholdTokens = CompressionKey + ".threshold_tokens"
	// Trigger share by model name, a provider:model key outranking a bare one
	ModelThresholds = CompressionKey + ".model_thresholds"
	TargetRatio     = CompressionKey + ".target_ratio"
	ProtectFirstN   = CompressionKey + ".protect_first_n"
	ProtectLastN    = CompressionKey + ".protect_last_n"
	MinTailUsers    = CompressionKey + ".min_tail_user_messages"
	TailMode        = CompressionKey + ".tail_mode"
	InPlace         = CompressionKey + ".in_place"
	PrepareAhead    = CompressionKey + ".prepare_ahead"
	MaxAttempts     = CompressionKey + ".max_attempts"
	AbortOnFailure  = CompressionKey + ".abort_on_summary_failure"
	// No compression until a memory provider saved a checkpoint of the chat
	CheckpointRequired = CompressionKey + ".checkpoint_required"
	ProgressNotices    = CompressionKey + ".progress_notices"
	// Seconds the summary step may stay silent, and may take in all
	IdleTimeout  = CompressionKey + ".context_timeout_seconds"
	TotalCeiling = CompressionKey + ".context_total_ceiling_seconds"
	IdleCompact  = CompressionKey + ".idle_compact_after_seconds"

	PruneTokens       = CompressionKey + ".proactive_prune_tokens"
	PruneMinChars     = CompressionKey + ".proactive_prune_min_result_chars"
	PruneMinReclaim   = CompressionKey + ".proactive_prune_min_reclaim_tokens"
	MicroCompact      = CompressionKey + ".micro_compact"
	MicroEveryNTurns  = CompressionKey + ".micro_compact_every_n_turns"
	MicroDefragTokens = CompressionKey + ".micro_compact_defrag_threshold_tokens"

	CodexAutoraise        = CompressionKey + ".codex_gpt55_autoraise"
	CodexAutoraiseNotice  = CompressionKey + ".codex_gpt55_autoraise_notice"
	CodexNative           = CompressionKey + ".codex_responses_native"
	CodexCompactThreshold = CompressionKey + ".codex_responses_compact_threshold"
)

// Values of tail_mode
const (
	TailLean   = "lean"
	TailLegacy = "legacy"
)
