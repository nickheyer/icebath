package config

import "gopkg.in/yaml.v3"

// Context compression section, with the summary_* keys of older configs that moved to auxiliary.compression
type CompressionConfig struct {
	Enabled                           bool                `yaml:"enabled" coerce:"strset"`
	CheckpointRequired                bool                `yaml:"checkpoint_required"`
	ProgressNotices                   bool                `yaml:"progress_notices"`
	Threshold                         float64             `yaml:"threshold"`
	ThresholdTokens                   *int                `yaml:"threshold_tokens"`
	TargetRatio                       float64             `yaml:"target_ratio"`
	TailMode                          TailMode            `yaml:"tail_mode"`
	ProtectLastN                      int                 `yaml:"protect_last_n"`
	MinTailUserMessages               int                 `yaml:"min_tail_user_messages" coerce:"whole"`
	MaxAttempts                       int                 `yaml:"max_attempts" coerce:"whole"`
	ProactivePruneTokens              int                 `yaml:"proactive_prune_tokens" coerce:"whole"`
	ProactivePruneMinResultChars      int                 `yaml:"proactive_prune_min_result_chars" coerce:"whole"`
	ProactivePruneMinReclaimTokens    int                 `yaml:"proactive_prune_min_reclaim_tokens" coerce:"whole"`
	MicroCompact                      bool                `yaml:"micro_compact"`
	MicroCompactEveryNTurns           int                 `yaml:"micro_compact_every_n_turns" coerce:"whole"`
	MicroCompactDefragThresholdTokens int                 `yaml:"micro_compact_defrag_threshold_tokens" coerce:"whole"`
	HygieneHardMessageLimit           int                 `yaml:"hygiene_hard_message_limit"`
	HygieneTimeoutSeconds             float64             `yaml:"hygiene_timeout_seconds"`
	HygieneTotalCeilingSeconds        float64             `yaml:"hygiene_total_ceiling_seconds"`
	HygieneFailureCooldownSeconds     float64             `yaml:"hygiene_failure_cooldown_seconds"`
	HygieneMaxTurnHoldSeconds         float64             `yaml:"hygiene_max_turn_hold_seconds"`
	ContextTimeoutSeconds             float64             `yaml:"context_timeout_seconds"`
	ContextTotalCeilingSeconds        float64             `yaml:"context_total_ceiling_seconds"`
	ProtectFirstN                     int                 `yaml:"protect_first_n"`
	AbortOnSummaryFailure             bool                `yaml:"abort_on_summary_failure" coerce:"strset"`
	CodexGPT55Autoraise               bool                `yaml:"codex_gpt55_autoraise" coerce:"strset"`
	CodexGPT55AutoraiseNotice         bool                `yaml:"codex_gpt55_autoraise_notice" coerce:"strset"`
	CodexAppServerAuto                CodexCompactionMode `yaml:"codex_app_server_auto"`
	CodexResponsesNative              bool                `yaml:"codex_responses_native"`
	CodexResponsesCompactThreshold    *int                `yaml:"codex_responses_compact_threshold" coerce:"nofloat"`
	InPlace                           bool                `yaml:"in_place"`
	ModelThresholds                   Map[float64]        `yaml:"model_thresholds" coerce:"exact"`
	IdleCompactAfterSeconds           int                 `yaml:"idle_compact_after_seconds"`
	PrepareAhead                      bool                `yaml:"prepare_ahead"`
	SummaryModel                      string              `yaml:"summary_model,omitempty"`
	SummaryProvider                   string              `yaml:"summary_provider,omitempty"`
	SummaryBaseURL                    string              `yaml:"summary_base_url,omitempty"`
	rest                              rest
}

type compressionFields CompressionConfig

// Decodes the mapping and keeps unknown keys
func (c *CompressionConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*compressionFields)(c), &c.rest)
}

// Writes the mapping with unknown keys after the named ones
func (c CompressionConfig) MarshalYAML() (any, error) {
	return encodeMapping(compressionFields(c), c.rest)
}

// Verbatim tail sizing of compaction
type TailMode string

// Tail modes
const (
	TailLean   TailMode = "lean"
	TailLegacy TailMode = "legacy"
)

// Accepted tail modes
func TailModes() []TailMode {
	return []TailMode{TailLean, TailLegacy}
}

// Who triggers Codex app-server thread compaction
type CodexCompactionMode string

// Codex compaction modes
const (
	CodexCompactNative  CodexCompactionMode = "native"
	CodexCompactIcebath CodexCompactionMode = "icebath"
	CodexCompactOff     CodexCompactionMode = "off"
)

// Accepted Codex compaction modes
func CodexCompactionModes() []CodexCompactionMode {
	return []CodexCompactionMode{CodexCompactNative, CodexCompactIcebath, CodexCompactOff}
}

// Anthropic prompt caching section
type PromptCachingConfig struct {
	CacheTTL *Scalar `yaml:"cache_ttl"`
	rest     rest
}

type promptCachingFields PromptCachingConfig

// Decodes the mapping and keeps unknown keys
func (p *PromptCachingConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*promptCachingFields)(p), &p.rest)
}

// Writes the mapping with unknown keys after the named ones
func (p PromptCachingConfig) MarshalYAML() (any, error) {
	return encodeMapping(promptCachingFields(p), p.rest)
}

// Prompt cache TTL words, any falsy value or null disabling caching
func CacheTTLs() []string {
	return []string{"5m", "1h", "auto"}
}

// Words that disable prompt caching besides false and null
func CacheTTLDisableWords() []string {
	return []string{"off", "disabled", "no", "none", "false", "0", ""}
}

// OpenRouter request settings
type OpenRouterConfig struct {
	ResponseCache    bool   `yaml:"response_cache" coerce:"bool"`
	ResponseCacheTTL int    `yaml:"response_cache_ttl" coerce:"exact"`
	MinCodingScore   Scalar `yaml:"min_coding_score"`
	rest             rest
}

type openRouterFields OpenRouterConfig

// Decodes the mapping and keeps unknown keys
func (o *OpenRouterConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*openRouterFields)(o), &o.rest)
}

// Writes the mapping with unknown keys after the named ones
func (o OpenRouterConfig) MarshalYAML() (any, error) {
	return encodeMapping(openRouterFields(o), o.rest)
}

// AWS Bedrock section
type BedrockConfig struct {
	Region    string           `yaml:"region"`
	Discovery BedrockDiscovery `yaml:"discovery"`
	Guardrail BedrockGuardrail `yaml:"guardrail"`
	rest      rest
}

type bedrockFields BedrockConfig

// Decodes the mapping and keeps unknown keys
func (b *BedrockConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*bedrockFields)(b), &b.rest)
}

// Writes the mapping with unknown keys after the named ones
func (b BedrockConfig) MarshalYAML() (any, error) {
	return encodeMapping(bedrockFields(b), b.rest)
}

// Bedrock model discovery
type BedrockDiscovery struct {
	Enabled         bool     `yaml:"enabled"`
	ProviderFilter  []string `yaml:"provider_filter"`
	RefreshInterval float64  `yaml:"refresh_interval"`
	rest            rest
}

type bedrockDiscoveryFields BedrockDiscovery

// Decodes the mapping and keeps unknown keys
func (b *BedrockDiscovery) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*bedrockDiscoveryFields)(b), &b.rest)
}

// Writes the mapping with unknown keys after the named ones
func (b BedrockDiscovery) MarshalYAML() (any, error) {
	return encodeMapping(bedrockDiscoveryFields(b), b.rest)
}

// Bedrock Guardrails binding
type BedrockGuardrail struct {
	GuardrailIdentifier  string         `yaml:"guardrail_identifier"`
	GuardrailVersion     string         `yaml:"guardrail_version"`
	StreamProcessingMode StreamProcMode `yaml:"stream_processing_mode"`
	Trace                GuardrailTrace `yaml:"trace"`
	rest                 rest
}

type bedrockGuardrailFields BedrockGuardrail

// Decodes the mapping and keeps unknown keys
func (b *BedrockGuardrail) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*bedrockGuardrailFields)(b), &b.rest)
}

// Writes the mapping with unknown keys after the named ones
func (b BedrockGuardrail) MarshalYAML() (any, error) {
	return encodeMapping(bedrockGuardrailFields(b), b.rest)
}

// Guardrail stream processing
type StreamProcMode string

// Stream processing modes
const (
	StreamProcSync  StreamProcMode = "sync"
	StreamProcAsync StreamProcMode = "async"
)

// Accepted stream processing modes
func StreamProcModes() []StreamProcMode {
	return []StreamProcMode{StreamProcSync, StreamProcAsync}
}

// Guardrail trace level
type GuardrailTrace string

// Guardrail trace levels
const (
	TraceEnabled     GuardrailTrace = "enabled"
	TraceDisabled    GuardrailTrace = "disabled"
	TraceEnabledFull GuardrailTrace = "enabled_full"
)

// Accepted guardrail trace levels
func GuardrailTraces() []GuardrailTrace {
	return []GuardrailTrace{TraceEnabled, TraceDisabled, TraceEnabledFull}
}

// Defaults of the compression section
func defaultCompression() CompressionConfig {
	return CompressionConfig{
		Enabled:                           true,
		Threshold:                         0.50,
		TargetRatio:                       0.20,
		TailMode:                          TailLean,
		ProtectLastN:                      20,
		MinTailUserMessages:               1,
		MaxAttempts:                       3,
		ProactivePruneMinResultChars:      8000,
		ProactivePruneMinReclaimTokens:    4096,
		MicroCompactEveryNTurns:           1,
		MicroCompactDefragThresholdTokens: 2000,
		HygieneHardMessageLimit:           5000,
		HygieneTimeoutSeconds:             30,
		HygieneTotalCeilingSeconds:        600,
		HygieneFailureCooldownSeconds:     300,
		HygieneMaxTurnHoldSeconds:         10,
		ContextTimeoutSeconds:             120,
		ContextTotalCeilingSeconds:        600,
		ProtectFirstN:                     3,
		CodexGPT55Autoraise:               true,
		CodexGPT55AutoraiseNotice:         true,
		CodexAppServerAuto:                CodexCompactNative,
		InPlace:                           true,
	}
}

// Defaults of the provider request sections
func defaultBedrock() BedrockConfig {
	return BedrockConfig{
		Discovery: BedrockDiscovery{Enabled: true, ProviderFilter: []string{}, RefreshInterval: 3600},
		Guardrail: BedrockGuardrail{StreamProcessingMode: StreamProcAsync, Trace: TraceDisabled},
	}
}

// Documentation of the compression and provider request sections
func compressionDocs() map[string]string {
	return map[string]string{
		"compression.enabled":                               "Compress the conversation when context usage passes the threshold.",
		"compression.checkpoint_required":                   "Fail closed before lossy compaction unless an active memory provider confirms checkpoint API compatibility and completes the checkpoint.",
		"compression.progress_notices":                      "When true, routine compression progress statuses (compacting, preflight, pre-API, idle, retry) reach chat gateways instead of being filtered as noise. Failure notices and manual /compress feedback are always visible.",
		"compression.threshold":                             "Compress when context usage exceeds this ratio. Models with windows below 512K are floored at 0.75 (raise-only) so compaction doesn't fire with half the window free; set above 0.75 to override the floor.",
		"compression.threshold_tokens":                      "Optional absolute token cap: when set, compression triggers at the lower of the ratio threshold and this count. Clamped to the model's context length. Off by default: no single count suits windows from 64K to 1M+, so the ratio decides.",
		"compression.target_ratio":                          "Fraction of threshold to preserve as recent tail.",
		"compression.tail_mode":                             "\"lean\" = clamped 2.5%-of-window tail (10K floor, 25K cap) plus chunked digests, anchor index, verbatim user messages and session_search pointers in the summary (~3x fewer retained tokens; a few extra summarizer calls at the boundary). \"legacy\" = 0.20 x threshold verbatim tail (100-240K tokens on big windows).",
		"compression.protect_last_n":                        "Minimum recent messages kept uncompressed, honoured up to a small count floor; the verbatim tail is otherwise token-bounded and never above 20% of the window.",
		"compression.min_tail_user_messages":                "REAL (actionable) user messages guaranteed to survive in the tail. 1 = single last-user anchor; raise (e.g. 3) when bulky tool outputs fill the tail budget.",
		"compression.max_attempts":                          "Retry rounds before a turn gives up with \"max compression attempts reached\". Raise (e.g. 6) for tool-schema-heavy sessions. Validated >= 1, cap 10.",
		"compression.proactive_prune_tokens":                "Opt-in trigger (tokens) for the deterministic no-LLM tool-result prune, independent of `threshold` (which rarely fires on large windows, so old tool output is re-sent every turn); e.g. 48000 reclaims early. 0 = off. Tail protected by `protect_last_n`. Built-in compressor only. Each committed prune rewrites sent history and breaks the prompt-cache prefix; the min_reclaim gate below keeps those breaks episodic.",
		"compression.proactive_prune_min_result_chars":      "The prune's summarize pass only touches tool results larger than this (chars); clamped >= 200 so a generated summary can't be re-summarized.",
		"compression.proactive_prune_min_reclaim_tokens":    "A prune only commits when it reclaims at least this many tokens, then waits for a trigger-sized runway to regrow before rearming. 0 = no minimum-savings gate.",
		"compression.micro_compact":                         "Opt-in: after each turn fold the oldest un-absorbed exchange into a rolling summary, amortizing compression cost. Off by default because every pass rewrites sent history and breaks the prompt-cache prefix EVERY turn; enable only if the amortized stall beats the cached-prefix discount.",
		"compression.micro_compact_every_n_turns":           "Cadence: run a pass every Nth completed turn (1 = one cache break per turn, 5 = a fifth of the breaks). Clamped >= 1; ignored unless micro_compact is true.",
		"compression.micro_compact_defrag_threshold_tokens": "Once the rolling summary exceeds this many tokens, the next pass re-summarizes it.",
		"compression.hygiene_hard_message_limit":            "Gateway session-hygiene force-compress threshold, by message count.",
		"compression.hygiene_timeout_seconds":               "Max seconds the gateway waits for pre-agent hygiene compression WITHOUT forward progress. Inactivity budget: a slow model still streaming tokens extends the wait.",
		"compression.hygiene_total_ceiling_seconds":         "Absolute cap on the hygiene wait even while tokens are moving (bounds a trickle stream). Clamped >= hygiene_timeout_seconds.",
		"compression.hygiene_failure_cooldown_seconds":      "Skip repeated failed hygiene attempts for this many seconds.",
		"compression.hygiene_max_turn_hold_seconds":         "Max seconds an ARRIVING user turn is held while a streaming hygiene summary finishes; bounds user-visible latency (keep under chat idle timeouts, Telegram ~30s). On expiry the turn proceeds uncompressed; the detached worker keeps its watermark-fenced commit, so the summary is adopted at the next safe boundary.",
		"compression.context_timeout_seconds":               "Inactivity budget for in-agent compress_context (loop, /compress, preflight); same progress-aware semantics as hygiene_timeout_seconds. 0 = disable the owned wrapper (callers passing commit_fence, e.g. gateway hygiene, never use it). Floored at the auxiliary compression request timeout (auxiliary.compression.timeout, min 300s): the host never judges silence before the summary request itself would time out.",
		"compression.context_total_ceiling_seconds":         "Absolute cap on the *pre-commit* compress_context wait (summary and stream phase) even while tokens move. Clamped >= context_timeout_seconds when that is > 0. A started session database commit is never abandoned: past the ceiling it is logged (WARNING, then ERROR) and surfaced on the warning channel while the host keeps waiting.",
		"compression.protect_first_n":                       "Non-system head messages always kept verbatim, in ADDITION to the (always protected) system prompt. 0 = pin nothing but system prompt + summary + tail.",
		"compression.abort_on_summary_failure":              "When true, auto-compression whose summary fails (aux error, non-JSON, timeout) aborts instead of dropping the middle with a \"summary unavailable\" marker; the session freezes at its size until /compress (bypasses the cooldown) or /new.",
		"compression.codex_gpt55_autoraise":                 "(Historical key name.) When true, gpt-5.4/5.5/5.6 and gpt-6 Astra (any slug containing \"astra\" without \"900k\") on the ChatGPT Codex OAuth route raise their compaction trigger to 85%: Codex hard-caps them at a 272K window, so the global 50% would compact at ~136K. false = global `threshold`. Only that route; the same models via OpenAI direct, OpenRouter or another relay keep the global value.",
		"compression.codex_gpt55_autoraise_notice":          "Show the one-time autoraise banner; false keeps the autoraise, hides the notice.",
		"compression.codex_app_server_auto":                 "Codex app-server thread compaction mode. The codex agent owns the thread context, so icebath's summarizer cannot shrink it. native = codex decides; icebath = icebath's threshold triggers thread/compact/start; off = never auto-trigger.",
		"compression.codex_responses_native":                "Opt in to OpenAI server-side compaction on the Responses API. Only gpt-5.6-family on api.openai.com or the Codex backend; local compression stays as fallback.",
		"compression.codex_responses_compact_threshold":     "Absolute server compaction trigger (input tokens). null follows the local trigger with a safety margin; explicit values only clamp downward so the server goes first.",
		"compression.in_place":                              "Compaction rewrites the message list and system prompt WITHOUT rotating the session id (no parent_session_id chain, no `name #N` renumbering), avoiding the session-rotation bug cluster. Pre-compaction turns are soft-archived under the same id (active=0, compacted=1), still session_search-able. false = legacy rotating-compaction path.",
		"compression.model_thresholds":                      "Per-model threshold overrides: keys substring-match the model name (longest wins), values replace the global `threshold`, e.g. {\"glm-5.2\": 0.40}. Prefix a key with \"<provider>:\" to scope it to one route ({\"openai-codex:astra\": 0.85} leaves Astra on OpenRouter or Portal at the global value). The <512K floor (0.75) still applies raise-only on top.",
		"compression.idle_compact_after_seconds":            "Opt-in idle compaction (0 = off): a session resuming after this many idle seconds compacts up front, before the first reply. Time-based complement to `threshold`; skipped when already at or below threshold x target_ratio; honors the same cooldown, anti-thrash and lock guards. Example: 1800 = 30 min.",
		"compression.prepare_ahead":                         "Opt-in: write the next summary in the background near the trigger; compaction uses (or waits for) it if it still covers the same messages.",
		"compression.summary_model":                         "Deprecated; use auxiliary.compression.",
		"compression.summary_provider":                      "Deprecated; use auxiliary.compression.",
		"compression.summary_base_url":                      "Deprecated; use auxiliary.compression.",
		"prompt_caching.cache_ttl":                          "Anthropic prompt caching (Claude via OpenRouter or native API): \"5m\" | \"1h\" | \"auto\" (auto = 1h for human-paced sessions, cli/tui/desktop/messaging, and 5m for subagent, cron, oneshot, webhook, kanban, api, tool, batch); other non-falsy values are ignored; falsy (false, null, \"off\", \"disabled\", \"no\", \"none\") disables caching.",
		"openrouter.response_cache":                         "X-OpenRouter-Cache header: identical requests return cached responses at zero billing; independent of Anthropic prompt caching.",
		"openrouter.response_cache_ttl":                     "Seconds (1-86400), only used when response_cache is on.",
		"openrouter.min_coding_score":                       "Pareto-code router knob (0.0-1.0), applied only when the model is \"openrouter/pareto-code\"; higher = stronger and pricier coders, 0.65 = mid-tier, \"\" = let OpenRouter pick the strongest. Docs: openrouter.ai/docs/guides/routing/routers/pareto-router.",
		"bedrock":                                           "AWS Bedrock; only used when model.provider is \"bedrock\".",
		"bedrock.region":                                    "Empty = AWS_REGION env var, then us-east-1.",
		"bedrock.discovery.enabled":                         "Auto-discover models via ListFoundationModels.",
		"bedrock.discovery.provider_filter":                 "Restrict discovery to these providers, e.g. [\"anthropic\", \"amazon\"].",
		"bedrock.discovery.refresh_interval":                "Cache discovery results for this many seconds.",
		"bedrock.guardrail":                                 "Bedrock Guardrails: create one in the console, then set ID and version. https://docs.aws.amazon.com/bedrock/latest/userguide/guardrails.html",
		"bedrock.guardrail.guardrail_identifier":            "Guardrail ID, e.g. \"abc123def456\".",
		"bedrock.guardrail.guardrail_version":               "Guardrail version, e.g. \"1\" or \"DRAFT\".",
		"bedrock.guardrail.stream_processing_mode":          "\"sync\" | \"async\".",
		"bedrock.guardrail.trace":                           "\"enabled\" | \"disabled\" | \"enabled_full\".",
	}
}
