package gatewayrun

// Section and key config values baked into the agent at construction whose change invalidates the cached agent
var cacheBustingConfigKeys = [][2]string{
	{"model", "context_length"}, {"compression", "enabled"},
	{"compression", "progress_notices"}, {"compression", "threshold"},
	{"compression", "model_thresholds"}, {"compression", "threshold_tokens"},
	{"compression", "codex_gpt55_autoraise"}, {"compression", "codex_app_server_auto"},
	{"compression", "codex_responses_native"},
	{"compression", "codex_responses_compact_threshold"}, {"compression", "in_place"},
	{"compression", "checkpoint_required"}, {"compression", "micro_compact"},
	{"compression", "micro_compact_every_n_turns"},
	{"compression", "micro_compact_defrag_threshold_tokens"}, {"compression", "target_ratio"},
	{"compression", "tail_mode"}, {"compression", "protect_last_n"},
	{"compression", "proactive_prune_tokens"},
	{"compression", "proactive_prune_min_result_chars"},
	{"compression", "proactive_prune_min_reclaim_tokens"},
	{"compression", "min_tail_user_messages"}, {"agent", "disabled_toolsets"},
	{"memory", "provider"}, {"checkpoints", "enabled"}, {"checkpoints", "max_snapshots"},
	{"checkpoints", "max_total_size_mb"}, {"checkpoints", "max_file_size_mb"},
}
