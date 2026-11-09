package provider

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
)

// Home directory holding a config file, the process environment left untouched
func runtimeConfigHome(t *testing.T, configYAML string) string {
	t.Helper()
	h := t.TempDir()
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestResolveRuntimeProviderExportHonorsExplicitEndpoint(t *testing.T) {
	h := runtimeConfigHome(t, "model:\n  default: m\n  provider: custom\n")
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

func TestHostGatedEnvKeyCandidatesExportGatesKeysByHost(t *testing.T) {
	h := runtimeConfigHome(t, "model:\n  default: m\n")
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

func TestResolveRequestedProviderExportPrefersArgThenConfigThenEnv(t *testing.T) {
	ctx := context.Background()
	h := runtimeConfigHome(t, "model:\n  default: m\n  provider: ' OpenRouter '\n")
	if got, err := ResolveRequestedProvider(ctx, h, "  Anthropic "); err != nil || got != "anthropic" {
		t.Fatalf("explicit %q %v", got, err)
	}
	if got, err := ResolveRequestedProvider(ctx, h, ""); err != nil || got != "openrouter" {
		t.Fatalf("config %q %v", got, err)
	}
	bare := runtimeConfigHome(t, "model:\n  default: m\n")
	if got, err := ResolveRequestedProvider(ctx, bare, ""); err != nil || got != "auto" {
		t.Fatalf("default %q %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(bare, ".env"), []byte("ICEBATH_INFERENCE_PROVIDER= Nous \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveRequestedProvider(ctx, bare, ""); err != nil || got != "nous" {
		t.Fatalf("env %q %v", got, err)
	}
	if got, err := ResolveRequestedProvider(ctx, runtimeConfigHome(t, "model: [unclosed\n"), ""); err != nil || got != "auto" {
		t.Fatalf("malformed config runs on defaults %q %v", got, err)
	}
}

func TestResolveRequestedProviderReadsBoundClientEnvBeforeSecrets(t *testing.T) {
	bare := runtimeConfigHome(t, "model:\n  default: m\n")
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

func TestProfileModelAPIModeAnswersTheHookAndWarnsOncePerProvider(t *testing.T) {
	ctx := context.Background()
	if mode, ok := ProfileModelAPIMode(ctx, Profile{Name: "plain"}, "m"); ok || mode != "" {
		t.Fatal("a profile without a per model hook answers None")
	}
	codex := Profile{Name: "hooked", APIModeForModelHook: func(model, _ string) APIMode {
		if strings.HasPrefix(model, "gpt-5") {
			return ModeCodexResponses
		}
		return ""
	}}
	if mode, ok := ProfileModelAPIMode(ctx, codex, "gpt-5.4"); !ok || mode != "codex_responses" {
		t.Fatal(mode, ok)
	}
	if _, ok := ProfileModelAPIMode(ctx, codex, "llama"); ok {
		t.Fatal("a hook answering None is no per model wire")
	}
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	apiModeWarnedMu.Lock()
	delete(apiModeWarned, "raising-hook")
	apiModeWarnedMu.Unlock()
	raising := Profile{Name: "raising-hook", Mode: ModeAnthropicMessages, APIModeForModelHook: func(string, string) APIMode { panic("catalog down") }}
	for range 2 {
		if mode, ok := ProfileModelAPIMode(ctx, raising, "m"); !ok || mode != "anthropic_messages" {
			t.Fatal(mode, ok)
		}
	}
	undeclared := Profile{Name: "raising-hook", APIModeForModelHook: raising.APIModeForModelHook}
	if mode, _ := ProfileModelAPIMode(ctx, undeclared, "m"); mode != "chat_completions" {
		t.Fatal(mode)
	}
	if n := strings.Count(buf.String(), "provider raising-hook: api_mode_for_model failed; using its declared api_mode"); n != 1 {
		t.Fatalf("warned %d times\n%s", n, buf.String())
	}
}

func TestGetAnthropicKeyPrefersTheHomeDotenvInRegistryOrder(t *testing.T) {
	dir := t.TempDir()
	ctx := home.WithOverride(context.Background(), dir)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_TOKEN", "sk-ant-shell-token")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	if got := GetAnthropicKey(ctx); got != "sk-ant-shell-token" {
		t.Fatalf("got %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("ANTHROPIC_TOKEN=sk-ant-dotenv-token\nCLAUDE_CODE_OAUTH_TOKEN=oauth\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := GetAnthropicKey(ctx); got != "sk-ant-dotenv-token" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("ANTHROPIC_TOKEN", "")
	if err := os.WriteFile(filepath.Join(dir, ".env"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := GetAnthropicKey(ctx); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestDetectAPIModeForURLWrapperMatches(t *testing.T) {
	for _, u := range []string{"https://api.minimax.io/anthropic", "https://openrouter.ai/api/v1", ""} {
		if got, want := DetectAPIModeForURL(u), detectAPIModeForURL(u); got != want {
			t.Fatalf("DetectAPIModeForURL(%q) = %q, want %q", u, got, want)
		}
	}
}

func TestAutoDetectLocalModelReadsTheSingleLoadedModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"data":[{"id":"local-llama"}]}`))
	}))
	defer srv.Close()
	if got := AutoDetectLocalModel(context.Background(), srv.URL); got != "local-llama" {
		t.Fatalf("got %q", got)
	}
	if got := AutoDetectLocalModel(context.Background(), ""); got != "" {
		t.Fatalf("empty base url gave %q", got)
	}
}

func TestLooksLikeOpenRouterKeyMatchesThePrefix(t *testing.T) {
	if !LooksLikeOpenRouterKey("sk-or-v1-abc") || LooksLikeOpenRouterKey("sk-proj-abc") || LooksLikeOpenRouterKey("") {
		t.Fatal("prefix check wrong")
	}
}
