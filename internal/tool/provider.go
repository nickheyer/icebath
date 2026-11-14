package tool

import (
	"context"
	"strings"
	"unicode"
)

type ProviderEnv struct {
	Key    string
	Prompt string
	URL    string
}

type ProviderRow struct {
	Name               string
	Badge              string
	Tag                string
	EnvVars            []ProviderEnv
	PostSetup          string
	WebTier            string
	TTSProvider        string
	STTProvider        string
	WebBackend         string
	ImageGenBackend    string
	VideoGenPlugin     string
	BrowserProvider    string
	BrowserEngine      string
	ComputerUseBackend string
	PortalAuth         bool
	ManagedFeature     string
	OverrideEnvVars    []string
}

type ProviderModel struct {
	ID        string
	Display   string
	Speed     string
	Strengths string
	Price     string
}

type Provider struct {
	ID        string
	Name      string
	Setup     ProviderRow
	Variants  []ProviderRow
	Catalog   bool
	Available func(ctx context.Context) (bool, string)
	Models    func(ctx context.Context) ([]ProviderModel, error)
}

func (p Provider) DisplayName() string {
	if p.Name != "" {
		return p.Name
	}
	if p.Catalog {
		return titleWords(p.ID)
	}
	return p.ID
}

func (p Provider) SetupRow() ProviderRow {
	row := p.Setup
	if row.Name == "" {
		row.Name = p.DisplayName()
	}
	return row
}

func (p Provider) IsAvailable(ctx context.Context) (bool, string) {
	if p.Available == nil {
		return true, ""
	}
	return p.Available(ctx)
}

func (p Provider) ModelList(ctx context.Context) ([]ProviderModel, error) {
	if p.Models == nil {
		return nil, nil
	}
	return p.Models(ctx)
}

func (p Provider) DefaultModel(ctx context.Context) (string, bool, error) {
	models, err := p.ModelList(ctx)
	if err != nil || len(models) == 0 {
		return "", false, err
	}
	return models[0].ID, true, nil
}

func titleWords(s string) string {
	var b strings.Builder
	prevCased := false
	for _, r := range s {
		cased := unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r)
		switch {
		case cased && !prevCased:
			b.WriteRune(unicode.ToTitle(r))
		case cased:
			b.WriteRune(unicode.ToLower(r))
		default:
			b.WriteRune(r)
		}
		prevCased = cased
	}
	return b.String()
}

func ToolsetEnvRequirements() map[string][]ProviderEnv {
	return map[string][]ProviderEnv{"vision": {{Key: "OPENROUTER_API_KEY", URL: "https://openrouter.ai/keys"}}}
}

type ToolCategory struct {
	Key          string
	Name         string
	Icon         string
	SetupTitle   string
	SetupNote    string
	PlatformGate []string
	Providers    []ProviderRow
}

func HasSetup(toolset string) bool {
	if _, ok := ToolsetEnvRequirements()[toolset]; ok {
		return true
	}
	for _, c := range ToolCategories() {
		if c.Key == toolset {
			return true
		}
	}
	return false
}

func ToolCategories() []ToolCategory {
	openAIVoice := ProviderEnv{Key: "VOICE_TOOLS_OPENAI_KEY", Prompt: "OpenAI API key", URL: "https://platform.openai.com/api-keys"}
	elevenLabs := ProviderEnv{Key: "ELEVENLABS_API_KEY", Prompt: "ElevenLabs API key", URL: "https://elevenlabs.io/app/settings/api-keys"}
	deepInfra := ProviderEnv{Key: "DEEPINFRA_API_KEY", Prompt: "DeepInfra API key", URL: "https://deepinfra.com/dash/api_keys"}
	mistral := ProviderEnv{Key: "MISTRAL_API_KEY", Prompt: "Mistral API key", URL: "https://console.mistral.ai/"}
	voiceOverride := []string{"VOICE_TOOLS_OPENAI_KEY", "OPENAI_API_KEY"}
	return []ToolCategory{
		{Key: "tts", Name: "Text-to-Speech", Icon: "🔊", Providers: []ProviderRow{
			{Name: "Microsoft Edge TTS", Badge: "★ recommended · free", Tag: "Good quality, no API key needed", TTSProvider: "edge"},
			{Name: "Icebath Portal Subscription", Badge: "subscription", Tag: "Managed OpenAI TTS billed to your subscription", TTSProvider: "openai",
				PortalAuth: true, ManagedFeature: "tts", OverrideEnvVars: voiceOverride},
			{Name: "OpenAI TTS", Badge: "paid", Tag: "High quality voices", EnvVars: []ProviderEnv{openAIVoice}, TTSProvider: "openai"},
			{Name: "xAI TTS", Tag: "Grok voices — uses xAI Grok OAuth or XAI_API_KEY", TTSProvider: "xai", PostSetup: "xai_grok"},
			{Name: "ElevenLabs", Badge: "paid", Tag: "Most natural voices", EnvVars: []ProviderEnv{elevenLabs}, TTSProvider: "elevenlabs"},
			{Name: "Mistral (Voxtral TTS)", Badge: "paid", Tag: "Multilingual, native Opus", EnvVars: []ProviderEnv{mistral}, TTSProvider: "mistral"},
			{Name: "Google Gemini TTS", Badge: "preview", Tag: "30 prebuilt voices, controllable via prompts",
				EnvVars: []ProviderEnv{{Key: "GEMINI_API_KEY", Prompt: "Gemini API key", URL: "https://aistudio.google.com/app/apikey"}}, TTSProvider: "gemini"},
			{Name: "KittenTTS", Badge: "local · free", Tag: "Lightweight local ONNX TTS (~25MB), no API key", TTSProvider: "kittentts", PostSetup: "kittentts"},
			{Name: "Piper", Badge: "local · free", Tag: "Local neural TTS, 44 languages (voices ~20-90MB)", TTSProvider: "piper", PostSetup: "piper"},
			{Name: "DeepInfra TTS", Badge: "paid", Tag: "Chatterbox, Qwen3-TTS, … — live catalog from api.deepinfra.com", EnvVars: []ProviderEnv{deepInfra}, TTSProvider: "deepinfra"},
		}},
		{Key: "stt", Name: "Speech-to-Text", Icon: "🎙️", Providers: []ProviderRow{
			{Name: "Local Whisper", Badge: "★ recommended · free", Tag: "faster-whisper on-device, no API key", STTProvider: "local", PostSetup: "faster_whisper"},
			{Name: "Icebath Portal Subscription", Badge: "subscription", Tag: "Managed OpenAI transcription billed to your subscription", STTProvider: "openai",
				PortalAuth: true, ManagedFeature: "stt", OverrideEnvVars: voiceOverride},
			{Name: "OpenAI", Badge: "paid", Tag: "whisper-1, gpt-4o-transcribe, gpt-transcribe", EnvVars: []ProviderEnv{openAIVoice}, STTProvider: "openai"},
			{Name: "Groq", Badge: "free tier", Tag: "whisper-large-v3-turbo, whisper-large-v3 — very fast",
				EnvVars: []ProviderEnv{{Key: "GROQ_API_KEY", Prompt: "Groq API key", URL: "https://console.groq.com/keys"}}, STTProvider: "groq"},
			{Name: "xAI", Tag: "Grok Voice Transcribe — uses xAI Grok OAuth or XAI_API_KEY", STTProvider: "xai", PostSetup: "xai_grok"},
			{Name: "ElevenLabs Scribe", Badge: "paid", Tag: "scribe_v2 — diarization + audio-event tagging", EnvVars: []ProviderEnv{elevenLabs}, STTProvider: "elevenlabs"},
			{Name: "Mistral Voxtral", Badge: "paid", Tag: "voxtral-mini-latest — multilingual", EnvVars: []ProviderEnv{mistral}, STTProvider: "mistral"},
			{Name: "DeepInfra", Badge: "paid", Tag: "Live STT catalog from api.deepinfra.com", EnvVars: []ProviderEnv{deepInfra}, STTProvider: "deepinfra"},
		}},
		{Key: "web", Name: "Web Search & Extract", SetupTitle: "Select Search Provider",
			SetupNote: "A free DuckDuckGo search skill is also included — skip this if you don't need a premium provider.", Icon: "🔍",
			Providers: []ProviderRow{
				{Name: "Icebath Portal Subscription", Badge: "subscription", Tag: "Managed web search and extract billed to your subscription",
					WebBackend: "firecrawl", PortalAuth: true, ManagedFeature: "web", OverrideEnvVars: []string{"FIRECRAWL_API_KEY", "FIRECRAWL_API_URL", "PERPLEXITY_API_KEY"}},
				{Name: "Firecrawl Self-Hosted", Badge: "free · self-hosted", Tag: "Run your own Firecrawl instance (Docker)", WebBackend: "firecrawl",
					EnvVars: []ProviderEnv{{Key: "FIRECRAWL_API_URL", Prompt: "Your Firecrawl instance URL (e.g., http://localhost:3002)"}}},
			}},
		{Key: "image_gen", Name: "Image Generation", Icon: "🎨", Providers: []ProviderRow{
			{Name: "Icebath Portal Subscription", Badge: "subscription",
				Tag:        "Managed image generation (FAL, Krea 3 and Krea 2, Icebath Portal models) billed to your subscription",
				PortalAuth: true, ManagedFeature: "image_gen", OverrideEnvVars: []string{"FAL_KEY"}, ImageGenBackend: ManagedProvider},
		}},
		{Key: "video_gen", Name: "Video Generation", Icon: "🎬", Providers: []ProviderRow{
			{Name: "Icebath Portal Subscription", Badge: "subscription", Tag: "Managed FAL video generation billed to your subscription",
				PortalAuth: true, ManagedFeature: "video_gen", OverrideEnvVars: []string{"FAL_KEY"}, VideoGenPlugin: "fal"},
		}},
		{Key: "x_search", Name: "X (Twitter) Search", SetupTitle: "Select xAI Credential Source",
			SetupNote: "icebath routes X searches through xAI's built-in x_search Responses tool for read-only public X discovery. Use the xurl skill for authenticated X API reads and account actions. Both credential sources hit the same https://api.x.ai/v1/responses endpoint — pick whichever you already have. SuperGrok OAuth is preferred when both are set (uses your subscription quota instead of API spend).",
			Icon:      "🐦", Providers: []ProviderRow{
				{Name: "xAI Grok OAuth (SuperGrok / Premium+)", Badge: "subscription", Tag: "Browser login at accounts.x.ai — no API key required", PostSetup: "xai_grok"},
				{Name: "xAI API key", Badge: "paid", Tag: "Direct xAI API billing via XAI_API_KEY",
					EnvVars: []ProviderEnv{{Key: "XAI_API_KEY", Prompt: "xAI API key", URL: "https://console.x.ai/"}}},
			}},
		{Key: "browser", Name: "Browser Automation", Icon: "🌐", Providers: []ProviderRow{
			{Name: "Local Browser", Badge: "★ recommended · free", Tag: "Headless Chromium, no API key needed", BrowserProvider: "local", BrowserEngine: "auto", PostSetup: "chromium"},
			{Name: "Lightpanda", Badge: "free · local · no Chromium", Tag: "Zig headless browser spawned by icebath, text-only (no screenshots)",
				BrowserProvider: "local", BrowserEngine: "lightpanda", PostSetup: "lightpanda"},
			{Name: "Icebath Portal Subscription (Browser Use cloud)", Badge: "subscription", Tag: "Managed Browser Use billed to your subscription",
				BrowserProvider: "browser-use", PortalAuth: true, ManagedFeature: "browser", OverrideEnvVars: []string{"BROWSER_USE_API_KEY"}, PostSetup: "browserbase"},
		}},
		{Key: "computer_use", Name: "Computer Use (macOS/Windows/Linux)", Icon: "🖱️", PlatformGate: []string{"darwin", "windows", "linux"},
			Providers: []ProviderRow{
				{Name: "cua-driver (background)", Badge: "★ recommended · free · local",
					Tag:                "Background computer-use via cua-driver — does NOT steal your cursor or focus. Works with any model.",
					ComputerUseBackend: "cua", PostSetup: "cua_driver"},
			}},
	}
}
