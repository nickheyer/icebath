package config

import transportconfig "github.com/nickheyer/icebath/internal/transport/config"

const (
	Key = transportconfig.Key + ".codex_responses"
	// Seconds a stream may go without its first event before it is dropped, 0 for no limit
	FirstEventTimeout = Key + ".ttfb_timeout"
	// Seconds a stream may go silent after its first event before it is dropped; empty picks it from the request's size
	EventTimeout = Key + ".event_stale_timeout"
	// Tokens of context from which the first event limit is raised to the event limit, 0 never
	FirstEventLargeAbove = Key + ".ttfb_disable_above_tokens"
	// Keeps the first event limit as it is for large requests
	FirstEventStrict = Key + ".ttfb_strict"
	// Most seconds the first event limit is raised to, 0 for no cap
	FirstEventMax = Key + ".ttfb_max"
	// Most seconds a request's silence limit is raised to, 0 for no cap
	HardTimeout = Key + ".hard_timeout"
	// How long the model's answers are: low, medium or high; empty for the model's own
	TextVerbosity = Key + ".text_verbosity"
)

// Variables that set them
const (
	FirstEventTimeoutEnv    = "ICEBATH_CODEX_TTFB_TIMEOUT_SECONDS"
	EventTimeoutEnv         = "ICEBATH_CODEX_EVENT_STALE_TIMEOUT_SECONDS"
	FirstEventLargeAboveEnv = "ICEBATH_CODEX_TTFB_DISABLE_ABOVE_TOKENS"
	FirstEventStrictEnv     = "ICEBATH_CODEX_TTFB_STRICT"
	FirstEventMaxEnv        = "ICEBATH_CODEX_TTFB_MAX_SECONDS"
	HardTimeoutEnv          = "ICEBATH_CODEX_HARD_TIMEOUT_SECONDS"
)
