package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func perModelRoutingAgent(model string, allowed any) *AIAgent {
	return &AIAgent{model: model, providersAllowed: allowed, providerSort: "price"}
}

func writePerModelRoutingConfig(t *testing.T) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	cfg := "provider_routing:\n  sort: price\n  models:\n    openai/gpt-6-astra:\n      only: [openai]\n    anthropic/claude-fable-5.1:\n      only: [anthropic]\n      sort: throughput\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPerModelEntryOverlaysFlatRoutingForThatModelOnly(t *testing.T) {
	writePerModelRoutingConfig(t)
	ctx := t.Context()
	got := perModelRoutingAgent("openai/gpt-6-astra", nil).providerPreferencesForAgent(ctx)
	if want := (map[string]any{"only": []any{"openai"}, "sort": "price"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("pinned model preferences %v want %v", got, want)
	}
	got = perModelRoutingAgent("anthropic/claude-fable-5.1", nil).providerPreferencesForAgent(ctx)
	if want := (map[string]any{"only": []any{"anthropic"}, "sort": "throughput"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("per model sort preferences %v want %v", got, want)
	}
	got = perModelRoutingAgent("moonshotai/kimi-k2.6", nil).providerPreferencesForAgent(ctx)
	if want := (map[string]any{"sort": "price"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("unlisted model preferences %v want %v", got, want)
	}
}

func TestPerModelMatchIsSpellingTolerantAndFollowsModelSwitch(t *testing.T) {
	writePerModelRoutingConfig(t)
	ctx := t.Context()
	a := perModelRoutingAgent("openrouter/openai/gpt-6-astra", []any{"together"})
	if got := a.providerPreferencesForAgent(ctx)["only"]; !reflect.DeepEqual(got, []any{"openai"}) {
		t.Fatalf("prefixed spelling only %v", got)
	}
	a.model = "claude-fable-5-1"
	if got := a.providerPreferencesForAgent(ctx)["only"]; !reflect.DeepEqual(got, []any{"anthropic"}) {
		t.Fatalf("switched model only %v", got)
	}
}
