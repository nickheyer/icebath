package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

func TestOneshotAndTuiQualifiedModelNeverReachesDefaultProvider(t *testing.T) {
	h := filepath.Join(t.TempDir(), "icebath-home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("model:\n  default: claude-sonnet-4-5\n  provider: anthropic\n"+
		"providers:\n  jetson-vllm:\n    base_url: http://127.0.0.1:8000/v1\n"+
		"    api_key: EMPTY\n    api_mode: chat_completions\n    models: [nemotron-nano-30b]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	setDirectAliases(t)
	provider := func(context.Context, string, string) (string, string, bool) {
		t.Fatal("auto-detection ran on a provider-qualified model")
		return "", "", false
	}
	swapSeam(t, &oneshotDetectProviderForModel, provider)
	cfg, err := config.Load(home.Dir(context.Background()), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_INFERENCE_PROVIDER", "")
	os.Unsetenv("ICEBATH_INFERENCE_PROVIDER")
	t.Setenv("ICEBATH_TUI_PROVIDER", "")
	os.Unsetenv("ICEBATH_TUI_PROVIDER")
	t.Setenv("ICEBATH_INFERENCE_MODEL", "custom:jetson-vllm:nemotron-nano-30b")
	choice, err := resolveOneshotModelAndProvider(context.Background(), cfg, "", "")
	if err != nil || choice.Provider != "custom:jetson-vllm" || choice.Model != "nemotron-nano-30b" {
		t.Fatalf("choice %+v err %v", choice, err)
	}
	if model, prov := tuiserver.ResolveStartupRuntime(context.Background()); model != "nemotron-nano-30b" || prov != "custom:jetson-vllm" {
		t.Fatalf("tui startup runtime %q %q", model, prov)
	}
}
