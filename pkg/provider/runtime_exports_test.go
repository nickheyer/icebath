package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
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

func TestHostGatedEnvKeyCandidatesExportGatesKeysByHost(t *testing.T) {
	h := runtimeExportsHome(t, "model:\n  default: m\n")
	if err := os.WriteFile(filepath.Join(h, ".env"), []byte("OPENAI_API_KEY=sk-oai\nOPENROUTER_API_KEY=sk-or\nOLLAMA_API_KEY=sk-oll\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	got := HostGatedEnvKeyCandidates(ctx, h, "https://api.openai.com/v1", true)
	if len(got) != 4 || got[0] != "" || got[1] != "sk-oai" || got[2] != "" {
		t.Fatalf("openai host candidates %q", got)
	}
	got = HostGatedEnvKeyCandidates(ctx, h, "https://ollama.com/v1", true)
	if len(got) != 4 || got[0] != "sk-oll" || got[1] != "" || got[2] != "" {
		t.Fatalf("ollama host candidates %q", got)
	}
	got = HostGatedEnvKeyCandidates(ctx, h, "https://openrouter.ai.evil.example/v1", false)
	if len(got) != 3 || got[0] != "" || got[1] != "" {
		t.Fatalf("lookalike host received a gated key %q", got)
	}
	want := modelsResolver(ctxForHome(ctx, h)).hostGatedEnvKeyCandidates("https://openrouter.ai/api/v1", false)
	got = HostGatedEnvKeyCandidates(ctx, h, "https://openrouter.ai/api/v1", false)
	if strings.Join(got, ",") != strings.Join(want, ",") || got[1] != "sk-or" {
		t.Fatalf("openrouter candidates %q want %q", got, want)
	}
}

func TestNamedCustomProviderExportsResolveTheConfiguredEntry(t *testing.T) {
	h := runtimeExportsHome(t, "model:\n  default: m\ncustom_providers:\n  - name: Local Box\n    base_url: http://10.0.0.5:8000/v1\n    api_key: sk-box\n")
	ctx := context.Background()
	if !HasNamedCustomProvider(ctx, h, "local-box") || HasNamedCustomProvider(ctx, h, "nowhere") {
		t.Fatal("named custom presence mismatch")
	}
	entry, err := NamedCustomProvider(ctx, h, "local-box")
	if err != nil || entry == nil || entry["base_url"] != "http://10.0.0.5:8000/v1" || entry["api_key"] != "sk-box" {
		t.Fatalf("entry %v err %v", entry, err)
	}
	if entry, err := NamedCustomProvider(ctx, h, "nowhere"); err != nil || entry != nil {
		t.Fatalf("unknown entry %v err %v", entry, err)
	}
	bad := runtimeExportsHome(t, "model: [unclosed\n")
	if entry, err := NamedCustomProvider(ctx, bad, "local-box"); err != nil || entry != nil {
		t.Fatalf("malformed config runs on defaults, entry %v err %v", entry, err)
	}
	if HasNamedCustomProvider(ctx, bad, "local-box") {
		t.Fatal("malformed config reported a named provider")
	}
}

func TestResolveRequestedProviderExportPrefersArgThenConfigThenEnv(t *testing.T) {
	ctx := context.Background()
	h := runtimeExportsHome(t, "model:\n  default: m\n  provider: ' OpenRouter '\n")
	if got, err := ResolveRequestedProvider(ctx, h, "  Anthropic "); err != nil || got != "anthropic" {
		t.Fatalf("explicit %q %v", got, err)
	}
	if got, err := ResolveRequestedProvider(ctx, h, ""); err != nil || got != "openrouter" {
		t.Fatalf("config %q %v", got, err)
	}
	bare := runtimeExportsHome(t, "model:\n  default: m\n")
	if got, err := ResolveRequestedProvider(ctx, bare, ""); err != nil || got != "auto" {
		t.Fatalf("default %q %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(bare, ".env"), []byte("ICEBATH_INFERENCE_PROVIDER= Nous \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveRequestedProvider(ctx, bare, ""); err != nil || got != "nous" {
		t.Fatalf("env %q %v", got, err)
	}
	if got, err := ResolveRequestedProvider(ctx, runtimeExportsHome(t, "model: [unclosed\n"), ""); err != nil || got != "auto" {
		t.Fatalf("malformed config runs on defaults %q %v", got, err)
	}
}

func TestRegistryInferenceBaseURLsFollowRegistryOrder(t *testing.T) {
	if got, ok := RegistryInferenceBaseURL("openai-api"); !ok || got != "https://api.openai.com/v1" {
		t.Fatalf("openai-api %q %v", got, ok)
	}
	if got, ok := RegistryInferenceBaseURL("vertex"); !ok || got != "" {
		t.Fatalf("vertex %q %v", got, ok)
	}
	if got, ok := RegistryInferenceBaseURL("azure-foundry"); !ok || got != "" {
		t.Fatalf("azure-foundry %q %v", got, ok)
	}
	if _, ok := RegistryInferenceBaseURL("no-such-provider"); ok {
		t.Fatal("unknown id registered")
	}
	all := RegistryInferenceBaseURLs()
	if len(all) < 3 || all[0] != NousInferenceBaseURL || all[1] != CodexBaseURL || all[2] != "https://api.openai.com/v1" {
		t.Fatalf("registry order %q", all[:min(len(all), 3)])
	}
}

func TestResolveRequestedProviderReadsBoundClientEnvBeforeSecrets(t *testing.T) {
	bare := runtimeExportsHome(t, "model:\n  default: m\n")
	if err := os.WriteFile(filepath.Join(bare, ".env"), []byte("ICEBATH_INFERENCE_PROVIDER=nous\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bound := state.ClientEnvScope(context.Background(), map[string]string{"ICEBATH_INFERENCE_PROVIDER": " XAI "}, true, nil)
	if got, err := ResolveRequestedProvider(bound, bare, ""); err != nil || got != "xai" {
		t.Fatalf("bound client env %q %v", got, err)
	}
	empty := state.ClientEnvScope(context.Background(), map[string]string{}, true, nil)
	if got, err := ResolveRequestedProvider(empty, bare, ""); err != nil || got != "auto" {
		t.Fatalf("bound client without the key never reads the daemon secret %q %v", got, err)
	}
	unbound := state.ClientEnvScope(context.Background(), nil, true, nil)
	if got, err := ResolveRequestedProvider(unbound, bare, ""); err != nil || got != "nous" {
		t.Fatalf("unbound session reads the secret store %q %v", got, err)
	}
}
