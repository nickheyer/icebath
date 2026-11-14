package provider

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

var fakeAPIModels = []string{
	"anthropic/claude-opus-4.6",
	"anthropic/claude-sonnet-4.5",
	"openai/gpt-5.4-pro",
	"openai/gpt-5.4",
	"google/gemini-3-pro-preview",
}

func validateTestHome(t *testing.T) context.Context {
	t.Helper()
	ctx := modelsTestHome(t)
	prevProbe, prevLMS, prevOllama, prevAnth, prevStaged, prevMoa := validateProbeAPIModelsFn, validateProbeLMStudioFn, validateProbeOllamaFn, validateFetchAnthropicFn, validateStagedModelIDsFn, validateMoaPresetNamesFn
	prevBedrock, prevRegion, prevNous := validateDiscoverBedrock, validateBedrockRegionFn, fetchNousRecommendedModelsFn
	clearOllamaLocalCaches()
	t.Cleanup(func() {
		validateProbeAPIModelsFn, validateProbeLMStudioFn, validateProbeOllamaFn, validateFetchAnthropicFn, validateStagedModelIDsFn, validateMoaPresetNamesFn = prevProbe, prevLMS, prevOllama, prevAnth, prevStaged, prevMoa
		validateDiscoverBedrock, validateBedrockRegionFn, fetchNousRecommendedModelsFn = prevBedrock, prevRegion, prevNous
		clearOllamaLocalCaches()
	})
	return ctx
}

// Mirrors the Python _validate helper patching fetch_api_models and probe_api_models
func validateWith(ctx context.Context, model, provider string, apiModels []string, opts ValidateModelOptions) ModelVerdict {
	found := apiModels != nil
	fetchAPIModelsFn = func(ctx context.Context, apiKey, baseURL string, timeout time.Duration, apiMode string, headers map[string]string) ([]string, bool) {
		return cloneStrings(apiModels), found
	}
	resolved := opts.BaseURL
	if resolved == "" {
		resolved = "http://localhost:11434/v1"
	}
	validateProbeAPIModelsFn = func(ctx context.Context, apiKey, baseURL string, timeout time.Duration, apiMode string, requestHeaders map[string]string) ProbeResult {
		return ProbeResult{Models: cloneStrings(apiModels), Found: found, ProbedURL: "http://localhost:11434/v1/models", ResolvedBaseURL: resolved}
	}
	return ValidateRequestedModel(ctx, model, provider, opts)
}

func catalogIDs(ids ...string) func(ctx context.Context, provider string, forceRefresh bool) CatalogRows {
	return func(ctx context.Context, provider string, forceRefresh bool) CatalogRows {
		return CatalogRows{IDs: ids}
	}
}

func TestModelsValidateFormatChecks(t *testing.T) {
	ctx := validateTestHome(t)
	if r := validateWith(ctx, "", "openrouter", fakeAPIModels, ValidateModelOptions{}); r.Accepted || !strings.Contains(r.Message, "empty") {
		t.Fatalf("empty %+v", r)
	}
	if r := validateWith(ctx, "gpt-5.4", "openrouter", []string{"gpt-5.4", "gpt-5.4-pro"}, ValidateModelOptions{}); !r.Accepted || !r.Persist {
		t.Fatalf("no slash listed %+v", r)
	}
	if r := validateWith(ctx, "gpt-5.4", "openrouter", []string{"openai/gpt-5.4"}, ValidateModelOptions{}); r.Accepted || r.Persist || !strings.Contains(r.Message, "not found") {
		t.Fatalf("no slash unlisted %+v", r)
	}
	if r := validateWith(ctx, "anthropic/claude-opus-4.5", "openrouter", fakeAPIModels, ValidateModelOptions{}); r.Accepted || !strings.Contains(r.Message, "anthropic/claude-opus-4.6") {
		t.Fatalf("not listed %+v", r)
	}
}

func TestModelsValidateLMStudio(t *testing.T) {
	ctx := validateTestHome(t)
	modelsTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
		return jsonResponse(req, 200, jsonMap(`{"models":[{"key":"publisher/chat-model","id":"publisher/chat-model","type":"llm"},{"key":"publisher/embed-model","id":"publisher/embed-model","type":"embedding"}]}`), nil), nil
	})
	models, err := FetchLMStudioModels(ctx, nil, "http://localhost:1234/v1", 0)
	if err != nil || len(models) != 1 || models[0] != "publisher/chat-model" {
		t.Fatalf("models %v %v", models, err)
	}
	modelsTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
		return jsonResponse(req, 401, map[string]any{"error": "Unauthorized"}, nil), nil
	})
	r := ValidateRequestedModel(ctx, "publisher/chat-model", "lmstudio", ValidateModelOptions{BaseURL: "http://localhost:1234/v1"})
	if r.Accepted || !strings.Contains(r.Message, "401") || !strings.Contains(r.Message, "LM_API_KEY") {
		t.Fatalf("auth failure %+v", r)
	}
}

func TestModelsValidateRequestedIDIsNeverRewritten(t *testing.T) {
	ctx := validateTestHome(t)
	cases := []struct {
		requested string
		listing   []string
	}{
		{"deepseek-v4.1-flash", []string{"deepseek-v4-flash-0731", "deepseek-v4-flash"}},
		{"gemini-3.8-flash", []string{"gemini-3.6-flash", "gemini-3.6-pro"}},
		{"gpt5.3-codex", []string{"gpt-5.4", "gpt-5.3-codex"}},
	}
	for _, c := range cases {
		for _, route := range [][2]string{{"custom:hyper", "http://127.0.0.1:1/v1"}, {"openrouter", ""}} {
			r := validateWith(ctx, c.requested, route[0], c.listing, ValidateModelOptions{BaseURL: route[1]})
			if r.Recognized || !(strings.Contains(r.Message, "Similar models") || strings.Contains(r.Message, c.listing[len(c.listing)-1])) {
				t.Fatalf("%s via %s: %+v", c.requested, route[0], r)
			}
		}
	}
	providerModelIDsFn = catalogIDs("gpt-5.4-mini", "gpt-5.4", "gpt-5.3-codex")
	r := ValidateRequestedModel(ctx, "gpt5.3-codex", "openai-codex", ValidateModelOptions{})
	if r.Recognized || !strings.Contains(r.Message, "gpt-5.3-codex") {
		t.Fatalf("static near miss %+v", r)
	}
}

func TestModelsValidateCodex900kVariants(t *testing.T) {
	ctx := validateTestHome(t)
	providerModelIDsFn = catalogIDs("gpt-5.6-sol", "gpt-5.6-sol-900k", "gpt-5.5", "gpt-5.4-mini")
	if r := ValidateRequestedModel(ctx, "gpt-5.6-sol-900k", "openai-codex", ValidateModelOptions{}); !r.Accepted || !r.Recognized {
		t.Fatalf("listed variant %+v", r)
	}
	for _, alias := range []string{"gpt-5.5-900k", "gpt-5.4-mini-900k", "gpt-5.6-sol-pro-900k"} {
		if r := ValidateRequestedModel(ctx, alias, "openai-codex", ValidateModelOptions{}); r.Accepted || r.Persist || !strings.Contains(r.Message, "272K") {
			t.Fatalf("%s %+v", alias, r)
		}
	}
	providerModelIDsFn = catalogIDs("gpt-5.6-sol")
	if r := ValidateRequestedModel(ctx, "gpt-5.6-sol-900k", "openai-codex", ValidateModelOptions{}); !r.Accepted {
		t.Fatalf("missing valid variant %+v", r)
	}
}

func TestModelsValidateOpenRouterVariantSuffixes(t *testing.T) {
	ctx := validateTestHome(t)
	listing := []string{"~x-ai/grok-latest", "x-ai/grok-4.6", "deepseek/deepseek-v4-flash", "thinkingmachines/inkling:free"}
	for _, suffix := range []string{"nitro", "floor", "exacto", "online"} {
		r := validateWith(ctx, "~x-ai/grok-latest:"+suffix, "openrouter", listing, ValidateModelOptions{})
		if !r.Accepted || !r.Recognized || r.Message != "" {
			t.Fatalf("%s %+v", suffix, r)
		}
	}
	if r := validateWith(ctx, "x-ai/notreal-model:nitro", "openrouter", listing, ValidateModelOptions{}); r.Accepted {
		t.Fatalf("unknown base %+v", r)
	}
	if r := validateWith(ctx, "x-ai/grok-4.6:bogus", "openrouter", listing, ValidateModelOptions{}); r.Accepted {
		t.Fatalf("unknown suffix %+v", r)
	}
	if r := validateWith(ctx, "thinkingmachines/inkling:free", "openrouter", listing, ValidateModelOptions{}); !r.Accepted {
		t.Fatalf("free sku %+v", r)
	}
	if r := validateWith(ctx, "x-ai/grok-4.6:NITRO", "openrouter", listing, ValidateModelOptions{}); !r.Accepted {
		t.Fatalf("uppercase suffix %+v", r)
	}
	providerModelIDsFn = catalogIDs("x-ai/grok-4.6", "anthropic/claude-opus-4.6")
	r := validateWith(ctx, "x-ai/grok-4.6:floor", "openrouter", nil, ValidateModelOptions{BaseURL: "https://openrouter.ai/api/v1"})
	if !r.Accepted || !r.Recognized {
		t.Fatalf("static fallback variant %+v", r)
	}
}

func TestModelsValidateCustomUnreachableFallback(t *testing.T) {
	ctx := validateTestHome(t)
	run := func(model, provider string, models []string, apiMode string) ModelVerdict {
		validateProbeAPIModelsFn = func(ctx context.Context, apiKey, baseURL string, timeout time.Duration, mode string, headers map[string]string) ProbeResult {
			return ProbeResult{Models: cloneStrings(models), Found: models != nil, ProbedURL: "http://localhost:8000/v1/models", ResolvedBaseURL: "http://localhost:8000/v1"}
		}
		fetchAPIModelsFn = func(ctx context.Context, apiKey, baseURL string, timeout time.Duration, mode string, headers map[string]string) ([]string, bool) {
			return cloneStrings(models), models != nil
		}
		return ValidateRequestedModel(ctx, model, provider, ValidateModelOptions{APIKey: "k", BaseURL: "http://localhost:8000/v1", APIMode: apiMode})
	}
	for _, provider := range []string{"custom", "custom:myproxy"} {
		for _, mode := range []string{"chat_completions", "anthropic_messages"} {
			r := run("my-proxy-model", provider, nil, mode)
			if !r.Accepted || !r.Persist || r.Recognized || !strings.Contains(r.Message, "accepted without verification") {
				t.Fatalf("%s %s %+v", provider, mode, r)
			}
		}
	}
	for _, mode := range []string{"", "codex_responses"} {
		if r := run("my-proxy-model", "custom", nil, mode); r.Accepted || !strings.Contains(r.Message, "was not saved") {
			t.Fatalf("%q %+v", mode, r)
		}
	}
	if r := run("my-model", "custom", []string{"my-model"}, "chat_completions"); !r.Recognized {
		t.Fatalf("listed custom %+v", r)
	}
	r := run("kimi-k3", "kimi-coding", []string{"k3", "k3-turbo"}, "anthropic_messages")
	if !r.Accepted || !r.Persist || r.Recognized || strings.Contains(r.Message, "do not implement") || !strings.Contains(r.Message, "`k3`") {
		t.Fatalf("kimi alias %+v", r)
	}
	if r := run("K3", "kimi-coding", []string{"k3"}, "anthropic_messages"); !r.Recognized {
		t.Fatalf("case insensitive alias %+v", r)
	}
}

func TestModelsValidateModelIDWhitespace(t *testing.T) {
	ctx := validateTestHome(t)
	const spaces = "Model names cannot contain spaces."
	for _, c := range [][2]string{{"anthropic", "claude opus"}, {"openrouter", "anthropic/claude opus"}, {"openai", "gpt 5.4"}} {
		if r := validateWith(ctx, c[1], c[0], fakeAPIModels, ValidateModelOptions{}); r.Accepted || r.Persist || r.Message != spaces {
			t.Fatalf("%s %+v", c[0], r)
		}
	}
	for _, c := range [][2]string{{"anthropic", "https://api.anthropic.com"}, {"openai", "https://api.openai.com/v1"}, {"openrouter", "https://openrouter.ai/api/v1"}} {
		if r := validateWith(ctx, "claude opus", c[0], fakeAPIModels, ValidateModelOptions{BaseURL: c[1]}); r.Accepted || !strings.Contains(r.Message, "spaces") {
			t.Fatalf("stock %s %+v", c[0], r)
		}
	}
	if r := validateWith(ctx, "My Custom Model", "custom", []string{"My Custom Model"}, ValidateModelOptions{BaseURL: "http://127.0.0.1:8000/v1"}); !r.Accepted || !r.Recognized || !r.Persist {
		t.Fatalf("custom spaced %+v", r)
	}
	if r := validateWith(ctx, "Go reasoning", "custom:omniroute", []string{"Go reasoning"}, ValidateModelOptions{BaseURL: "http://127.0.0.1:20128/v1"}); !r.Accepted || !r.Recognized {
		t.Fatalf("named custom spaced %+v", r)
	}
	const model = "Meta Llama 3.1 8B"
	for _, provider := range []string{"vllm", "ollama", "local", "lmstudio"} {
		var r ModelVerdict
		if provider == "lmstudio" {
			validateProbeLMStudioFn = func(ctx context.Context, apiKey any, baseURL string, timeout time.Duration) ([]string, bool, error) {
				return []string{model}, true, nil
			}
			r = ValidateRequestedModel(ctx, model, provider, ValidateModelOptions{})
		} else {
			r = validateWith(ctx, model, provider, []string{model}, ValidateModelOptions{BaseURL: "http://127.0.0.1:8000/v1"})
		}
		if !r.Accepted || strings.Contains(r.Message, "spaces") {
			t.Fatalf("%s %+v", provider, r)
		}
	}
	if r := validateWith(ctx, "Go reasoning", "omniroute", []string{"Go reasoning"}, ValidateModelOptions{BaseURL: "http://127.0.0.1:20128/v1", APIKey: "sk-test"}); !r.Accepted || !r.Recognized || strings.Contains(r.Message, "spaces") {
		t.Fatalf("user configured base %+v", r)
	}
	if r := validateWith(ctx, "claude\topus", "anthropic", fakeAPIModels, ValidateModelOptions{}); r.Accepted || r.Message != spaces {
		t.Fatalf("tab %+v", r)
	}
}

func TestModelsValidatePickerRowsOmitWhitespaceIDs(t *testing.T) {
	rows := []any{
		map[string]any{"slug": "anthropic", "models": []any{"claude-opus-4.6", "claude opus"}, "total_models": 2, "source": "curated"},
		map[string]any{"slug": "omniroute", "models": []any{"Go reasoning", "plain-id"}, "total_models": 2, "api_url": "http://127.0.0.1:20128/v1"},
		map[string]any{"slug": "lmstudio", "models": []any{"Meta Llama 3.1 8B"}, "total_models": 1, "source": "local"},
	}
	DropUnofferableModelIDs(rows)
	anth := rows[0].(map[string]any)
	if got := anth["models"].([]any); len(got) != 1 || got[0] != "claude-opus-4.6" || anth["total_models"] != 1 {
		t.Fatalf("anthropic row %+v", anth)
	}
	if got := rows[1].(map[string]any)["models"].([]any); len(got) != 2 || got[0] != "Go reasoning" {
		t.Fatalf("omniroute row %+v", got)
	}
	if got := rows[2].(map[string]any)["models"].([]any); len(got) != 1 || got[0] != "Meta Llama 3.1 8B" {
		t.Fatalf("lmstudio row %+v", got)
	}
}

func TestModelsValidateLlamacppAcceptsListedSpacedID(t *testing.T) {
	ctx := validateTestHome(t)
	const model = "Meta Llama 3.1 8B"
	if r := validateWith(ctx, model, "llamacpp", []string{model}, ValidateModelOptions{BaseURL: "http://127.0.0.1:8000/v1"}); !r.Accepted || strings.Contains(r.Message, "spaces") {
		t.Fatalf("llamacpp %+v", r)
	}
}

func TestModelsValidateManagedLocal(t *testing.T) {
	ctx := validateTestHome(t)
	run := func(model string, staged []string) ModelVerdict {
		validateStagedModelIDsFn = func() []string { return cloneStrings(staged) }
		return validateWith(ctx, model, "llamacpp", []string{"other-model"}, ValidateModelOptions{BaseURL: "http://127.0.0.1:18434/v1"})
	}
	if r := run("Qwen3-4B-Q4_K_M", []string{"Qwen3-4B-Q4_K_M"}); !r.Accepted || !r.Persist || !r.Recognized {
		t.Fatalf("staged %+v", r)
	}
	if r := run("Llama-4-90B-Q4_K_M", nil); r.Accepted {
		t.Fatalf("unstaged %+v", r)
	}
}
