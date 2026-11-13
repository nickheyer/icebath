package config

import (
	"math"
	"reflect"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Agent section
type AgentConfig struct {
	MaxTurns                          *TurnLimit           `yaml:"max_turns"`
	BudgetWarningRatio                *float64             `yaml:"budget_warning_ratio" coerce:"strict"`
	RunBudgetSeconds                  *float64             `yaml:"run_budget_seconds" coerce:"strict"`
	GatewayTimeout                    float64              `yaml:"gateway_timeout" coerce:"strict"`
	GatewayTurnLeaseTimeout           float64              `yaml:"gateway_turn_lease_timeout" coerce:"strict"`
	AgentCache                        AgentCacheConfig     `yaml:"agent_cache"`
	RestartDrainTimeout               float64              `yaml:"restart_drain_timeout" coerce:"strict"`
	CronDrainTimeout                  float64              `yaml:"cron_drain_timeout" coerce:"strict"`
	RestartAfterTurnTimeout           float64              `yaml:"restart_after_turn_timeout" coerce:"strict"`
	BuildWaitTimeout                  float64              `yaml:"build_wait_timeout"`
	APIMaxRetries                     int                  `yaml:"api_max_retries"`
	AutoRecoveryCycles                int                  `yaml:"auto_recovery_cycles"`
	StreamDrainTimeout                float64              `yaml:"stream_drain_timeout" coerce:"exact"`
	EmptyResponseGuard                EmptyResponseGuard   `yaml:"empty_response_guard"`
	ServiceTier                       ServiceTier          `yaml:"service_tier"`
	FastAutoSeconds                   float64              `yaml:"fast_auto_seconds"`
	TextVerbosity                     TextVerbosity        `yaml:"text_verbosity"`
	Daybreak                          bool                 `yaml:"daybreak" coerce:"strict"`
	ToolUseEnforcement                ModelGate            `yaml:"tool_use_enforcement"`
	ExecutionGuidance                 ModelGate            `yaml:"execution_guidance"`
	IntentAckContinuation             ModelGate            `yaml:"intent_ack_continuation"`
	StallGuards                       bool                 `yaml:"stall_guards" coerce:"bool"`
	TaskCompletionGuidance            bool                 `yaml:"task_completion_guidance" coerce:"bool"`
	ParallelToolCallGuidance          bool                 `yaml:"parallel_tool_call_guidance" coerce:"bool"`
	EnvironmentProbe                  bool                 `yaml:"environment_probe" coerce:"bool"`
	BotModeProtocol                   bool                 `yaml:"bot_mode_protocol" coerce:"bool"`
	EnvironmentHint                   string               `yaml:"environment_hint"`
	CodingContext                     CodingContextMode    `yaml:"coding_context"`
	CodingInstructions                StringList           `yaml:"coding_instructions"`
	VerifyGuidance                    bool                 `yaml:"verify_guidance"`
	MaxVerifyNudges                   int                  `yaml:"max_verify_nudges"`
	VerifyOnStop                      Scalar               `yaml:"verify_on_stop"`
	GatewayTimeoutWarning             float64              `yaml:"gateway_timeout_warning" coerce:"strict"`
	ClarifyTimeout                    int                  `yaml:"clarify_timeout"`
	GatewayNotifyInterval             float64              `yaml:"gateway_notify_interval" coerce:"strict"`
	SessionStallTimeout               float64              `yaml:"session_stall_timeout" coerce:"strict"`
	SanitizerHealEscalationThreshold  int                  `yaml:"sanitizer_heal_escalation_threshold"`
	ReconnectAttentionAfter           float64              `yaml:"reconnect_attention_after"`
	GatewayAutoContinueFreshness      float64              `yaml:"gateway_auto_continue_freshness" coerce:"strict"`
	GatewayStartupRestoreDrainTimeout float64              `yaml:"gateway_startup_restore_drain_timeout" coerce:"strict"`
	GatewayStartupWarmupTimeout       float64              `yaml:"gateway_startup_warmup_timeout" coerce:"strict"`
	LocalStreamStaleTimeout           float64              `yaml:"local_stream_stale_timeout" coerce:"exact"`
	StreamMaxLifetime                 float64              `yaml:"stream_max_lifetime" coerce:"exact"`
	ImageInputMode                    ImageInputMode       `yaml:"image_input_mode" coerce:"exact"`
	DisabledToolsets                  StringList           `yaml:"disabled_toolsets"`
	ReasoningOverrides                Map[ReasoningEffort] `yaml:"reasoning_overrides"`
	ReasoningEcho                     bool                 `yaml:"reasoning_echo"`
	TurnLiveness                      TurnLiveness         `yaml:"turn_liveness"`
	ReasoningEffort                   *ReasoningEffort     `yaml:"reasoning_effort,omitempty"`
	SystemPrompt                      StringList           `yaml:"system_prompt,omitempty"`
	Personalities                     Map[Personality]     `yaml:"personalities,omitempty"`
	rest                              rest
}

type agentFields AgentConfig

// Decodes the mapping and keeps unknown keys
func (a *AgentConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*agentFields)(a), &a.rest)
}

// Writes the mapping with unknown keys after the named ones
func (a AgentConfig) MarshalYAML() (any, error) {
	return encodeMapping(agentFields(a), a.rest)
}

// Per-session agent cache of the gateway
type AgentCacheConfig struct {
	MaxSize             int     `yaml:"max_size" coerce:"strict"`
	IdleTTLSecs         float64 `yaml:"idle_ttl_secs" coerce:"strict"`
	MemoryHighMB        Scalar  `yaml:"memory_high_mb"`
	MaxEvictionsPerPass int     `yaml:"max_evictions_per_pass" coerce:"strict"`
	ProtectRecent       int     `yaml:"protect_recent" coerce:"strict"`
	rest                rest
}

type agentCacheFields AgentCacheConfig

// Decodes the mapping and keeps unknown keys
func (a *AgentCacheConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*agentCacheFields)(a), &a.rest)
}

// Writes the mapping with unknown keys after the named ones
func (a AgentCacheConfig) MarshalYAML() (any, error) {
	return encodeMapping(agentCacheFields(a), a.rest)
}

// Retry guard for empty model responses
type EmptyResponseGuard struct {
	Enabled          bool    `yaml:"enabled" coerce:"notfalse"`
	CostThresholdUSD float64 `yaml:"cost_threshold_usd" coerce:"strict"`
	rest             rest
}

type emptyResponseGuardFields EmptyResponseGuard

// Decodes the mapping and keeps unknown keys
func (e *EmptyResponseGuard) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*emptyResponseGuardFields)(e), &e.rest)
}

// Writes the mapping with unknown keys after the named ones
func (e EmptyResponseGuard) MarshalYAML() (any, error) {
	return encodeMapping(emptyResponseGuardFields(e), e.rest)
}

// Watchdog that interrupts a turn without observable progress
type TurnLiveness struct {
	TimeoutS float64 `yaml:"timeout_s"`
	PollS    float64 `yaml:"poll_s"`
	rest     rest
}

type turnLivenessFields TurnLiveness

// Decodes the mapping and keeps unknown keys
func (t *TurnLiveness) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*turnLivenessFields)(t), &t.rest)
}

// Writes the mapping with unknown keys after the named ones
func (t TurnLiveness) MarshalYAML() (any, error) {
	return encodeMapping(turnLivenessFields(t), t.rest)
}

// Iteration cap meaning no limit at all
const TurnLimitUnlimited = math.MaxInt

// Turn cap written as a number, a numeric string, or a word meaning unlimited, kept as written
type TurnLimit struct {
	node yaml.Node
}

// Turn cap of n iterations
func TurnLimitOf(n int) *TurnLimit {
	return &TurnLimit{node: yaml.Node{Kind: yaml.ScalarNode, Tag: tagInt, Value: strconv.Itoa(n)}}
}

// Keeps any value as written, since an unreadable cap falls back to the default rather than failing the load
func (t *TurnLimit) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	t.node = *plainCopy(n)
	return nil
}

// Writes the value exactly as read
func (t TurnLimit) MarshalYAML() (any, error) {
	return emitted(&t.node), nil
}

// Words that mean no turn cap, compared lowercased and trimmed
func unlimitedTurnWords() []string {
	return []string{"none", "null", "unlimited", "infinite", "infinity", "inf", "∞", "-1", "0"}
}

// Iteration cap of at least 1, def for an unset or unreadable value, unlimited for zero, negatives and unlimited words
func (t *TurnLimit) Resolve(def int) int {
	if t == nil {
		return def
	}
	n := t.node
	if n.Kind != yaml.ScalarNode {
		return def
	}
	var v int
	switch n.ShortTag() {
	case tagNull, tagBool:
		return def
	case tagInt, tagFloat:
		f, ok := floatOf(&n, n.ShortTag(), coerceDefault)
		if !ok || math.IsNaN(f) {
			return def
		}
		if f >= math.MaxInt {
			return TurnLimitUnlimited
		}
		if f <= math.MinInt {
			return TurnLimitUnlimited
		}
		v = int(f)
	default:
		s := strings.ToLower(strings.TrimSpace(n.Value))
		if s == "" {
			return def
		}
		for _, w := range unlimitedTurnWords() {
			if s == w {
				return TurnLimitUnlimited
			}
		}
		i, err := strconv.Atoi(s)
		if err != nil {
			f, ferr := strconv.ParseFloat(s, 64)
			if ferr != nil || math.IsNaN(f) || math.IsInf(f, 0) {
				return def
			}
			if f >= math.MaxInt {
				return TurnLimitUnlimited
			}
			i = int(f)
		}
		v = i
	}
	if v <= 0 {
		return TurnLimitUnlimited
	}
	return v
}

// Reasoning effort written as a level word, a boolean, or a mapping of enabled and effort
type ReasoningEffort struct {
	Level   Scalar
	Enabled Scalar
	Effort  Scalar
	Mapping bool
	rest    rest
}

// Effort written as a level word
func EffortLevel(level string) ReasoningEffort {
	return ReasoningEffort{Level: Text(level)}
}

// Accepts a scalar level or a mapping with enabled and effort, other values under those keys kept as written
func (r *ReasoningEffort) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	switch n.Kind {
	case yaml.ScalarNode:
		*r = ReasoningEffort{}
		return r.Level.UnmarshalYAML(n)
	case yaml.MappingNode:
		out := ReasoningEffort{Mapping: true}
		for _, p := range mappingPairs(n) {
			switch {
			case p.key.Value == "enabled" && p.value.Kind == yaml.ScalarNode:
				err = out.Enabled.UnmarshalYAML(p.value)
			case p.key.Value == "effort" && p.value.Kind == yaml.ScalarNode:
				err = out.Effort.UnmarshalYAML(p.value)
			default:
				out.rest.set(p.key, p.value)
			}
			if err != nil {
				return err
			}
		}
		*r = out
		return nil
	}
	return shapeError(n, yaml.ScalarNode)
}

// Writes the form that was read
func (r ReasoningEffort) MarshalYAML() (any, error) {
	if !r.Mapping {
		return r.Level.MarshalYAML()
	}
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: tagMap}
	for _, f := range []struct {
		key string
		v   Scalar
	}{{"enabled", r.Enabled}, {"effort", r.Effort}} {
		if f.v.IsZero() {
			continue
		}
		vn, err := encodeValue(reflect.ValueOf(f.v))
		if err != nil {
			return nil, err
		}
		n.Content = append(n.Content, strNode(f.key), vn)
	}
	n.Content = append(n.Content, r.rest.emit()...)
	return n, nil
}

// Reasoning levels a bare word may name
func ReasoningLevels() []string {
	return []string{"minimal", "low", "medium", "high", "xhigh", "max", "ultra"}
}

// Reasoning request the value asks for, set false when the caller keeps its default
func (r *ReasoningEffort) Parse() (enabled bool, level string, set bool) {
	if r == nil {
		return false, "", false
	}
	if r.Mapping {
		if v, ok := r.Enabled.AsBool(); ok && !v {
			return false, "", true
		}
		if lv := strings.TrimSpace(scalarTruthText(r.Effort)); lv != "" {
			return true, lv, true
		}
		return false, "", false
	}
	if b, ok := r.Level.AsBool(); ok && b {
		return false, "", false
	}
	word := strings.ToLower(strings.TrimSpace(r.Level.String()))
	switch word {
	case "none", "false", "disabled":
		return false, "", true
	}
	for _, lv := range ReasoningLevels() {
		if word == lv {
			return true, word, true
		}
	}
	return false, "", false
}

// Text of a scalar, empty for a false, zero, null or empty value
func scalarTruthText(s Scalar) string {
	if v, ok := s.AsBool(); ok && !v {
		return ""
	}
	if f, ok := s.AsFloat(); ok && f == 0 {
		return ""
	}
	if s.Kind() == KindNull {
		return ""
	}
	return s.String()
}

// Fast mode service tier word as written, read through Wire into the tier requests carry
type ServiceTier string

// Service tiers requests carry, the empty tier being normal
const (
	ServiceTierNormal    ServiceTier = ""
	ServiceTierPriority  ServiceTier = "priority"
	ServiceTierUltrafast ServiceTier = "ultrafast"
	ServiceTierAuto      ServiceTier = "auto"
	ServiceTierCold      ServiceTier = "cold"
)

// Accepted service tier words, normal ones first, then the words of each tier
func ServiceTiers() []ServiceTier {
	return []ServiceTier{"", "normal", "default", "standard", "off", "none", "false", "fast", "priority", "on", "ultrafast", "ultra", "auto", "cold"}
}

// Tier the word selects, normal for the normal words and a YAML false, ok false for an unknown word
func (t ServiceTier) Wire() (ServiceTier, bool) {
	switch strings.ToLower(strings.TrimSpace(string(t))) {
	case "", "normal", "default", "standard", "off", "none", "false":
		return ServiceTierNormal, true
	case "fast", "priority", "on":
		return ServiceTierPriority, true
	case "ultrafast", "ultra":
		return ServiceTierUltrafast, true
	case "auto":
		return ServiceTierAuto, true
	case "cold":
		return ServiceTierCold, true
	}
	return ServiceTierNormal, false
}

// Responses API text verbosity
type TextVerbosity string

// Text verbosity levels, empty sends nothing
const (
	VerbosityUnset  TextVerbosity = ""
	VerbosityLow    TextVerbosity = "low"
	VerbosityMedium TextVerbosity = "medium"
	VerbosityHigh   TextVerbosity = "high"
)

// Accepted text verbosity values
func TextVerbosities() []TextVerbosity {
	return []TextVerbosity{VerbosityUnset, VerbosityLow, VerbosityMedium, VerbosityHigh}
}

// Coding posture mode
type CodingContextMode string

// Coding posture modes
const (
	CodingAuto  CodingContextMode = "auto"
	CodingFocus CodingContextMode = "focus"
	CodingOn    CodingContextMode = "on"
	CodingOff   CodingContextMode = "off"
)

// Accepted coding posture words, each canonical mode followed by its aliases
func CodingContextModes() []CodingContextMode {
	return []CodingContextMode{
		CodingAuto, CodingFocus, "strict", "lean", CodingOn, "true", "yes", "1", "always", CodingOff, "false", "no", "0", "never",
	}
}

// Mode with the reader aliases folded in, auto for anything unrecognized
func (m CodingContextMode) Canonical() CodingContextMode {
	switch strings.ToLower(strings.TrimSpace(string(m))) {
	case "focus", "strict", "lean":
		return CodingFocus
	case "on", "true", "yes", "1", "always":
		return CodingOn
	case "off", "false", "no", "0", "never":
		return CodingOff
	}
	return CodingAuto
}

// How user-attached images reach the main model
type ImageInputMode string

// Image input modes
const (
	ImageInputAuto   ImageInputMode = "auto"
	ImageInputNative ImageInputMode = "native"
	ImageInputText   ImageInputMode = "text"
)

// Accepted image input modes
func ImageInputModes() []ImageInputMode {
	return []ImageInputMode{ImageInputAuto, ImageInputNative, ImageInputText}
}

// Defaults of the agent section
func defaultAgent() AgentConfig {
	return AgentConfig{
		GatewayTimeout:          1800,
		GatewayTurnLeaseTimeout: 5,
		AgentCache: AgentCacheConfig{
			MaxSize:             128,
			IdleTTLSecs:         3600,
			MemoryHighMB:        Text("auto"),
			MaxEvictionsPerPass: 16,
			ProtectRecent:       8,
		},
		RestartDrainTimeout:               0,
		CronDrainTimeout:                  30,
		RestartAfterTurnTimeout:           1800,
		BuildWaitTimeout:                  600,
		APIMaxRetries:                     3,
		AutoRecoveryCycles:                5,
		StreamDrainTimeout:                2.0,
		EmptyResponseGuard:                EmptyResponseGuard{Enabled: true, CostThresholdUSD: 0.25},
		ServiceTier:                       ServiceTierNormal,
		FastAutoSeconds:                   60,
		TextVerbosity:                     VerbosityUnset,
		ToolUseEnforcement:                AutoGate(),
		ExecutionGuidance:                 AutoGate(),
		IntentAckContinuation:             AutoGate(),
		StallGuards:                       true,
		TaskCompletionGuidance:            true,
		ParallelToolCallGuidance:          true,
		EnvironmentProbe:                  true,
		BotModeProtocol:                   true,
		CodingContext:                     CodingAuto,
		VerifyGuidance:                    true,
		MaxVerifyNudges:                   3,
		VerifyOnStop:                      Bool(false),
		GatewayTimeoutWarning:             900,
		ClarifyTimeout:                    3600,
		GatewayNotifyInterval:             180,
		SessionStallTimeout:               300,
		SanitizerHealEscalationThreshold:  3,
		ReconnectAttentionAfter:           7200,
		GatewayAutoContinueFreshness:      3600,
		GatewayStartupRestoreDrainTimeout: 30,
		GatewayStartupWarmupTimeout:       20,
		LocalStreamStaleTimeout:           900,
		StreamMaxLifetime:                 0,
		ImageInputMode:                    ImageInputAuto,
		DisabledToolsets:                  ListOf(),
		TurnLiveness:                      TurnLiveness{TimeoutS: 600.0, PollS: 15.0},
	}
}

// Documentation of the agent section
func agentDocs() map[string]string {
	return map[string]string{
		"agent.max_turns":                               "Turn cap. null = unlimited (default; caps caused silent mid-task truncation). A positive int caps; \"none\", \"unlimited\", \"inf\", 0 and -1 also mean unlimited.",
		"agent.budget_warning_ratio":                    "Optional one-time model-visible checkpoint warning before a finite turn cap is exhausted. null = off; set a ratio strictly between 0 and 1 (for example, 0.75).",
		"agent.run_budget_seconds":                      "Wall-clock budget (seconds) per run. null = off. When set, a one-time wrap-up notice fires at 80% elapsed and implicit provider stale timeouts are capped to the remaining budget. CLI equivalent: `icebath chat --run-budget N`.",
		"agent.gateway_timeout":                         "Gateway inactivity timeout (seconds). Only fires when the agent is completely idle, not while calling tools or receiving API responses. 0 = unlimited.",
		"agent.gateway_turn_lease_timeout":              "Max seconds an alias routing key waits for the active turn holding the same session lease; on expiry the message is rejected with a resend notice. Keep short: Telegram dispatches sequentially, so a waiter delays unrelated topics. Non-positive -> 5s.",
		"agent.agent_cache":                             "Per-session agent cache in the gateway. Each entry keeps a warm prompt prefix AND the full transcript: too small re-pays uncached prompts, too large fills the heap.",
		"agent.agent_cache.max_size":                    "LRU entry cap.",
		"agent.agent_cache.idle_ttl_secs":               "Evict agents idle this long (seconds).",
		"agent.agent_cache.memory_high_mb":              "Anonymous-RSS budget (MB) above which LRU transcripts are shed (reloaded from disk next turn). \"auto\" = derive from the cgroup memory limit (or total RAM); a number = explicit; 0 or off = disable the pass.",
		"agent.agent_cache.max_evictions_per_pass":      "Max sessions shed per pass, so teardown bursts cannot stall the gateway.",
		"agent.agent_cache.protect_recent":              "Number of most-recently-used sessions the shedding pass never touches.",
		"agent.restart_drain_timeout":                   "Force-interrupt budget (seconds) once gateway stop or drain has begun (SIGTERM, and the final phase of in-band restart). 0 = interrupt immediately. Keep under systemd TimeoutStopSec or risk SIGKILL mid-cleanup; for /restart prefer restart_after_turn_timeout so turns finish BEFORE stop.",
		"agent.cron_drain_timeout":                      "Floor under the stop/drain wait (seconds) for cron jobs and api_server runs. Interrupted chat turns resume on the next message, but an interrupted cron run is recorded as a permanent failure and an interrupted /v1 run fails its waiting caller, so neither may inherit restart_drain_timeout's 0. Clamped to the shutdown-watchdog leash minus teardown headroom (~50s unless TimeoutStopSec is raised). 0 = opt out.",
		"agent.restart_after_turn_timeout":              "In-band restart (/restart, SIGUSR1): refuse new work, then wait up to this many seconds for in-flight agents, cron and api runs to finish before stop. 0 = enter stop at once. The 30 min default is a safety valve for wedged agents, not a target latency; an interactive `icebath gateway restart` must never block for hours on a wedged turn. Raise for long unattended turns.",
		"agent.build_wait_timeout":                      "Max seconds a submitted prompt waits for the deferred agent build (MCP discovery, model metadata, skills scan) before failing visibly. The prompt is delivered as soon as the build completes (progress notice past 30s), so this only fires on a hung build. Raise for many slow or unreachable MCP servers.",
		"agent.api_max_retries":                         "icebath-level retry attempts for API errors (connection drops, timeouts, 5xx) wrapping the whole call; the OpenAI SDK also retries transient errors (max_retries=2). Set 1 for fast failover to fallback providers; raise to tolerate longer provider hiccups.",
		"agent.auto_recovery_cycles":                    "Once api_max_retries AND the fallback chain are spent on a transient outage (5xx, overloaded/529, connect or read timeouts) with nothing delivered yet, wait and retry this many more cycles (jittered 15/30/60/60/60s; a provider Retry-After wins up to 120s) with a visible \"retrying automatically\" countdown instead of ending the turn. Esc or interrupt stops the wait; auth, format, billing and policy errors never enter. 0 disables.",
		"agent.stream_drain_timeout":                    "Seconds the Codex/Responses stream may keep reading after its terminal frame so the relay finalizer can run. Relays that never close the SSE socket after response.completed would otherwise wedge the turn until the idle watchdog discards the already-billed response. 0 skips the drain. Well-behaved endpoints close immediately and never wait this long.",
		"agent.empty_response_guard":                    "Empty-response retry guard. Empty retries re-send the full input at full price; this stops re-billing deterministic empties (unsignaled refusals, zero output tokens) while failing open on ambiguous evidence (missing usage, any tokens, model or provider change).",
		"agent.empty_response_guard.enabled":            "false = legacy fixed 3 retries unconditionally.",
		"agent.empty_response_guard.cost_threshold_usd": "When one empty attempt's estimated input cost >= this USD, the streak's retry budget drops from 3 to 1. Unknown pricing or missing usage leaves it untouched.",
		"agent.service_tier":                            "Fast mode: \"\" or \"normal\" (off), \"fast\" (always), \"auto\" (first fast_auto_seconds of every turn), \"cold\" (first turn of a session only).",
		"agent.fast_auto_seconds":                       "Seconds of every turn that run on the fast tier when service_tier is auto.",
		"agent.text_verbosity":                          "Responses-only `text.verbosity` (never chat_completions): \"\" = not sent, or low | medium | high.",
		"agent.daybreak":                                "Opt-in; only catalog-listed Codex models.",
		"agent.tool_use_enforcement":                    "System-prompt guidance telling the model to call tools instead of describing actions. \"auto\" = gpt/codex models; true/false = force for all models; or a list of model-name substrings (e.g. [\"gpt\", \"codex\", \"gemini\", \"qwen\"]).",
		"agent.execution_guidance":                      "Execution-discipline prompt block (tool persistence, tools for arithmetic and system facts, read-back after external writes, count reconciliation, literal identifiers, verification-gated completion). Chosen once per session by model name (byte-stable). \"auto\" = gpt/codex/grok/deepseek/kimi/qwen/glm/minimax/mimo/mistral; true/false = force; or a list of model-name substrings.",
		"agent.intent_ack_continuation":                 "When the model narrates an action (\"I'll go check the logs...\") but emits no tool call, inject a \"continue now, execute the tools\" nudge and loop (max 2 nudges per turn). Corrective sibling of tool_use_enforcement. \"auto\" = codex_responses api_mode only; true = all api_modes (fixes Gemini/Claude \"stops after stating intent\"); false = never; or a list of model-name substrings.",
		"agent.stall_guards":                            "Anti-stall guards: (1) the identical-call loop breaker appends a notice when the same tool is called 3+ times with identical args AND results (never blocks; pollers like `process` exempt); (2) the continue-intent extension of empty-response recovery re-prompts once when the model says it will continue but takes no action. false disables both.",
		"agent.task_completion_guidance":                "\"Finish the job\" prompt block for all models: don't stop at a stub, never fabricate output when the real path is blocked. ~80 cached tokens. false disables.",
		"agent.parallel_tool_call_guidance":             "Prompt block for all models steering independent tool calls (reads, searches, fetches, read-only commands) into one batched turn; the runtime already runs them concurrently. ~70 cached tokens. false disables.",
		"agent.environment_probe":                       "Toolchain probe: surfaces the interpreter, pip, uv and PEP-668 state in the system prompt only when something non-default is detected (no pip module, pip and interpreter mismatch, PEP 668 without uv); zero tokens when clean. Skipped for docker, modal and ssh backends (own probe).",
		"agent.bot_mode_protocol":                       "Bot Mode teammate-messaging protocol section (silent unless desktop Bot Mode manages it).",
		"agent.environment_hint":                        "Embedder-supplied text appended to the system prompt's environment-hints block, so a host wrapping icebath (sandbox runner, managed platform) can describe proxy, credential and mount layout without editing SOUL.md. Env ICEBATH_ENVIRONMENT_HINT overrides it.",
		"agent.coding_context":                          "Coding posture: on interactive coding surfaces (CLI, TUI, desktop, ACP) in a code workspace, add a coding brief + live git and workspace snapshot to the system prompt. \"auto\" = prompt-only when interactive AND cwd is a code workspace (toolsets untouched, messaging platforms unaffected); \"focus\" = auto + collapse the toolset to the lean coding set (+ enabled MCP servers) + demote non-coding skill categories to names-only (explicit opt-in); \"on\" = force everywhere; \"off\" = disable.",
		"agent.coding_instructions":                     "Standing operator instructions (string or list) appended to the coding brief as an extra stable system block, project-wide workflow rules such as \"Don't run tsc/lint until I approve.\" Cache-safe: takes effect next session.",
		"agent.verify_guidance":                         "When verify-on-stop finds edits without fresh verification evidence, add guidance for creative UI work (no broad tsc/lint/test before visual approval) and clean-diff expectations. false = keep the evidence nudge terse.",
		"agent.max_verify_nudges":                       "Max consecutive `pre_verify` \"continue\" nudges per turn (hooks can't trap the loop).",
		"agent.verify_on_stop":                          "Verification closure: after code edits in a workspace, refuse a final answer until fresh verification evidence exists or the agent explains why it can't check (bounded loop, passive ledger). false (default) because the nudges proved more noise than signal; true = force on everywhere; \"auto\" = on for interactive coding surfaces and programmatic callers, off for messaging surfaces. Doc, markdown and skill-only edits never fire.",
		"agent.gateway_timeout_warning":                 "Inactivity warning (seconds), once per run before gateway_timeout; no interrupt. 0 = off.",
		"agent.clarify_timeout":                         "Max seconds any surface (CLI, TUI/Desktop, messaging gateway) blocks an agent awaiting a clarify-tool reply; then it unblocks with \"[user did not respond within Xm]\". 0 or less = unlimited. A legacy top-level clarify.timeout still wins when explicitly set. 1h because users step away and a shorter value evicted the entry mid-think so a later button tap hit a dead entry. Tradeoff: a higher value holds the gateway's running-agent guard longer for a genuinely abandoned prompt; lower it to free the guard sooner.",
		"agent.gateway_notify_interval":                 "\"Still working\" status interval (seconds); 0 = off. Lower = faster feedback, more noise; 180 catches spinning weak-model runs before users /restart.",
		"agent.session_stall_timeout":                   "Session stall watchdog (seconds): RECOVERY notifier for an in-process agent with an adapter-queued follow-up while its activity clock is stale, NOT a general stall detector (ignores startup restore, build sentinels, leases, debounce, other processes; scan cadence per agent). Notify-only: tells the user to try /new. Distinct from gateway_timeout (kills the turn) and gateway_notify_interval. 0 = disable.",
		"agent.sanitizer_heal_escalation_threshold":     "Transcript-sanitiser heal escalation: after this many pre-send heal passes within a 10-minute window, log one ERROR and queue a ONE-TIME out-of-band notice pointing at /debug share or `icebath doctor` (status channel only; prompt cache untouched). 0 = no escalation (per-window WARNINGs still fire).",
		"agent.reconnect_attention_after":               "Seconds of continuous reconnect failure before a platform gets needs_attention flagged in gateway status (`icebath status`, fleet monitoring). Retries never stop; a signal, not a circuit breaker. 0 = disable.",
		"agent.gateway_auto_continue_freshness":         "Freshness window (seconds) for the auto-continue note. After a crash or restart mid-run the next user message gets \"[System note: your previous turn was interrupted...]\" prepended, only when the last persisted transcript row is younger than this, so stale markers don't revive an unrelated old task. Covers gateway_timeout (1800) plus slack. 0 = always inject.",
		"agent.gateway_startup_restore_drain_timeout":   "Max seconds the gateway waits for boot auto-resume turns before releasing the startup-restore inbound gate (all inbound is QUEUED while shut, so one long resumed turn would leave every channel unanswered). On timeout the gate opens and the resume keeps running in the background; duplicate-agent protection is unaffected because the resume slot is claimed synchronously first. 0 = wait forever.",
		"agent.gateway_startup_warmup_timeout":          "Max seconds the boot turn-machinery warm-up (agent code paths, tool schemas + availability probes, context-file tier) may hold the inbound gate shut, so an early message isn't served with a skeleton system prompt. On timeout the gate opens and warm-up finishes in the background. 0 = disable warm-up (lazy init).",
		"agent.local_stream_stale_timeout":              "Stale-stream ceiling (s) for local providers (Ollama, oMLX, llama-cpp) when the base stale timeout is at its 180s default, so a wedged local server eventually trips the detector instead of hanging forever. Env ICEBATH_LOCAL_STREAM_STALE_TIMEOUT overrides.",
		"agent.stream_max_lifetime":                     "Opt-in whole-stream cap in seconds, 0 = off.",
		"agent.image_input_mode":                        "How user-attached images reach the main model (gateway, TUI, CLI /attach). \"auto\" = native when the model reports supports_vision=true AND auxiliary.vision.provider is not explicitly set, else text; \"native\" = always attach (non-vision models error at the provider or get a last-chance text fallback); \"text\" = always pre-analyze with vision_analyze and prepend the description. vision_analyze stays a tool regardless.",
		"agent.disabled_toolsets":                       "Toolsets disabled for every surface.",
		"agent.reasoning_overrides":                     "Model name (any reasonable spelling) -> effort level; overrides agent.reasoning_effort when the current model matches. Edit in config.yaml (no CLI support: dots in keys).",
		"agent.reasoning_echo":                          "Stored reasoning is replayed to every provider that can read it by default. This flag only adds the must-echo \" \" pad on reasoning-less tool-call turns, which DeepSeek, Kimi/Moonshot and Xiaomi MiMo require (auto-detected by provider name and host). Set `reasoning_echo: true` on a `model:` or `fallback_providers:` entry for a custom gateway proxying one of them. Strict providers (Mistral, Groq, Cerebras) never get the field.",
		"agent.turn_liveness":                           "Turn liveness watchdog: a turn with no observable progress for `timeout_s` seconds is logged, force-interrupted so the UI can retry, and its lease stops renewing so stale-turn cleanup can reclaim the session even if the interrupt can't unwind a wedged frame. timeout_s <= 0 disables; poll_s = sampling interval. Invalid values (NaN, Inf, non-positive poll) warn and fall back to defaults.",
		"agent.reasoning_effort":                        "Reasoning effort for the main model: minimal, low, medium, high, xhigh, max, ultra, or none/false to disable; a mapping {enabled, effort} passes a bespoke level through.",
		"agent.system_prompt":                           "Your manual system prompt (a string or a list of lines), used when display.personality names no known personality. Personality selection never writes it.",
		"agent.personalities":                           "User personalities that add to or override the built-ins by name, same shapes as the root personalities block; an entry here wins on a name clash.",
	}
}
