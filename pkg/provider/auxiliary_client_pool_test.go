package provider

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestAnthropicSuffixHostCheckDirect(t *testing.T) {
	for url, want := range map[string]bool{
		"https://api.anthropic.com":               true,
		"https://gateway.example.com/anthropic":   true,
		"http://127.0.0.1:8080/anthropic/v1":      true,
		"https://openrouter.ai/api/v1":            false,
		"https://api.openai.com/v1":               false,
		"":                                        false,
		"https://API.Anthropic.com./v1":           true,
		"https://gateway.example.com/Anthropic/":  true,
		"https://gateway.example.com/anthropicv1": false,
	} {
		if got := auxIsAnthropicCompatibleHost(url); got != want {
			t.Fatalf("%q got %v", url, got)
		}
	}
}

func TestCodexStallIsLadderTimeoutMessage(t *testing.T) {
	guard := newAuxCodexStreamGuard(context.Background(), nil, 300.0, true, 0)
	guard.sawContent.Store(true)
	msg := guard.timeoutMessage()
	if !strings.Contains(msg, CodexStreamStallMarker) || !strings.HasPrefix(msg, "Codex auxiliary Responses stream stalled: no new output for 60.0s (") {
		t.Fatalf("stall message %q", msg)
	}
	fresh := newAuxCodexStreamGuard(context.Background(), nil, 300.0, true, 0)
	if msg := fresh.timeoutMessage(); !strings.HasPrefix(msg, "Codex auxiliary Responses stream produced no output within 60.0s (no-progress timeout, ") {
		t.Fatalf("first token message %q", msg)
	}
}

func multiplexSecondaryScope(t *testing.T) context.Context {
	t.Helper()
	for _, name := range []string{"OPENAI_BASE_URL", "XAI_BASE_URL", "ICEBATH_XAI_BASE_URL", "NOUS_INFERENCE_BASE_URL",
		"GATEWAY_PROXY_URL", "FIRECRAWL_API_URL", "BROWSERBASE_BASE_URL", "XAI_API_KEY"} {
		t.Setenv(name, "https://"+strings.ToLower(name)+".default.example/v1")
	}
	SetMultiplexActive(true)
	t.Cleanup(func() { SetMultiplexActive(false) })
	return WithSecretScope(context.Background(), map[string]string{"BROWSERBASE_API_KEY": "bb-b", "BROWSERBASE_PROJECT_ID": "pid-b"}, "")
}

func TestBaseURLsFollowTheScopedKeyNotDefaultEnviron(t *testing.T) {
	h := newTestHome(t)
	ctx := multiplexSecondaryScope(t)
	r := newResolver(ctx, config.Tree{}, config.NewSecrets(h), h, &fakeCreds{})
	if _, base := r.expandDirectAPIAlias("openai", ""); strings.Contains(base, "default.example") {
		t.Fatalf("direct alias base %q", base)
	}
	if v, err := auxScopedKeyEnv(ctx, "OPENAI_BASE_URL"); v != "" || err != nil {
		t.Fatalf("scoped base url %q %v", v, err)
	}
	if v := nousInferenceEnvOverrideIn(modelsPoolEnv(ctx)); v != "" {
		t.Fatalf("nous override %q", v)
	}
}

func TestUnscopedSingleProfileReadsKeepEnviron(t *testing.T) {
	newTestHome(t)
	SetMultiplexActive(false)
	ctx := WithSecretScope(context.Background(), nil, "")
	t.Setenv("NOUS_INFERENCE_BASE_URL", "https://nous.mine.example/v1/")
	if v := nousInferenceEnvOverrideIn(modelsPoolEnv(ctx)); v != "https://nous.mine.example/v1" {
		t.Fatalf("nous override %q", v)
	}
}

func TestAuxiliaryScopedKeyEnv(t *testing.T) {
	multiplex := func(t *testing.T) {
		SetMultiplexActive(true)
		t.Cleanup(func() { SetMultiplexActive(false) })
	}
	t.Run("scoped value wins", func(t *testing.T) {
		t.Setenv("OPENROUTER_API_KEY", "sk-env")
		multiplex(t)
		ctx := WithSecretScope(context.Background(), map[string]string{"OPENROUTER_API_KEY": "sk-scoped"}, "")
		if v, err := auxScopedKeyEnv(ctx, "OPENROUTER_API_KEY"); v != "sk-scoped" || err != nil {
			t.Fatalf("got %q %v", v, err)
		}
	})
	t.Run("scoped miss no borrow", func(t *testing.T) {
		t.Setenv("OPENAI_API_KEY", "sk-other-profile")
		multiplex(t)
		ctx := WithSecretScope(context.Background(), map[string]string{"UNRELATED": "x"}, "")
		if v, err := auxScopedKeyEnv(ctx, "OPENAI_API_KEY"); v != "" || err != nil {
			t.Fatalf("got %q %v", v, err)
		}
	})
	t.Run("unscoped multiplex falls back", func(t *testing.T) {
		t.Setenv("OPENAI_API_KEY", "sk-own-env")
		multiplex(t)
		if v, err := auxScopedKeyEnv(context.Background(), "OPENAI_API_KEY"); v != "sk-own-env" || err != nil {
			t.Fatalf("got %q %v", v, err)
		}
	})
	t.Run("empty name returns empty", func(t *testing.T) {
		if v, err := auxScopedKeyEnv(context.Background(), ""); v != "" || err != nil {
			t.Fatalf("got %q %v", v, err)
		}
	})
	t.Run("scope failure never borrows env", func(t *testing.T) {
		t.Setenv("OPENAI_API_KEY", "sk-other-profile")
		multiplex(t)
		boom := errors.New("resolver boom")
		saved := auxGetSecretFn
		auxGetSecretFn = func(ctx context.Context, name string) (string, bool, error) {
			if SecretScopeFrom(ctx).Mapping != nil {
				return "", false, boom
			}
			return GetSecret(ctx, name)
		}
		t.Cleanup(func() { auxGetSecretFn = saved })
		ctx := WithSecretScope(context.Background(), map[string]string{}, "")
		if v, err := auxScopedKeyEnv(ctx, "OPENAI_API_KEY"); !errors.Is(err, boom) || v != "" {
			t.Fatalf("got %q %v", v, err)
		}
	})
}
