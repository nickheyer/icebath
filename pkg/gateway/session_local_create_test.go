package gateway

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Single profile owner whose launch home is the test home, with the given config.yaml and env file
func envRefsOwner(t *testing.T, configYAML, envFile string) (context.Context, *SessionAuthority, string) {
	t.Helper()
	ctx, a, _ := newRecoverAuthority(t)
	installPrefillLoaders(t)
	home := os.Getenv("ICEBATH_HOME")
	a.ProfileID = home
	envRefsWrite(t, home, "config.yaml", configYAML)
	envRefsWrite(t, home, ".env", envFile)
	return ctx, a, home
}

func envRefsWrite(t *testing.T, home, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, name), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func envRefsActor(a *SessionAuthority) Principal {
	return Principal{Subject: "owner", ProfileID: a.ProfileID, TransportID: "sock",
		Capabilities: map[string]bool{"session:create": true, "session:read": true, "session:control": true}}
}

// Frozen policy of a session created with the given params
func envRefsCreated(t *testing.T, ctx context.Context, a *SessionAuthority, params map[string]any) (SessionRef, *LocalSessionPolicy) {
	t.Helper()
	ref, err := createLocalSession(ctx, a, envRefsActor(a), params, localCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rawPolicy, _ := recoverReceipt(t, ctx, a.DB, ref.SessionID).Get("policy")
	policy, err := restorePolicy(rawPolicy)
	if err != nil {
		t.Fatal(err)
	}
	return ref, policy
}

func envRefsModel(t *testing.T, ctx context.Context, policy *LocalSessionPolicy) map[string]any {
	t.Helper()
	cfg, err := policy.config(ctx, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	model, _ := cfg["model"].(map[string]any)
	return model
}

func TestNewSessionExpandsConfigRefsAgainstCurrentProfileEnv(t *testing.T) {
	t.Setenv("CFG4_BASE_URL", "http://boot.invalid/v1")
	t.Setenv("CFG4_INJECTED", "chat_completions")
	ctx, a, home := envRefsOwner(t, "platform_toolsets: {cli: []}\n"+
		"model: {provider: custom, default: m, base_url: \"${CFG4_BASE_URL}\", api_mode: \"${CFG4_INJECTED}\"}\n",
		"CFG4_BASE_URL=http://rotated.invalid/v1\n")
	created := func(requestID string) map[string]any {
		_, policy := envRefsCreated(t, ctx, a, map[string]any{"request_id": requestID, "source": "cli", "cwd": home, "toolsets": []any{}})
		return envRefsModel(t, ctx, policy)
	}
	first := created("first")
	if first["base_url"] != "http://rotated.invalid/v1" || first["api_mode"] != "chat_completions" {
		t.Fatalf("first model %v", first)
	}
	envRefsWrite(t, home, ".env", "CFG4_BASE_URL=http://rotated-again.invalid/v1\n")
	if second := created("second"); second["base_url"] != "http://rotated-again.invalid/v1" {
		t.Fatalf("second model %v", second)
	}
	if provider.SecretScopeBound(ctx) {
		t.Fatal("the read's scope outlived the build")
	}
}

func TestExactCreateRetryAfterAnEnvRotationReturnsTheOriginalSession(t *testing.T) {
	for name, model := range map[string]string{"default_model": "", "alias_env": "alias-env"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CFG4_BASE_URL", "http://boot.invalid/v1")
			ctx, a, home := envRefsOwner(t, "platform_toolsets: {cli: []}\n"+
				"model: {provider: custom, default: m, base_url: \"${CFG4_BASE_URL}\"}\n"+
				"model_aliases:\n"+
				"  alias-env: {model: model-alias, provider: custom, base_url: \"${CFG4_BASE_URL}\"}\n",
				"CFG4_BASE_URL=http://boot.invalid/v1\n")
			params := func() map[string]any {
				out := map[string]any{"request_id": "retry", "source": "cli", "cwd": home, "toolsets": []any{}}
				if model != "" {
					out["model"] = model
				}
				return out
			}
			ref, policy := envRefsCreated(t, ctx, a, params())
			saved, _ := recoverReceipt(t, ctx, a.DB, ref.SessionID).Get("policy")
			if base := envRefsModel(t, ctx, policy)["base_url"]; base != "http://boot.invalid/v1" {
				t.Fatalf("frozen base url %v", base)
			}
			envRefsWrite(t, home, ".env", "CFG4_BASE_URL=http://rotated.invalid/v1\n")
			again, err := createLocalSession(ctx, a, envRefsActor(a), params(), localCreateOptions{})
			if err != nil || again != ref {
				t.Fatalf("exact retry answered %v %v", again, err)
			}
			after, _ := recoverReceipt(t, ctx, a.DB, ref.SessionID).Get("policy")
			if !reflect.DeepEqual(recoverPlain(t, after), recoverPlain(t, saved)) {
				t.Fatalf("the frozen policy changed to %v", recoverPlain(t, after))
			}
		})
	}
}

func TestNewAliasSessionUsesTheCurrentProfileEnv(t *testing.T) {
	t.Setenv("CFG4_BASE_URL", "http://boot.invalid/v1")
	ctx, a, home := envRefsOwner(t, "platform_toolsets: {cli: []}\n"+
		"model: {provider: custom, default: m}\n"+
		"model_aliases:\n"+
		"  alias-env: {model: model-alias, provider: custom, base_url: \"${CFG4_BASE_URL}\"}\n",
		"CFG4_BASE_URL=http://rotated.invalid/v1\n")
	created := func(requestID string) [2]any {
		_, policy := envRefsCreated(t, ctx, a, map[string]any{
			"request_id": requestID, "source": "cli", "cwd": home, "toolsets": []any{}, "model": "alias-env"})
		return [2]any{optValue(policy.Model), envRefsModel(t, ctx, policy)["base_url"]}
	}
	if got := created("first"); got != [2]any{"model-alias", "http://rotated.invalid/v1"} {
		t.Fatalf("first alias session %v", got)
	}
	envRefsWrite(t, home, ".env", "CFG4_BASE_URL=http://rotated-again.invalid/v1\n")
	if got := created("second"); got != [2]any{"model-alias", "http://rotated-again.invalid/v1"} {
		t.Fatalf("second alias session %v", got)
	}
}

func TestBoundScopeIsKept(t *testing.T) {
	home := t.TempDir()
	envRefsWrite(t, home, "config.yaml", "model: {base_url: \"${CFG4_BASE_URL}\"}\n")
	envRefsWrite(t, home, ".env", "CFG4_BASE_URL=http://file.invalid/v1\n")
	ctx := provider.WithSecretScope(context.Background(), map[string]string{"CFG4_BASE_URL": "http://scope.invalid/v1"}, home)
	model, _ := loadProfileConfig(ctx, home)["model"].(map[string]any)
	if model["base_url"] != "http://scope.invalid/v1" {
		t.Fatalf("model %v", model)
	}
}
