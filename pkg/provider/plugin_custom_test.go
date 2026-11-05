package provider

import (
	"context"
	"testing"
)

func TestCustomPluginProfileMatchesUpstreamRegistration(t *testing.T) {
	p := bundledPluginProfile(t, "custom")
	assertProfileFields(t, p, map[string]any{
		"name": "custom", "aliases": []string{"ollama", "local", "vllm", "llamacpp", "llama.cpp", "llama-cpp"}, "env_vars": []string{},
		"base_url": "", "base_url_env_var": "", "default_max_tokens": 0, "api_mode": "chat_completions", "auth_type": "api_key",
		"display_name": "", "supports_model_listing": true,
	})
	if r := p.DefaultReasoningConfig("kimi-k3"); r == nil || r.Enabled == nil || !*r.Enabled || r.Effort != "medium" {
		t.Fatalf("default reasoning %#v", r)
	}
	if got, ok, err := p.SupportedReasoningEfforts(context.Background(), "anything"); err != nil || !ok || len(got) == 0 || got[len(got)-1] != "max" {
		t.Fatalf("declared efforts %v %v", got, ok)
	}
	if ids, ok := p.FetchModels(context.Background(), "", "", 0); ok || ids != nil {
		t.Fatalf("fetch without a base url %v %v", ids, ok)
	}
}

func TestCustomReasoningWireShape(t *testing.T) {
	p := bundledPluginProfile(t, "custom")
	disabled := &Reasoning{Enabled: bptr(false)}
	effortNone := &Reasoning{Enabled: bptr(true), Effort: "none"}
	cases := []struct {
		label  string
		r      *Reasoning
		kw     map[string]any
		eb, tl map[string]any
	}{
		{"no config", nil, map[string]any{"model": "glm-5.2"}, map[string]any{}, map[string]any{}},
		{"disabled ollama", disabled, map[string]any{"model": "qwen3", "base_url": "http://127.0.0.1:11434/v1"},
			map[string]any{"think": false}, map[string]any{"reasoning_effort": "none"}},
		{"effort none ollama", effortNone, map[string]any{"model": "qwen3", "base_url": "http://localhost:11434/v1"},
			map[string]any{"think": false}, map[string]any{"reasoning_effort": "none"}},
		{"mistral", effortNone, map[string]any{"model": "mistral-small-latest", "base_url": "https://api.mistral.ai/v1"},
			map[string]any{}, map[string]any{"reasoning_effort": "none"}},
		{"no base url", disabled, map[string]any{"model": "glm-5.2"}, map[string]any{}, map[string]any{"reasoning_effort": "none"}},
		{"ollama cloud", disabled, map[string]any{"model": "qwen3", "base_url": "https://ollama.com/v1"},
			map[string]any{"think": false}, map[string]any{"reasoning_effort": "none"}},
		{"num ctx alone", nil, map[string]any{"model": "qwen3", "ollama_num_ctx": 8192}, map[string]any{"options": map[string]any{"num_ctx": 8192}}, map[string]any{}},
		{"groq enabled", effortCfg("high"), map[string]any{"base_url": "https://api.groq.com/openai/v1"}, map[string]any{}, map[string]any{"reasoning_effort": "default"}},
		{"groq disabled", &Reasoning{Enabled: bptr(false), Effort: "medium"}, map[string]any{"base_url": "https://api.groq.com/openai/v1"},
			map[string]any{}, map[string]any{"reasoning_effort": "none"}},
		{"ultra clamps", effortCfg("ultra"), map[string]any{"model": "glm-5.2"}, map[string]any{}, map[string]any{"reasoning_effort": "max"}},
	}
	for _, base := range []string{"http://127.0.0.1:8080/v1", "http://localhost:1234/v1", "https://api.groq.com/openai/v1"} {
		cases = append(cases, struct {
			label  string
			r      *Reasoning
			kw     map[string]any
			eb, tl map[string]any
		}{"non ollama relay " + base, &Reasoning{Effort: "none"}, map[string]any{"model": "llama3", "base_url": base}, map[string]any{}, map[string]any{"reasoning_effort": "none"}})
	}
	for _, base := range []string{"http://myhost:99999/v1", "http://localhost:80a/v1", "http://localhost:11434./v1"} {
		cases = append(cases, struct {
			label  string
			r      *Reasoning
			kw     map[string]any
			eb, tl map[string]any
		}{"malformed port " + base, disabled, map[string]any{"model": "qwen3", "base_url": base}, map[string]any{}, map[string]any{"reasoning_effort": "none"}})
	}
	for _, effort := range []string{"minimal", "low", "medium", "high", "xhigh", "max"} {
		cases = append(cases, struct {
			label  string
			r      *Reasoning
			kw     map[string]any
			eb, tl map[string]any
		}{"enabled " + effort, effortCfg(effort), map[string]any{"model": "glm-5.2"}, map[string]any{}, map[string]any{"reasoning_effort": effort}})
	}
	for _, c := range cases {
		eb, tl := p.BuildAPIKwargsExtras(c.r, c.kw)
		assertExtras(t, c.label, eb, tl, c.eb, c.tl)
	}
}

func TestCustomNamedRelayKeepsConfiguredMaxOnTheResponsesWire(t *testing.T) {
	p := bundledPluginProfile(t, "custom")
	_, tl := p.BuildAPIKwargsExtras(effortCfg("max"), map[string]any{"base_url": "https://relay.example/v1"})
	kw := build(t, newCodexTransport(), "deepseek-flash", userMsgs("Hi"), nil,
		codexParams{provider: "custom:relay", baseURL: "https://relay.example/v1", reasoning: reasoningCfg{set: true, effort: "max"}})
	if got := routerEffort(t, kw); got != "max" || tl["reasoning_effort"] != "max" {
		t.Fatalf("responses effort %v chat effort %v", got, tl["reasoning_effort"])
	}
}

func TestCustomEntryAtTheOfficialOpenAIOriginKeepsThePerModelLadder(t *testing.T) {
	bundledPluginProfile(t, "custom")
	kw := build(t, newCodexTransport(), "gpt-5.2", userMsgs("Hi"), nil,
		codexParams{provider: "custom:oai", baseURL: "https://api.openai.com/v1", reasoning: reasoningCfg{set: true, effort: "max"}})
	if got := routerEffort(t, kw); got != "xhigh" {
		t.Fatalf("effort %v", got)
	}
}
