package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	Key                 = pluginsconfig.Key + ".agent"
	Provider            = Key + ".provider"
	Model               = Key + ".model"
	ReasoningEffort     = Key + ".reasoning_effort"
	MaxTurns            = Key + ".max_turns"
	APIMaxRetries       = Key + ".api_max_retries"
	ContextFileMaxChars = Key + ".context_file_max_chars"
	ApprovalsMode       = Key + ".approvals.mode"
	ApprovalsTimeout    = Key + ".approvals.timeout"
	ApprovalsAllowlist  = Key + ".approvals.allowlist"
	ApprovalsManual     = "manual"
	ApprovalsOff        = "off"
)
