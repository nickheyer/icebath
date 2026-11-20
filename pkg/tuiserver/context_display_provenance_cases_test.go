package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
)

// test_context_display_provenance.py test_preflight_seed_does_not_label_actual_usage_estimated
func TestPreflightSeedDoesNotLabelActualUsageEstimated(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("model:\n  context_length: 100000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a, err := agent.NewAIAgent(ctx, agent.AIAgentOptions{Model: "fixture", Provider: "custom", BaseURL: "http://127.0.0.1:9/v1",
		APIKey: "k", QuietMode: true, SkipContextFiles: true, SkipMemory: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	comp, ok := a.ContextCompressor().(*agent.ContextCompressor)
	if !ok {
		t.Fatalf("context engine %T", a.ContextCompressor())
	}
	comp.MaybeSeedPreflightDisplayTokens(1234)
	if got := getUsage(ctx, a)["context_estimated"]; got != true {
		t.Fatalf("seeded context_estimated %v", got)
	}
	comp.UpdateFromResponse(map[string]any{"prompt_tokens": 1234, "completion_tokens": 20})
	usage := getUsage(ctx, a)
	if usage["context_estimated"] != false || usage["context_used"] != 1234 {
		t.Fatalf("usage %v", usage)
	}
	comp.LastPromptTokens = -1
	if _, present := getUsage(ctx, a)["context_used"]; present {
		t.Fatalf("usage %v", getUsage(ctx, a))
	}
	if got := agent.ContextDisplaySource(comp); got != "provider_usage" {
		t.Fatalf("display source %q", got)
	}
}
