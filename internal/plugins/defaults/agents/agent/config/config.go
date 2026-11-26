package config

const (
	Key                 = "agent"
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
	Thinking            = Key + ".thinking"
	ThinkingHidden      = "hidden"
	ThinkingFolded      = "folded"
	ThinkingOpen        = "open"
)

// How hard a model can be told to think, least first
var Efforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}
