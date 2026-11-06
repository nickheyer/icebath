package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Home directory holding a config file, the process environment left untouched
func runtimeExportsHome(t *testing.T, configYAML string) string {
	t.Helper()
	h := t.TempDir()
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestResolveRuntimeProviderExportHonorsExplicitEndpoint(t *testing.T) {
	h := runtimeExportsHome(t, "model:\n  default: m\n  provider: custom\n")
	rt, err := ResolveRuntimeProvider(context.Background(), h, "custom", "m", "http://127.0.0.1:18080/v1", "sk-explicit")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimRight(rt.BaseURL, "/") != "http://127.0.0.1:18080/v1" || rt.APIKey != "sk-explicit" {
		t.Fatalf("runtime %+v", rt)
	}
	direct, err := modelsResolver(ctxForHome(context.Background(), h)).ResolveRuntime("custom", "sk-explicit", "http://127.0.0.1:18080/v1", nil, "m")
	if err != nil || direct.BaseURL != rt.BaseURL || direct.Provider != rt.Provider {
		t.Fatalf("export diverged from the resolver %+v %+v %v", rt, direct, err)
	}
}

func TestResolveFallbackEntryAPIKeyExport(t *testing.T) {
	h := runtimeExportsHome(t, "model:\n  default: m\n")
	t.Setenv("FALLBACK_ENTRY_KEY", "  sk-from-env  ")
	ctx := context.Background()
	if got := ResolveFallbackEntryAPIKey(ctx, h, map[string]any{"api_key": " sk-inline ", "key_env": "FALLBACK_ENTRY_KEY"}); got != "sk-inline" {
		t.Fatalf("inline %q", got)
	}
	if got := ResolveFallbackEntryAPIKey(ctx, h, map[string]any{"api_key_env": "FALLBACK_ENTRY_KEY"}); got != "sk-from-env" {
		t.Fatalf("key env alias %q", got)
	}
	if got := ResolveFallbackEntryAPIKey(ctx, h, nil); got != "" {
		t.Fatalf("nil entry %q", got)
	}
}

func TestTrialModelExportReadsTheHomeIdentity(t *testing.T) {
	h := runtimeExportsHome(t, "model:\n  default: m\n")
	ctx := context.Background()
	if got, want := TrialModel(ctx, h), trialModel(modelsPoolEnv(ctxForHome(ctx, h))); got != want || got != "" {
		t.Fatalf("trial model %q want empty %q", got, want)
	}
}

func TestTrialModelExportCarriesTheClaimedWelcomeTrial(t *testing.T) {
	_, env := installAnonPortal(t)
	anonClaimedTrial(t, env)
	if got := TrialModel(context.Background(), os.Getenv("ICEBATH_HOME")); got != GuestModel {
		t.Fatalf("claimed trial model %q want %q", got, GuestModel)
	}
}
