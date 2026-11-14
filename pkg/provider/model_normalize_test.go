package provider

import "testing"

func TestOpenCodeGoPreservesDots(t *testing.T) {
	for _, model := range []string{"minimax-m2.7", "minimax-m2.5", "glm-4.5", "kimi-k2.5", "some-model-1.0.3"} {
		if got := NormalizeModelForProvider(model, "opencode-go"); got != model {
			t.Errorf("%s: got %q", model, got)
		}
	}
}

func TestOpenAICodexStripsOpenAIPrefix(t *testing.T) {
	if got := NormalizeModelForProvider("openai/gpt-5.4", "openai-codex"); got != "gpt-5.4" {
		t.Fatalf("got %q", got)
	}
}

func TestDeepseekVSeriesPassThrough(t *testing.T) {
	if got := NormalizeModelForProvider("deepseek-v4-pro", "deepseek"); got != "deepseek-v4-pro" {
		t.Fatalf("got %q", got)
	}
	if got := NormalizeModelForProvider("deepseek-flash", "deepseek"); got != "deepseek-flash" {
		t.Fatalf("got %q", got)
	}
}

func TestDeepseekRetiredAliases(t *testing.T) {
	if got := NormalizeModelForProvider("deepseek-reasoner", "deepseek"); got != "deepseek-flash" {
		t.Fatalf("got %q", got)
	}
	for _, model := range []string{"deepseek-v4.1-flash", "deepseek-v4-flash-0731", "deepseek-r1", "deepseek-next-preview", "my-fine-tune"} {
		if got := normalizeForDeepseek(model); got != model {
			t.Errorf("%s: got %q", model, got)
		}
	}
}

func TestNvidiaPrefixRepair(t *testing.T) {
	cases := map[string]string{
		"nemotron-3-ultra-550b-a55b":             "nvidia/nemotron-3-ultra-550b-a55b",
		"nemotron-3-super-120b-a12b":             "nvidia/nemotron-3-super-120b-a12b",
		"nemotron-3-nano-omni-30b-a3b-reasoning": "nvidia/nemotron-3-nano-omni-30b-a3b-reasoning",
		"glm-5.2":                                "z-ai/glm-5.2",
		"nvidia/nemotron-3-ultra-550b-a55b":      "nvidia/nemotron-3-ultra-550b-a55b",
		"z-ai/glm-5.2":                           "z-ai/glm-5.2",
		"my-local-nim-container":                 "my-local-nim-container",
		"some-finetune-v2":                       "some-finetune-v2",
	}
	for model, want := range cases {
		if got := NormalizeModelForProvider(model, "nvidia"); got != want {
			t.Errorf("%s: got %q want %q", model, got, want)
		}
	}
	if got := NormalizeModelForProvider("my-model", "custom"); got != "my-model" {
		t.Fatalf("custom: got %q", got)
	}
	if got := NormalizeModelForProvider("claude-sonnet-4.6", "openrouter"); got != "anthropic/claude-sonnet-4.6" {
		t.Fatalf("openrouter: got %q", got)
	}
	if got := SuggestPrefixedModelID("nvidia", "glm-5.2"); got != "z-ai/glm-5.2" {
		t.Fatalf("suggest: got %q", got)
	}
	if got := SuggestPrefixedModelID("nvidia", "z-ai/glm-5.2"); got != "" {
		t.Fatalf("suggest prefixed: got %q", got)
	}
	if got := SuggestPrefixedModelID("nvidia", "unknown-model"); got != "" {
		t.Fatalf("suggest unknown: got %q", got)
	}
}

func TestColonProviderPrefixStrippedLikeSlash(t *testing.T) {
	cases := []struct{ model, provider, want string }{
		{"openai:gpt-5.4", "openai-codex", "gpt-5.4"},
		{"zai:glm-5.1", "zai", "glm-5.1"},
		{"custom:qwen3:8b", "custom", "qwen3:8b"},
		{"qwen3:8b", "custom", "qwen3:8b"},
		{"anthropic:claude-x", "openai-codex", "anthropic:claude-x"},
	}
	for _, c := range cases {
		if got := NormalizeModelForProvider(c.model, c.provider); got != c.want {
			t.Errorf("%s/%s: got %q want %q", c.model, c.provider, got, c.want)
		}
	}
}

func TestProfileNormalizerWinsOverGenericRules(t *testing.T) {
	const name = "l4-fake-idspace"
	RegisterModelIDNormalizer(name, func(model, _ string) string {
		bare := stripVendorPrefix(model)
		if bare == "fancy-4-5" {
			return "fancy-4.5"
		}
		return model
	})
	t.Cleanup(func() { RegisterModelIDNormalizer(name, nil) })
	if got := NormalizeModelForProvider("fancy-4-5", name); got != "fancy-4.5" {
		t.Fatalf("got %q", got)
	}
	if got := NormalizeModelForProvider("vendor/fancy-4-5", name); got != "fancy-4.5" {
		t.Fatalf("prefixed: got %q", got)
	}
}

func TestProfilesWithoutHookKeepGenericNormalization(t *testing.T) {
	if got := NormalizeModelForProvider("openai/gpt-5.4", "openai-codex"); got != "gpt-5.4" {
		t.Fatalf("got %q", got)
	}
	if got := NormalizeModelForProvider("claude-sonnet-4.6", "anthropic"); got != "claude-sonnet-4-6" {
		t.Fatalf("got %q", got)
	}
}

func TestDetectVendor(t *testing.T) {
	cases := map[string]string{"claude-sonnet-4.6": "anthropic", "qwen3.5-plus": "qwen", "vendor/x": "vendor", "mystery": "", "": ""}
	for model, want := range cases {
		if got := DetectVendor(model); got != want {
			t.Errorf("%s: got %q want %q", model, got, want)
		}
	}
}
