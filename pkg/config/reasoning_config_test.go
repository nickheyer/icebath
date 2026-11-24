package config

import (
	"reflect"
	"testing"
)

func effortOf(level string) map[string]any { return map[string]any{"enabled": true, "effort": level} }

func TestEmptyOrWhitespaceReturnsNone(t *testing.T) {
	for _, v := range []string{"", "   ", "\t", "\n"} {
		t.Run(v, func(t *testing.T) {
			if got := ParseReasoningEffort(v); got != nil {
				t.Fatalf("ParseReasoningEffort(%q) = %v", v, got)
			}
		})
	}
}

func TestUnknownLevelsReturnNone(t *testing.T) {
	for _, v := range []string{"bogus", "very-high", "0", "off", "true", "default"} {
		t.Run(v, func(t *testing.T) {
			if got := ParseReasoningEffort(v); got != nil {
				t.Fatalf("ParseReasoningEffort(%q) = %v", v, got)
			}
		})
	}
}

func TestParseReasoningEffortSpellings(t *testing.T) {
	for _, v := range []any{nil, true} {
		if got := ParseReasoningEffort(v); got != nil {
			t.Errorf("ParseReasoningEffort(%v) = %v", v, got)
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

func TestResolvePerModelReasoningEffortExactMatch(t *testing.T) {
	overrides := map[string]any{"claude-opus-4.5": "xhigh"}
	if got := ResolvePerModelReasoningEffort("claude-opus-4.5", overrides); !reflect.DeepEqual(got, effortOf("xhigh")) {
		t.Fatal(got)
	}
}

func TestResolvePerModelReasoningEffortEmptyModelReturnsNone(t *testing.T) {
	if got := ResolvePerModelReasoningEffort("", map[string]any{"gpt-5": "low"}); got != nil {
		t.Fatal(got)
	}
}

func TestExactMatchWinsOverVariant(t *testing.T) {
	overrides := map[string]any{"claude-opus-4.5": "high", "claude-opus-4-5": "xhigh"}
	if got := ResolvePerModelReasoningEffort("claude-opus-4.5", overrides); !reflect.DeepEqual(got, effortOf("high")) {
		t.Fatal(got)
	}
}

func TestPrefixedKeyMatchesBareModel(t *testing.T) {
	overrides := map[string]any{"ollama-local/qwen3.6:27b-q4_k_m": "low"}
	if got := ResolvePerModelReasoningEffort("qwen3.6:27b-q4_k_m", overrides); !reflect.DeepEqual(got, effortOf("low")) {
		t.Fatal(got)
	}
	if got := ResolvePerModelReasoningEffort("llama3.2:3b", overrides); got != nil {
		t.Fatal(got)
	}
}

func TestDirectMatchWinsOverReverseLookup(t *testing.T) {
	overrides := map[string]any{"qwen3.6:27b": "medium", "ollama-local/qwen3.6:27b": "low"}
	if got := ResolvePerModelReasoningEffort("qwen3.6:27b", overrides); !reflect.DeepEqual(got, effortOf("medium")) {
		t.Fatal(got)
	}
}

func TestResolvePerModelReasoningEffortSpellingVariants(t *testing.T) {
	cases := []struct {
		model     string
		overrides map[string]any
		want      map[string]any
	}{
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

func TestInvalidOverrideValueFallsBackToGlobal(t *testing.T) {
	if got := ResolvePerModelReasoningEffort("gpt-5", map[string]any{"gpt-5": "turbo-max"}); got != nil {
		t.Fatalf("an unparseable override must give way to the global effort, got %v", got)
	}
}

func TestDictFormPassesBespokeTierVerbatimGloballyAndPerModel(t *testing.T) {
	if got := ParseReasoningEffort(map[string]any{"enabled": true, "effort": "thinking"}); !reflect.DeepEqual(got, effortOf("thinking")) {
		t.Fatal(got)
	}
	if got := ResolvePerModelReasoningEffort("my-relay/lumo-max", map[string]any{"lumo-max": map[string]any{"enabled": true, "effort": "fast"}}); !reflect.DeepEqual(got, effortOf("fast")) {
		t.Fatal(got)
	}
	if ParseReasoningEffort("thinking") != nil {
		t.Fatal("bare bespoke tier accepted")
	}
}

func TestDictFormDisabledOrEmptyEffort(t *testing.T) {
	if got := ParseReasoningEffort(map[string]any{"enabled": false, "effort": "low"}); !reflect.DeepEqual(got, map[string]any{"enabled": false}) {
		t.Fatal(got)
	}
	if ParseReasoningEffort(map[string]any{"enabled": true}) != nil {
		t.Fatal("dict without a level parsed")
	}
	if ParseReasoningEffort(map[string]any{"effort": 0}) != nil {
		t.Fatal("falsy dict level parsed")
	}
}

func TestSpellingTolerantLookupWorksWithUserConfig(t *testing.T) {
	if got := ResolvePerModelReasoningEffort("claude-opus-4-5", map[string]any{"anthropic/claude-opus-4.5": "xhigh"}); !reflect.DeepEqual(got, effortOf("xhigh")) {
		t.Fatal(got)
	}
	if got := ResolvePerModelReasoningEffort("openai/gpt-5", map[string]any{"gpt-5": "low"}); !reflect.DeepEqual(got, effortOf("low")) {
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

func TestFallbackReResolvesReasoningConfig(t *testing.T) {
	overrides := map[string]any{"claude-opus-4.5": "xhigh", "gemini-flash": "medium"}
	fb := ResolvePerModelReasoningEffort("claude-opus-4.5", overrides)
	if fb == nil || fb["effort"] != "xhigh" {
		t.Fatalf("fallback = %v", fb)
	}
	primary := ResolvePerModelReasoningEffort("gemini-flash", overrides)
	if primary == nil || primary["effort"] != "medium" {
		t.Fatalf("primary = %v", primary)
	}
	if fb["effort"] == primary["effort"] {
		t.Fatal("fallback and primary share an effort")
	}
}

func TestFallbackToModelWithoutOverrideUsesGlobal(t *testing.T) {
	if got := ResolvePerModelReasoningEffort("gpt-5", map[string]any{"claude-opus-4.5": "xhigh"}); got != nil {
		t.Fatal(got)
	}
}

func TestFallbackGlobalFallbackWithYamlFalse(t *testing.T) {
	fbAgentCfg := map[string]any{"reasoning_effort": false}
	effort, ok := fbAgentCfg["reasoning_effort"]
	if !ok {
		effort = ""
	}
	result := ParseReasoningEffort(effort)
	if result == nil || result["enabled"] != false {
		t.Fatalf("YAML false = %v", result)
	}
}
