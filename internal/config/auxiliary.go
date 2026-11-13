package config

import "gopkg.in/yaml.v3"

// Side-task model routing, one block per built-in task plus plugin-registered task blocks kept by name
type AuxiliaryConfig struct {
	TransientRetries   int                 `yaml:"transient_retries"`
	FreeOnly           bool                `yaml:"free_only" coerce:"bool"`
	OpenRouterModel    string              `yaml:"openrouter_model" coerce:"exact"`
	StreamOnlyBaseURLs []string            `yaml:"stream_only_base_urls" coerce:"exact"`
	Vision             AuxVision           `yaml:"vision"`
	Compression        AuxCompression      `yaml:"compression"`
	SkillsHub          AuxTask             `yaml:"skills_hub"`
	Approval           AuxTask             `yaml:"approval"`
	Review             AuxReview           `yaml:"review"`
	MCP                AuxTask             `yaml:"mcp"`
	TitleGeneration    AuxTitleGeneration  `yaml:"title_generation"`
	MemoryQueryRewrite AuxTask             `yaml:"memory_query_rewrite"`
	TTSAudioTags       AuxTask             `yaml:"tts_audio_tags"`
	VoiceChat          AuxTask             `yaml:"voice_chat"`
	TriageSpecifier    AuxTask             `yaml:"triage_specifier"`
	KanbanDecomposer   AuxTask             `yaml:"kanban_decomposer"`
	ProfileDescriber   AuxTask             `yaml:"profile_describer"`
	GoalJudge          AuxTask             `yaml:"goal_judge"`
	Curator            AuxTask             `yaml:"curator"`
	Monitor            AuxTask             `yaml:"monitor"`
	BackgroundReview   AuxBackgroundReview `yaml:"background_review"`
	MoAReference       AuxTask             `yaml:"moa_reference"`
	MoAAggregator      AuxTask             `yaml:"moa_aggregator"`
	rest               rest
}

type auxiliaryFields AuxiliaryConfig

// Decodes the mapping and keeps plugin task blocks and unknown keys
func (a *AuxiliaryConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*auxiliaryFields)(a), &a.rest)
}

// Writes the mapping with plugin task blocks after the built-in ones
func (a AuxiliaryConfig) MarshalYAML() (any, error) {
	return encodeMapping(auxiliaryFields(a), a.rest)
}

// Route block of a task by name, built-in or plugin-registered, ok false when the file holds none
func (a AuxiliaryConfig) Task(name string) (AuxTask, bool, error) {
	switch name {
	case "vision":
		return a.Vision.AuxTask, true, nil
	case "compression":
		return a.Compression.AuxTask, true, nil
	case "skills_hub":
		return a.SkillsHub, true, nil
	case "approval":
		return a.Approval, true, nil
	case "review":
		r := a.Review
		return AuxTask{Provider: r.Provider, Model: r.Model, BaseURL: r.BaseURL, APIKey: r.APIKey, APIMode: r.APIMode}, true, nil
	case "mcp":
		return a.MCP, true, nil
	case "title_generation":
		t := a.TitleGeneration
		return AuxTask{
			Provider: t.Provider, Model: t.Model, BaseURL: t.BaseURL, APIKey: t.APIKey, Timeout: t.Timeout, ExtraBody: t.ExtraBody,
			ReasoningEffort: t.ReasoningEffort, APIMode: t.APIMode, KeyEnv: t.KeyEnv, APIKeyEnv: t.APIKeyEnv,
			MaxConcurrency: t.MaxConcurrency, NoProgressTimeout: t.NoProgressTimeout, FallbackChain: t.FallbackChain,
		}, true, nil
	case "memory_query_rewrite":
		return a.MemoryQueryRewrite, true, nil
	case "tts_audio_tags":
		return a.TTSAudioTags, true, nil
	case "voice_chat":
		return a.VoiceChat, true, nil
	case "triage_specifier":
		return a.TriageSpecifier, true, nil
	case "kanban_decomposer":
		return a.KanbanDecomposer, true, nil
	case "profile_describer":
		return a.ProfileDescriber, true, nil
	case "goal_judge":
		return a.GoalJudge, true, nil
	case "curator":
		return a.Curator, true, nil
	case "monitor":
		return a.Monitor, true, nil
	case "background_review":
		return a.BackgroundReview.AuxTask, true, nil
	case "moa_reference":
		return a.MoAReference, true, nil
	case "moa_aggregator":
		return a.MoAAggregator, true, nil
	}
	n := a.rest.get(name)
	if n == nil || n.Kind != yaml.MappingNode {
		return AuxTask{}, false, nil
	}
	var t AuxTask
	if err := n.Decode(&t); err != nil {
		return t, true, err
	}
	return t, true, nil
}

// Shared route block of an auxiliary task
type AuxTask struct {
	Provider          string           `yaml:"provider"`
	Model             string           `yaml:"model"`
	BaseURL           string           `yaml:"base_url"`
	APIKey            string           `yaml:"api_key"`
	Timeout           float64          `yaml:"timeout"`
	ExtraBody         Map[Raw]         `yaml:"extra_body"`
	ReasoningEffort   *ReasoningEffort `yaml:"reasoning_effort,omitempty"`
	APIMode           string           `yaml:"api_mode,omitempty"`
	KeyEnv            string           `yaml:"key_env,omitempty"`
	APIKeyEnv         string           `yaml:"api_key_env,omitempty"`
	MaxConcurrency    *int             `yaml:"max_concurrency,omitempty"`
	NoProgressTimeout *float64         `yaml:"no_progress_timeout,omitempty" coerce:"strict"`
	FallbackChain     []FallbackEntry  `yaml:"fallback_chain,omitempty"`
	ContextLength     *int             `yaml:"context_length,omitempty"`
	rest              rest
}

type auxTaskFields AuxTask

// Decodes the mapping and keeps unknown keys
func (t *AuxTask) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*auxTaskFields)(t), &t.rest)
}

// Writes the mapping with unknown keys after the named ones
func (t AuxTask) MarshalYAML() (any, error) {
	return encodeMapping(auxTaskFields(t), t.rest)
}

// Vision task block with the image download timeout
type AuxVision struct {
	AuxTask         `yaml:",inline"`
	DownloadTimeout float64 `yaml:"download_timeout"`
}

type auxVisionFields struct {
	auxTaskFields   `yaml:",inline"`
	DownloadTimeout float64 `yaml:"download_timeout"`
}

// Decodes the mapping and keeps unknown keys
func (v *AuxVision) UnmarshalYAML(n *yaml.Node) error {
	f := auxVisionFields{auxTaskFields(v.AuxTask), v.DownloadTimeout}
	err := decodeMapping(n, &f, &f.rest)
	v.AuxTask, v.DownloadTimeout = AuxTask(f.auxTaskFields), f.DownloadTimeout
	return err
}

// Writes the mapping with unknown keys after the named ones
func (v AuxVision) MarshalYAML() (any, error) {
	return encodeMapping(auxVisionFields{auxTaskFields(v.AuxTask), v.DownloadTimeout}, v.rest)
}

// Compression task block with the stream no-progress timeout
type AuxCompression struct {
	AuxTask `yaml:",inline"`
}

// Decodes the mapping and keeps unknown keys
func (c *AuxCompression) UnmarshalYAML(n *yaml.Node) error {
	return c.AuxTask.UnmarshalYAML(n)
}

// Writes the block, always naming no_progress_timeout right after the standard keys as the defaults do
func (c AuxCompression) MarshalYAML() (any, error) {
	out, err := c.AuxTask.MarshalYAML()
	if err != nil || c.NoProgressTimeout != nil {
		return out, err
	}
	n := out.(*yaml.Node)
	at := len(n.Content)
	for i := 0; i+1 < len(n.Content); i += 2 {
		switch n.Content[i].Value {
		case "extra_body", "reasoning_effort":
			at = i + 2
		}
	}
	tail := append([]*yaml.Node{strNode("no_progress_timeout"), nullNode()}, n.Content[at:]...)
	n.Content = append(n.Content[:at:at], tail...)
	return n, nil
}

// Review subagent route block
type AuxReview struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
	BaseURL  string `yaml:"base_url"`
	APIKey   string `yaml:"api_key"`
	APIMode  string `yaml:"api_mode"`
	rest     rest
}

type auxReviewFields AuxReview

// Decodes the mapping and keeps unknown keys
func (r *AuxReview) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*auxReviewFields)(r), &r.rest)
}

// Writes the mapping with unknown keys after the named ones
func (r AuxReview) MarshalYAML() (any, error) {
	return encodeMapping(auxReviewFields(r), r.rest)
}

// Session title generation block
type AuxTitleGeneration struct {
	Enabled             bool             `yaml:"enabled"`
	ModelUpgradeEnabled bool             `yaml:"model_upgrade_enabled"`
	Provider            string           `yaml:"provider"`
	Model               string           `yaml:"model"`
	PreferFastModel     bool             `yaml:"prefer_fast_model"`
	BaseURL             string           `yaml:"base_url"`
	APIKey              string           `yaml:"api_key"`
	Timeout             float64          `yaml:"timeout"`
	ExtraBody           Map[Raw]         `yaml:"extra_body"`
	ReasoningEffort     *ReasoningEffort `yaml:"reasoning_effort"`
	Language            string           `yaml:"language"`
	APIMode             string           `yaml:"api_mode,omitempty"`
	KeyEnv              string           `yaml:"key_env,omitempty"`
	APIKeyEnv           string           `yaml:"api_key_env,omitempty"`
	MaxConcurrency      *int             `yaml:"max_concurrency,omitempty"`
	NoProgressTimeout   *float64         `yaml:"no_progress_timeout,omitempty" coerce:"strict"`
	FallbackChain       []FallbackEntry  `yaml:"fallback_chain,omitempty"`
	rest                rest
}

type auxTitleFields AuxTitleGeneration

// Decodes the mapping and keeps unknown keys
func (t *AuxTitleGeneration) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*auxTitleFields)(t), &t.rest)
}

// Writes the mapping with unknown keys after the named ones
func (t AuxTitleGeneration) MarshalYAML() (any, error) {
	return encodeMapping(auxTitleFields(t), t.rest)
}

// Post-turn self-improvement review block
type AuxBackgroundReview struct {
	Enabled        bool `yaml:"enabled"`
	AuxTask        `yaml:",inline"`
	MaxInputTokens *int `yaml:"max_input_tokens,omitempty"`
}

type auxBackgroundReviewFields struct {
	Enabled        bool `yaml:"enabled"`
	auxTaskFields  `yaml:",inline"`
	MaxInputTokens *int `yaml:"max_input_tokens,omitempty"`
}

// Decodes the mapping and keeps unknown keys
func (b *AuxBackgroundReview) UnmarshalYAML(n *yaml.Node) error {
	f := auxBackgroundReviewFields{b.Enabled, auxTaskFields(b.AuxTask), b.MaxInputTokens}
	err := decodeMapping(n, &f, &f.rest)
	b.Enabled, b.AuxTask, b.MaxInputTokens = f.Enabled, AuxTask(f.auxTaskFields), f.MaxInputTokens
	return err
}

// Writes the mapping with unknown keys after the named ones
func (b AuxBackgroundReview) MarshalYAML() (any, error) {
	return encodeMapping(auxBackgroundReviewFields{b.Enabled, auxTaskFields(b.AuxTask), b.MaxInputTokens}, b.rest)
}

// Standard auxiliary block with the reference timeout, carrying an empty reasoning effort unless noEffort
func auxTask(timeout float64, noEffort bool) AuxTask {
	t := AuxTask{Provider: "auto", Timeout: timeout}
	if !noEffort {
		t.ReasoningEffort = new(EffortLevel(""))
	}
	return t
}

// Defaults of the auxiliary section
func defaultAuxiliary() AuxiliaryConfig {
	voice := auxTask(120, false)
	voice.ReasoningEffort = new(EffortLevel("none"))
	return AuxiliaryConfig{
		TransientRetries:   2,
		StreamOnlyBaseURLs: []string{},
		Vision:             AuxVision{AuxTask: auxTask(120, false), DownloadTimeout: 30},
		Compression:        AuxCompression{AuxTask: auxTask(120, false)},
		SkillsHub:          auxTask(30, false),
		Approval:           auxTask(30, false),
		Review:             AuxReview{Provider: "auto"},
		MCP:                auxTask(30, false),
		TitleGeneration: AuxTitleGeneration{
			Enabled: true, ModelUpgradeEnabled: true, Provider: "auto", Timeout: 30, ReasoningEffort: new(EffortLevel("")),
		},
		MemoryQueryRewrite: auxTask(8, true),
		TTSAudioTags:       auxTask(30, false),
		VoiceChat:          voice,
		TriageSpecifier:    auxTask(120, false),
		KanbanDecomposer:   auxTask(180, false),
		ProfileDescriber:   auxTask(60, false),
		GoalJudge:          auxTask(60, false),
		Curator:            auxTask(600, false),
		Monitor:            auxTask(60, false),
		BackgroundReview:   AuxBackgroundReview{Enabled: true, AuxTask: auxTask(120, false)},
		MoAReference:       auxTask(900, true),
		MoAAggregator:      auxTask(900, true),
	}
}

// Documentation of the auxiliary section
func auxiliaryDocs() map[string]string {
	return map[string]string{
		"auxiliary":                                        "Auxiliary model config, provider and model per side task. provider \"auto\" = auto-detect; empty model = the provider's default aux model; all tasks fall back to openrouter:google/gemini-3-flash-preview when the configured provider is unavailable. extra_body is forwarded verbatim as request body fields for that task, e.g. OpenRouter routing prefs or the Pareto Code floor (extra_body: {provider: {order: [anthropic, google], sort: throughput}, plugins: [{id: pareto-router, min_coding_score: 0.5}]}). Each task is independent: main-agent provider_routing and openrouter.min_coding_score do NOT propagate to aux calls by design. web_extract and session_search no longer use an aux model; leftover auxiliary.web_extract and auxiliary.session_search blocks are ignored.",
		"auxiliary.transient_retries":                      "Same-provider retries for a transient blip (reset, timeout, 5xx, 408) on ANY aux call before falling back; clamped [0,6]. Matters for pinned calls (MoA advisors) where provider fallback is not meaningful recovery.",
		"auxiliary.free_only":                              "When true, the auto-chain's OpenRouter step is skipped unless the fallback model ends in \":free\", so a PAID lane is never used for background aux traffic even with OPENROUTER_API_KEY set.",
		"auxiliary.openrouter_model":                       "Override the auto-chain's OpenRouter fallback model (default google/gemini-3.6-flash, PAID). Pair e.g. \"nvidia/nemotron-3-ultra-550b-a55b:free\" with free_only: true. A one-time WARNING is logged whenever a non-\":free\" model is engaged.",
		"auxiliary.stream_only_base_urls":                  "Endpoints that reject NON-streaming chat (HTTP 400): aux calls are sent with stream=true and aggregated. Case-insensitive URL substrings; some vendor endpoints are always stream-only.",
		"auxiliary.vision":                                 "Per-task blocks share one shape: provider \"auto\" = inherit the main model; base_url overrides provider; api_key falls back to OPENAI_API_KEY; reasoning_effort: none|minimal|low|medium|high|xhigh|max|ultra (\"\" = provider default); extra_body = OpenAI-compatible request fields; fallback_chain = ordered routes (each needs provider; model, base_url and api_key optional) tried when the task provider fails.",
		"auxiliary.vision.download_timeout":                "Image HTTP download timeout (seconds).",
		"auxiliary.compression":                            "Context compression summarizer. Raise timeout for local models. context_length hints the summarizer window where a custom endpoint cannot report it.",
		"auxiliary.compression.no_progress_timeout":        "Seconds a streamed call goes without a substantive chunk before it fails fast into retry or fallback; null = built-in 60s default. Independent of \"timeout\" (the overall request budget); raising \"timeout\" alone does not widen this window.",
		"auxiliary.skills_hub":                             "Skills hub assistant.",
		"auxiliary.approval":                               "Smart-approval classifier; a fast, cheap model is recommended.",
		"auxiliary.review":                                 "/review reviewer: a full subagent on the async delegation rail, credentials resolved like delegation.provider pins. \"auto\" + \"\" = the main agent's model. api_mode forces transport: chat_completions | anthropic_messages | codex_responses.",
		"auxiliary.mcp":                                    "MCP sampling and elicitation helper.",
		"auxiliary.title_generation":                       "Session titles. prefer_fast_model opts in to the provider fast tier; auto otherwise = main model.",
		"auxiliary.title_generation.model_upgrade_enabled": "false = keep the instant derived title, never call a model.",
		"auxiliary.memory_query_rewrite":                   "Memory recall query rewriter.",
		"auxiliary.tts_audio_tags":                         "Inserts audio tags into TTS scripts.",
		"auxiliary.voice_chat":                             "Voice chat turn routing; reasoning_effort none is the lowest valid level.",
		"auxiliary.triage_specifier":                       "Kanban: expands a Triage one-liner into a spec (a cheap model is fine).",
		"auxiliary.kanban_decomposer":                      "Kanban: emits a JSON graph of child tasks (more tokens).",
		"auxiliary.profile_describer":                      "One or two sentence profile blurb; short, cheap.",
		"auxiliary.goal_judge":                             "/goal satisfaction and contract drafting; JSON calls.",
		"auxiliary.curator":                                "Curator skill-usage review can take minutes on reasoning models (umbrellas over hundreds of skills); route cheaper via `icebath model` -> auxiliary -> Curator.",
		"auxiliary.monitor":                                "Important-mail 0-10 scorer; high-volume, a small model is fine.",
		"auxiliary.background_review":                      "Post-turn self-improvement fork (save memory, patch skill). \"auto\" = main model replaying the full conversation (warm cache); other models replay a compact digest (~3-5x cheaper). enabled=false skips auto spawns (/refine still works). An explicit max_input_tokens caps the SUM of replayed input tokens over the review loop (iterations capped at 16); the loop stops before crossing it. When unset, the runtime derives a budget from the active model context window. <= 0 = unlimited. reasoning_effort is IGNORED while the review stays on the main model: the fork inherits the conversation's reasoning config verbatim so its request bytes keep the parent's warm prompt-cache prefix. Set provider and model to route the review to another model for a different effort level; a one-time warning says so when the key is set.",
		"auxiliary.moa_reference":                          "Mixture-of-agents reference slot. No reasoning_effort by design: depth is configured PER SLOT in the preset (moa.presets.<name>.reference_models[].reasoning_effort).",
		"auxiliary.moa_aggregator":                         "Mixture-of-agents aggregator slot. No reasoning_effort by design: depth is configured in the preset (moa.presets.<name>.aggregator.reasoning_effort).",
	}
}
