package tools

import (
	"context"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// PluginToolsetCatalog is the plugin toolset source pkg/plugins installs at init
var PluginToolsetCatalog struct {
	Discover       func(ctx context.Context)
	Toolsets       func(ctx context.Context) []ConfigurableToolset
	KeysNowait     func(ctx context.Context) map[string]bool
	PortableNowait func(ctx context.Context) map[string]bool
}

// GatewayPluginToolsets is the projected plugin toolsets and portable MCP servers a gateway backed CLI installs
var GatewayPluginToolsets func() ([]ConfigurableToolset, map[string]bool, bool)

func projectedPluginToolsets() ([]ConfigurableToolset, map[string]bool, bool) {
	if GatewayPluginToolsets == nil {
		return nil, nil, false
	}
	return GatewayPluginToolsets()
}

// Portable plugin MCP server names from the gateway projection, else the plugin catalog without discovery
func portableMCPServerNames(ctx context.Context) (names map[string]bool) {
	defer func() {
		if rec := recover(); rec != nil {
			toolsConfigLogger().Debug("Failed to include portable MCP servers", "error", rec)
			names = map[string]bool{}
		}
	}()
	if _, portable, ok := projectedPluginToolsets(); ok {
		return portable
	}
	if PluginToolsetCatalog.PortableNowait == nil {
		return map[string]bool{}
	}
	return PluginToolsetCatalog.PortableNowait(ctx)
}

// PortableMCPServerSet lists the in memory portable plugin MCP servers, the projected set first
func PortableMCPServerSet(ctx context.Context) map[string]bool { return portableMCPServerNames(ctx) }

// Toolset title without its leading emoji word for GUI surfaces, the raw stripped label otherwise
func GUIToolsetLabel(label string) string {
	text := pystr.Strip(label)
	i := strings.IndexFunc(text, pystr.IsSpace)
	if i < 0 {
		return text
	}
	for _, ch := range text[:i] {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') {
			return text
		}
	}
	return pystr.Strip(strings.TrimLeftFunc(text[i:], pystr.IsSpace))
}

// Platform a platform less configuration UI targets, a restricted toolset moving to its first allowed platform
func ToolsetConfigurationPlatform(tsKey, def string) string {
	allowed := ToolsetPlatformRestrictions[tsKey]
	if len(allowed) == 0 || allowed[def] {
		return def
	}
	names := make([]string, 0, len(allowed))
	for k := range allowed {
		names = append(names, k)
	}
	return slices.Min(names)
}

// Built in configurable toolsets then plugin toolsets whose keys are not built in
func EffectiveConfigurableToolsets(ctx context.Context) (result []ConfigurableToolset) {
	result = slices.Clone(ConfigurableToolsets)
	seen := map[string]bool{}
	for _, ts := range result {
		seen[ts.Key] = true
	}
	defer func() {
		if recover() != nil {
			result = result[:len(ConfigurableToolsets)]
		}
	}()
	projected, _, ok := projectedPluginToolsets()
	if !ok {
		PluginToolsetCatalog.Discover(ctx)
		projected = PluginToolsetCatalog.Toolsets(ctx)
	}
	for _, entry := range projected {
		if !seen[entry.Key] {
			seen[entry.Key] = true
			result = append(result, entry)
		}
	}
	return result
}

// Toolset keys plugins provide, empty when the plugin system fails
func PluginToolsetKeys(ctx context.Context) (keys map[string]bool) {
	defer func() {
		if recover() != nil {
			keys = map[string]bool{}
		}
	}()
	if projected, _, ok := projectedPluginToolsets(); ok {
		keys = map[string]bool{}
		for _, ts := range projected {
			keys[ts.Key] = true
		}
		return keys
	}
	return PluginToolsetCatalog.KeysNowait(ctx)
}

// Toolset keys the tools checklist offers for a platform
func checklistToolsetKeys(ctx context.Context, platform string) map[string]bool {
	out := map[string]bool{}
	for _, ts := range EffectiveConfigurableToolsets(ctx) {
		if ToolsetAllowedForPlatform(ts.Key, platform) && !ConfigOnlyToolsets[ts.Key] {
			out[ts.Key] = true
		}
	}
	return out
}

// Display label of a built in or plugin toolset, the key itself when unknown
func toolsetLabel(ctx context.Context, tsKey string) string {
	for _, ts := range EffectiveConfigurableToolsets(ctx) {
		if ts.Key == tsKey {
			return ts.Label
		}
	}
	return tsKey
}

// True when the toolset has provider rows or env var requirements to prompt for
func isConfigurableToolset(tsKey string) bool {
	if v, ok := ToolCategories.Get(tsKey); ok && pyTruthy(v) {
		return true
	}
	return len(ToolsetEnvRequirements[tsKey]) > 0
}

// One env_vars entry of a provider row, extra pairs before the optional url
func toolKey(key, prompt, url string, extra ...any) *pyjson.Object {
	o := pyjson.NewObject()
	o.Set("key", key)
	o.Set("prompt", prompt)
	for i := 0; i+1 < len(extra); i += 2 {
		o.Set(extra[i].(string), extra[i+1])
	}
	if url != "" {
		o.Set("url", url)
	}
	return o
}

// One provider row, markers as ordered key value pairs after env_vars
func toolRow(name, badge, tag string, envVars []any, markers ...any) *pyjson.Object {
	o := pyjson.NewObject()
	o.Set("name", name)
	if badge != "" {
		o.Set("badge", badge)
	}
	if tag != "" {
		o.Set("tag", tag)
	}
	o.Set("env_vars", append([]any{}, envVars...))
	for i := 0; i+1 < len(markers); i += 2 {
		o.Set(markers[i].(string), markers[i+1])
	}
	return o
}

func toolCategory(pairs ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i+1 < len(pairs); i += 2 {
		o.Set(pairs[i].(string), pairs[i+1])
	}
	return o
}

// ToolsetEnvRequirements lists env var and key URL pairs a toolset prompts for
var ToolsetEnvRequirements = map[string][][2]string{"vision": {{"OPENROUTER_API_KEY", "https://openrouter.ai/keys"}}}

// ToolCategories is the ordered provider catalog of every configurable tool category
var ToolCategories = buildToolCategories()

func buildToolCategories() *pyjson.Object {
	nous := []any{"requires_nous_auth", true}
	with := func(parts ...[]any) []any {
		var out []any
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	openAIVoice := toolKey("VOICE_TOOLS_OPENAI_KEY", "OpenAI API key", "https://platform.openai.com/api-keys")
	eleven := toolKey("ELEVENLABS_API_KEY", "ElevenLabs API key", "https://elevenlabs.io/app/settings/api-keys")
	deepinfra := toolKey("DEEPINFRA_API_KEY", "DeepInfra API key", "https://deepinfra.com/dash/api_keys")
	mistral := func() *pyjson.Object {
		return toolKey("MISTRAL_API_KEY", "Mistral API key", "https://console.mistral.ai/")
	}
	voiceOverride := []any{"VOICE_TOOLS_OPENAI_KEY", "OPENAI_API_KEY"}
	return toolCategory(
		"tts", toolCategory("name", "Text-to-Speech", "icon", "🔊", "providers", []any{
			toolRow("Microsoft Edge TTS", "★ recommended · free", "Good quality, no API key needed", nil, "tts_provider", "edge"),
			toolRow("Nous Subscription", "subscription", "Managed OpenAI TTS billed to your subscription", nil,
				with([]any{"tts_provider", "openai"}, nous, []any{"managed_nous_feature", "tts", "override_env_vars", voiceOverride})...),
			toolRow("OpenAI TTS", "paid", "High quality voices", []any{openAIVoice}, "tts_provider", "openai"),
			toolRow("xAI TTS", "", "Grok voices — uses xAI Grok OAuth or XAI_API_KEY", nil, "tts_provider", "xai", "post_setup", "xai_grok"),
			toolRow("ElevenLabs", "paid", "Most natural voices", []any{eleven}, "tts_provider", "elevenlabs"),
			toolRow("Mistral (Voxtral TTS)", "paid", "Multilingual, native Opus", []any{mistral()}, "tts_provider", "mistral"),
			toolRow("Google Gemini TTS", "preview", "30 prebuilt voices, controllable via prompts",
				[]any{toolKey("GEMINI_API_KEY", "Gemini API key", "https://aistudio.google.com/app/apikey")}, "tts_provider", "gemini"),
			toolRow("KittenTTS", "local · free", "Lightweight local ONNX TTS (~25MB), no API key", nil, "tts_provider", "kittentts", "post_setup", "kittentts"),
			toolRow("Piper", "local · free", "Local neural TTS, 44 languages (voices ~20-90MB, downloaded on first use)", nil, "tts_provider", "piper"),
			toolRow("DeepInfra TTS", "paid", "Chatterbox, Qwen3-TTS, … — live catalog from api.deepinfra.com", []any{deepinfra}, "tts_provider", "deepinfra"),
		}),
		"stt", toolCategory("name", "Speech-to-Text", "icon", "🎙️", "providers", []any{
			toolRow("Local Whisper", "★ recommended · free", "faster-whisper on-device, no API key", nil, "stt_provider", "local", "post_setup", "faster_whisper"),
			toolRow("Nous Subscription", "subscription", "Managed OpenAI transcription billed to your subscription", nil,
				with([]any{"stt_provider", "openai"}, nous, []any{"managed_nous_feature", "stt", "override_env_vars", voiceOverride})...),
			toolRow("OpenAI", "paid", "whisper-1, gpt-4o-transcribe, gpt-transcribe", []any{openAIVoice}, "stt_provider", "openai"),
			toolRow("Groq", "free tier", "whisper-large-v3-turbo, whisper-large-v3 — very fast",
				[]any{toolKey("GROQ_API_KEY", "Groq API key", "https://console.groq.com/keys")}, "stt_provider", "groq"),
			toolRow("xAI", "", "Grok Voice Transcribe — uses xAI Grok OAuth or XAI_API_KEY", nil, "stt_provider", "xai", "post_setup", "xai_grok"),
			toolRow("ElevenLabs Scribe", "paid", "scribe_v2 — diarization + audio-event tagging", []any{eleven}, "stt_provider", "elevenlabs"),
			toolRow("Mistral Voxtral", "paid", "voxtral-mini-latest — multilingual", []any{mistral()}, "stt_provider", "mistral"),
			toolRow("DeepInfra", "paid", "Live STT catalog from api.deepinfra.com", []any{deepinfra}, "stt_provider", "deepinfra"),
		}),
		"web", toolCategory("name", "Web Search & Extract", "setup_title", "Select Search Provider",
			"setup_note", "A free DuckDuckGo search skill is also included — skip this if you don't need a premium provider.",
			"icon", "🔍", "providers", []any{
				toolCategory(with([]any{"name", "Nous Subscription", "badge", "subscription", "tag", "Managed web search and extract billed to your subscription",
					"web_backend", "firecrawl", "env_vars", []any{}}, nous, []any{"managed_nous_feature", "web",
					"override_env_vars", []any{"FIRECRAWL_API_KEY", "FIRECRAWL_API_URL", "PERPLEXITY_API_KEY"}})...),
				toolCategory("name", "Firecrawl Self-Hosted", "badge", "free · self-hosted", "tag", "Run your own Firecrawl instance (Docker)",
					"web_backend", "firecrawl", "env_vars", []any{toolKey("FIRECRAWL_API_URL", "Your Firecrawl instance URL (e.g., http://localhost:3002)", "")}),
			}),
		"image_gen", toolCategory("name", "Image Generation", "icon", "🎨", "providers", []any{
			toolRow("Nous Subscription", "subscription", "Managed image generation (FAL, Krea 3 and Krea 2, Nous Portal models) billed to your subscription", nil,
				with(nous, []any{"managed_nous_feature", "image_gen", "override_env_vars", []any{"FAL_KEY"}, "imagegen_backend", "nous"})...),
		}),
		"video_gen", toolCategory("name", "Video Generation", "icon", "🎬", "providers", []any{
			toolRow("Nous Subscription", "subscription", "Managed FAL video generation billed to your subscription", nil,
				with(nous, []any{"managed_nous_feature", "video_gen", "override_env_vars", []any{"FAL_KEY"}, "video_gen_plugin_name", "fal"})...),
		}),
		"x_search", toolCategory("name", "X (Twitter) Search", "setup_title", "Select xAI Credential Source",
			"setup_note", "Icebath routes X searches through xAI's built-in x_search Responses tool for read-only public X "+
				"discovery. Use the xurl skill for authenticated X API reads and account actions. Both credential "+
				"sources hit the same https://api.x.ai/v1/responses endpoint — pick whichever you already have. "+
				"SuperGrok OAuth is preferred when both are set (uses your subscription quota instead of API spend).",
			"icon", "🐦", "providers", []any{
				toolRow("xAI Grok OAuth (SuperGrok / Premium+)", "subscription", "Browser login at accounts.x.ai — no API key required", nil, "post_setup", "xai_grok"),
				toolRow("xAI API key", "paid", "Direct xAI API billing via XAI_API_KEY", []any{toolKey("XAI_API_KEY", "xAI API key", "https://console.x.ai/")}),
			}),
		"browser", toolCategory("name", "Browser Automation", "icon", "🌐", "providers", []any{
			toolRow("Local Browser", "★ recommended · free", "Headless Chromium, no API key needed", nil, "browser_provider", "local", "browser_engine", "auto", "post_setup", "chromium"),
			toolRow("Lightpanda", "free · local · no Chromium", "Zig headless browser spawned by Icebath, text-only (no screenshots)", nil,
				"browser_provider", "local", "browser_engine", "lightpanda", "post_setup", "lightpanda"),
			toolRow("Nous Subscription (Browser Use cloud)", "subscription", "Managed Browser Use billed to your subscription", nil,
				with([]any{"browser_provider", "browser-use"}, nous, []any{"managed_nous_feature", "browser", "override_env_vars", []any{"BROWSER_USE_API_KEY"}, "post_setup", "browserbase"})...),
		}),
		"computer_use", toolCategory("name", "Computer Use (macOS/Windows/Linux)", "icon", "🖱️", "platform_gate", []any{"darwin", "win32", "linux"}, "providers", []any{
			toolRow("cua-driver (background)", "★ recommended · free · local",
				"Background computer-use via cua-driver — does NOT steal your cursor or focus. Works with any model.", nil, "computer_use_backend", "cua", "post_setup", "cua_driver"),
		}),
	)
}

// Every env var key a tool category provider row asks for, in catalog order
func ToolProviderEnvKeys() []string {
	var keys []string
	for _, cat := range ToolCategories.Keys {
		v, _ := ToolCategories.Get(cat)
		category, _ := v.(*pyjson.Object)
		if category == nil {
			continue
		}
		providers, _ := category.Get("providers")
		rows, _ := providers.([]any)
		for _, r := range rows {
			row, _ := r.(*pyjson.Object)
			if row == nil {
				continue
			}
			envVars, _ := row.Get("env_vars")
			entries, _ := envVars.([]any)
			for _, e := range entries {
				if entry, ok := e.(*pyjson.Object); ok {
					if k, ok := entry.Get("key"); ok {
						if s, ok := k.(string); ok {
							keys = append(keys, s)
						}
					}
				}
			}
		}
	}
	return keys
}
