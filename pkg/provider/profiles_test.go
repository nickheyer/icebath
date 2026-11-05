package provider

import "testing"

func TestNormalizeProviderAliases(t *testing.T) {
	cases := map[string]string{
		"OpenAI": "openrouter", "glm": "zai", "grok": "xai", "kimi": "kimi-coding", "kimi-for-coding": "kimi-coding",
		"ollama": "custom", "vllm": "custom", "zen": "opencode-zen", "kilo": "kilocode", "vercel": "ai-gateway",
		"claude": "anthropic", "chatgpt": "openai-codex", "aws": "bedrock", "qwen": "qwen-oauth", "google": "gemini",
		"azure": "azure-foundry", "meta": "meta-ai", "ramp": "router", "unknown-thing": "unknown-thing",
	}
	for in, want := range cases {
		if got := NormalizeProvider(in); got != want {
			t.Errorf("NormalizeProvider(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProfileForResolvesAliasesAndDefaults(t *testing.T) {
	p, ok := ProfileFor("moonshot")
	if !ok || p.Name != "kimi-coding" || p.BaseURL != "https://api.moonshot.ai/v1" || !p.OmitTemperature || p.DefaultMaxTokens != 32000 {
		t.Fatalf("kimi profile wrong: %+v ok=%v", p, ok)
	}
	if h := p.DefaultHeaders(); h["Accept-Encoding"] != "gzip" || h["User-Agent"] == "" {
		t.Fatalf("kimi headers wrong: %v", h)
	}
	a, _ := ProfileFor("anthropic")
	if a.Mode != ModeAnthropicMessages || a.Auth != AuthAPIKey || a.ModelsURL != "https://api.anthropic.com/models" {
		t.Fatalf("anthropic profile wrong: %+v", a)
	}
	b, _ := ProfileFor("amazon")
	if b.Mode != modeBedrockConverse || b.Auth != AuthAWSSigV4 || b.ModelListing || b.ModelsURL != "" {
		t.Fatalf("bedrock profile wrong: %+v", b)
	}
	v, _ := ProfileFor("vertexai")
	if v.Auth != AuthGCPOAuth || v.AuthType != AuthTypeVertex {
		t.Fatalf("vertex profile wrong: %+v", v)
	}
	if _, ok := ProfileFor("nope"); ok {
		t.Fatal("unknown provider resolved")
	}
	seen := map[string]bool{}
	for _, p := range Profiles() {
		if seen[p.Name] {
			t.Fatalf("duplicate profile %s", p.Name)
		}
		seen[p.Name] = true
		if p.Mode == "" || p.Auth == "" || p.AuthType == "" {
			t.Fatalf("profile %s missing defaults: %+v", p.Name, p)
		}
	}
	if len(seen) < 50 {
		t.Fatalf("expected the full provider table, got %d", len(seen))
	}
}

func TestHostMandatedAndDetectedModes(t *testing.T) {
	cases := []struct {
		url  string
		want APIMode
	}{
		{"https://api.openai.com/v1", ModeCodexResponses},
		{"https://eu.api.openai.com/v1", ModeCodexResponses},
		{"https://api.openai.com.attacker.test/v1", ""},
		{"https://proxy.test/api.openai.com/v1", ""},
		{"https://api.anthropic.com", ModeAnthropicMessages},
		{"https://api.minimax.io/anthropic", ModeAnthropicMessages},
		{"https://api.kimi.com/coding/v1", ModeAnthropicMessages},
		{"https://api.actual.inc/v1", ModeChatCompletions},
		{"https://api.router.com/v1", ModeCodexResponses},
		{"https://bedrock-runtime.us-east-1.amazonaws.com", modeBedrockConverse},
		{"http://localhost:11434/v1", ""},
	}
	for _, c := range cases {
		if got := HostMandatedMode(c.url); got != c.want {
			t.Errorf("HostMandatedMode(%q) = %q, want %q", c.url, got, c.want)
		}
	}
	if DetectModeForURL("https://api.x.ai/v1") != ModeCodexResponses || DetectModeForURL("https://api.z.ai/api/paas/v4") != "" {
		t.Fatal("DetectModeForURL wrong")
	}
	if DetermineMode("nous", "", "anthropic/claude-sonnet-4.6", "chat") != ModeChatCompletions || DetermineMode("nous", "", "anthropic/claude-sonnet-4.6", "native") != ModeAnthropicMessages {
		t.Fatal("nous dual wire wrong")
	}
	goProfile, _ := ProfileFor("opencode-go")
	zenProfile, _ := ProfileFor("opencode-zen")
	if goProfile.APIModeForModel("claude-sonnet-4.6", "") != "" || zenProfile.APIModeForModel("gpt-5.4", "") != "" {
		t.Fatal("opencode profiles declare a per model wire, upstream routes opencode per model in core")
	}
	if OpenCodeModelMode("opencode-go", "claude-sonnet-4.6") != ModeAnthropicMessages || OpenCodeModelMode("opencode-zen", "gpt-5.4") != ModeCodexResponses || OpenCodeModelMode("opencode-go", "glm-5") != ModeChatCompletions {
		t.Fatal("opencode per model mode wrong")
	}
	azure, _ := ProfileFor("azure-foundry")
	if azure.PerModelMode != nil || azure.APIModeForModel("gpt-5-mini", "") != "" || azure.Mode != ModeChatCompletions {
		t.Fatal("azure foundry profile declares a per model wire, upstream infers it at runtime resolution")
	}
	if AzureFoundryModelMode("gpt-5-mini") != ModeCodexResponses || AzureFoundryModelMode("openai/o3") != ModeCodexResponses || AzureFoundryModelMode("llama") != "" ||
		azureInferredAPIMode("gpt-5-mini", ModeChatCompletions) != ModeCodexResponses || azureInferredAPIMode("gpt-5-mini", ModeAnthropicMessages) != ModeAnthropicMessages {
		t.Fatal("azure foundry model mode wrong")
	}
	fakeModelsDevInfo(t, nil)
	if DetermineMode("opencode-go", "", "claude-sonnet-4.6", "") != ModeChatCompletions || DetermineMode("azure-foundry", "https://x.openai.azure.com", "gpt-5-mini", "") != ModeChatCompletions {
		t.Fatal("determine_api_mode reads the overlay transport, the per model wire belongs to the profile")
	}
	if ParseMode("openai") != ModeChatCompletions || ParseMode("responses") != ModeCodexResponses || ParseMode("garbage") != "" {
		t.Fatal("ParseMode wrong")
	}
}

func TestOpenCodeBaseURLNormalization(t *testing.T) {
	cases := []struct {
		provider, in string
		mode         APIMode
		want         string
	}{
		{"opencode-zen", "https://opencode.ai/zen/v1", ModeAnthropicMessages, "https://opencode.ai/zen"},
		{"opencode-zen", "https://opencode.ai/zen", ModeChatCompletions, "https://opencode.ai/zen/v1"},
		{"opencode-go", "https://opencode.ai/zen/v1", ModeChatCompletions, "https://opencode.ai/zen/go/v1"},
		{"opencode-go", "https://proxy.example/custom/v1", ModeAnthropicMessages, "https://proxy.example/custom"},
		{"opencode-go", "https://proxy.example/custom", ModeChatCompletions, "https://proxy.example/custom"},
		{"zai", "https://api.z.ai/api/paas/v4", ModeChatCompletions, "https://api.z.ai/api/paas/v4"},
	}
	for _, c := range cases {
		if got := NormalizeOpenCodeBaseURL(c.provider, c.mode, c.in); got != c.want {
			t.Errorf("NormalizeOpenCodeBaseURL(%q,%q,%q) = %q, want %q", c.provider, c.mode, c.in, got, c.want)
		}
	}
	if NormalizeOpenCodeModelID("opencode-go", "opencode-go/glm-5") != "glm-5" {
		t.Fatal("opencode model id prefix not stripped")
	}
}

func TestURLHelpers(t *testing.T) {
	if !IsOllamaEndpoint("http://localhost:11434") || !IsOllamaEndpoint("https://ollama.com/v1") || !IsOllamaEndpoint("http://my.ollama.lan/v1") || IsOllamaEndpoint("http://localhost:8080/v1") || IsOllamaEndpoint("http://host:99999") {
		t.Fatal("IsOllamaEndpoint wrong")
	}
	if !HostMatches("https://us.api.openai.com/v1", "openai.com") || HostMatches("https://openai.com.evil.test", "openai.com") {
		t.Fatal("HostMatches wrong")
	}
	if !IsLoopbackHost("127.0.0.1") || !IsLoopbackHost("localhost") || !IsLoopbackHost("::1") || IsLoopbackHost("10.0.0.5") {
		t.Fatal("IsLoopbackHost wrong")
	}
	if NormalizeActualBaseURL("https://api.actual.inc/") != "https://api.actual.inc/v1" || NormalizeActualBaseURL("http://localhost:9000/v1/") != "http://localhost:9000/v1" {
		t.Fatal("NormalizeActualBaseURL wrong")
	}
	if !IsAggregator("openrouter") || !IsAggregator("custom:myproxy") || IsAggregator("deepseek") || IsRoutingAggregator("opencode-go") || !IsRoutingAggregator("openrouter") {
		t.Fatal("aggregator flags wrong")
	}
	if Label("kimi") != "Kimi / Moonshot" || Label("custom:lab") != "lab" || Label("mystery") != "mystery" {
		t.Fatal("Label wrong")
	}
}
