package config

import (
	"reflect"
	"testing"
)

func effortOf(level string) map[string]any { return map[string]any{"enabled": true, "effort": level} }

func TestParseReasoningEffortRejectsEmptyAndUnknown(t *testing.T) {
	for _, v := range []any{"", "   ", "\t", "\n", "bogus", "very-high", "0", "off", "true", "default", nil, true} {
		if got := ParseReasoningEffort(v); got != nil {
			t.Errorf("ParseReasoningEffort(%q) = %v", v, got)
		}
	}
	for v, want := range map[any]map[string]any{
		"none": {"enabled": false}, false: {"enabled": false}, " HIGH ": effortOf("high"), "ultra": effortOf("ultra"),
	} {
		if got := ParseReasoningEffort(v); !reflect.DeepEqual(got, want) {
			t.Errorf("ParseReasoningEffort(%v) = %v", v, got)
		}
	}
}

func TestParseReasoningEffortDictForm(t *testing.T) {
	if got := ParseReasoningEffort(map[string]any{"enabled": false, "effort": "low"}); !reflect.DeepEqual(got, map[string]any{"enabled": false}) {
		t.Fatal(got)
	}
	if ParseReasoningEffort(map[string]any{"enabled": true}) != nil || ParseReasoningEffort(map[string]any{"effort": 0}) != nil {
		t.Fatal("empty dict effort parsed")
	}
	if got := ParseReasoningEffort(map[string]any{"enabled": true, "effort": " thinking "}); !reflect.DeepEqual(got, effortOf("thinking")) {
		t.Fatal(got)
	}
	if ParseReasoningEffort("thinking") != nil {
		t.Fatal("bare bespoke tier accepted")
	}
}

func TestResolvePerModelReasoningEffort(t *testing.T) {
	cases := []struct {
		model     string
		overrides map[string]any
		want      map[string]any
	}{
		{"claude-opus-4.5", map[string]any{"claude-opus-4.5": "xhigh"}, effortOf("xhigh")},
		{"", map[string]any{"gpt-5": "low"}, nil},
		{"claude-opus-4.5", map[string]any{"claude-opus-4.5": "high", "claude-opus-4-5": "xhigh"}, effortOf("high")},
		{"qwen3.6:27b-q4_k_m", map[string]any{"ollama-local/qwen3.6:27b-q4_k_m": "low"}, effortOf("low")},
		{"llama3.2:3b", map[string]any{"ollama-local/qwen3.6:27b-q4_k_m": "low"}, nil},
		{"qwen3.6:27b", map[string]any{"qwen3.6:27b": "medium", "ollama-local/qwen3.6:27b": "low"}, effortOf("medium")},
		{"claude-opus-4-5", map[string]any{"anthropic/claude-opus-4.5": "xhigh"}, effortOf("xhigh")},
		{"openai/gpt-5", map[string]any{"gpt-5": "low"}, effortOf("low")},
		{"x-4.5", map[string]any{"x.4.5": "low"}, effortOf("low")},
		{"openrouter/anthropic/x-4", map[string]any{"anthropic/x-4": "max"}, effortOf("max")},
	}
	for _, c := range cases {
		if got := ResolvePerModelReasoningEffort(c.model, c.overrides); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q over %v = %v, want %v", c.model, c.overrides, got, c.want)
		}
	}
	if ResolvePerModelReasoningEffort("gpt-5", "bad") != nil {
		t.Fatal("non dict overrides matched")
	}
}

func TestReverseLookupFollowsFileOrder(t *testing.T) {
	overrides := map[string]any{"b-relay/qwen3": "low", "a-relay/qwen3": "high"}
	SetMappingKeys(overrides, []string{"b-relay/qwen3", "a-relay/qwen3"})
	if got := ResolvePerModelReasoningEffort("qwen3", overrides); !reflect.DeepEqual(got, effortOf("low")) {
		t.Fatalf("got %v, want the first key in file order", got)
	}
	overrides = map[string]any{"b-relay/qwen3": "bogus", "a-relay/qwen3": "high"}
	SetMappingKeys(overrides, []string{"b-relay/qwen3", "a-relay/qwen3"})
	if got := ResolvePerModelReasoningEffort("qwen3", overrides); !reflect.DeepEqual(got, effortOf("high")) {
		t.Fatalf("got %v, want the next parseable key", got)
	}
}

func reasoningCfg(effort any, overrides map[string]any, defaultModel string) map[string]any {
	if overrides == nil {
		overrides = map[string]any{}
	}
	return map[string]any{
		"model": map[string]any{"default": defaultModel},
		"agent": map[string]any{"reasoning_effort": effort, "reasoning_overrides": overrides},
	}
}

func TestResolveReasoningConfig(t *testing.T) {
	if got := ResolveReasoningConfig(reasoningCfg("medium", map[string]any{"claude-opus-4.5": "xhigh"}, "gpt-5"), "claude-opus-4.5"); !reflect.DeepEqual(got, effortOf("xhigh")) {
		t.Fatal(got)
	}
	if got := ResolveReasoningConfig(reasoningCfg("medium", map[string]any{"gpt-5": "high"}, "gpt-5"), ""); !reflect.DeepEqual(got, effortOf("high")) {
		t.Fatal(got)
	}
	for _, cfg := range []any{
		map[string]any{"agent": "oops", "model": 42},
		map[string]any{"agent": nil, "model": nil},
		map[string]any{"agent": map[string]any{"reasoning_overrides": "bad"}},
		nil,
	} {
		if got := ResolveReasoningConfig(cfg, ""); got != nil {
			t.Fatalf("malformed %v gave %v", cfg, got)
		}
	}
	if got := ResolveReasoningConfig(reasoningCfg("medium", map[string]any{"gpt-5": "turbo-max"}, "gpt-5"), "gpt-5"); !reflect.DeepEqual(got, effortOf("medium")) {
		t.Fatal(got)
	}
	bespoke := reasoningCfg(map[string]any{"enabled": true, "effort": "thinking"}, map[string]any{"lumo-max": map[string]any{"enabled": true, "effort": "fast"}}, "gpt-5")
	if got := ResolveReasoningConfig(bespoke, "gpt-5"); !reflect.DeepEqual(got, effortOf("thinking")) {
		t.Fatal(got)
	}
	if got := ResolveReasoningConfig(bespoke, "my-relay/lumo-max"); !reflect.DeepEqual(got, effortOf("fast")) {
		t.Fatal(got)
	}
	if got := ResolveReasoningConfig(reasoningCfg(false, nil, "gpt-5"), ""); !reflect.DeepEqual(got, map[string]any{"enabled": false}) {
		t.Fatalf("YAML false re-enabled thinking: %v", got)
	}
	if got := ResolveReasoningConfig(Tree{"model": map[string]any{"model": " gpt-5 "}, "agent": map[string]any{"reasoning_overrides": map[string]any{"gpt-5": "low"}}}, ""); !reflect.DeepEqual(got, effortOf("low")) {
		t.Fatal(got)
	}
}

func TestResolvePerModelProviderRouting(t *testing.T) {
	models := map[string]any{"anthropic/claude-opus-4.5": map[string]any{"order": []any{"anthropic"}}, "gpt-5": "not a dict"}
	if got := ResolvePerModelProviderRouting("claude-opus-4-5", models); !reflect.DeepEqual(got, models["anthropic/claude-opus-4.5"]) {
		t.Fatal(got)
	}
	for _, c := range []struct {
		model  string
		models any
	}{{"gpt-5", models}, {"", models}, {"gpt-5", nil}, {"gpt-5", "x"}} {
		if got := ResolvePerModelProviderRouting(c.model, c.models); got == nil || len(got) != 0 {
			t.Fatalf("%v gave %v", c, got)
		}
	}
}
