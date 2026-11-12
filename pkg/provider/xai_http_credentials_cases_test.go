package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Multiplexing on for the case, the previous state restored afterwards
func multiplexOn(t *testing.T) {
	t.Helper()
	previous := tools.MultiplexActive()
	tools.SetMultiplexActive(true)
	t.Cleanup(func() { tools.SetMultiplexActive(previous) })
}

// Tool env bound to a profile scope holding exactly the given secrets
func scopedToolEnv(t *testing.T, secrets map[string]string) *tools.Env {
	t.Helper()
	profile := t.TempDir()
	body := ""
	for k, v := range secrets {
		body += k + "=" + v + "\n"
	}
	if err := os.WriteFile(filepath.Join(profile, ".env"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return &tools.Env{Home: profile, Secrets: config.NewSecrets(profile)}
}

func TestXaiCredentialsDoNotFallBackToEnvironWhenScopeHasNoKey(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("XAI_API_KEY", "foreign-xai-key")
	t.Setenv("XAI_BASE_URL", "https://foreign.example/v1")
	env := scopedToolEnv(t, nil)
	multiplexOn(t)
	provider, key, base := tools.ResolveXAIHTTPCredentialsRefreshed(env, "")
	if provider != "xai" || key != "" || base != "https://api.x.ai/v1" {
		t.Fatalf("credentials %q %q %q", provider, key, base)
	}
}

func TestPreferAPIKeyHonorsProfileScopeOnlyKey(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, name := range []string{"XAI_API_KEY", "XAI_BASE_URL", "ICEBATH_XAI_BASE_URL"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	env := scopedToolEnv(t, map[string]string{"XAI_API_KEY": "scoped-key-x1"})
	multiplexOn(t)
	provider, key, base := tools.ResolveXAIHTTPCredentials(env, true)
	if provider != "xai" || key != "scoped-key-x1" || base != "https://api.x.ai/v1" {
		t.Fatalf("credentials %q %q %q", provider, key, base)
	}
}

func TestSuppressedEnvKeyDoesNotOverrideOauth(t *testing.T) {
	for _, tc := range []struct {
		suppressed map[string]any
		want       string
	}{
		{map[string]any{"xai": []any{"env:XAI_API_KEY"}}, "xai-oauth"},
		{map[string]any{}, "xai"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			h := t.TempDir()
			t.Setenv("ICEBATH_HOME", h)
			store, err := json.Marshal(map[string]any{
				"version": 1,
				"providers": map[string]any{"xai-oauth": map[string]any{"tokens": map[string]any{
					"access_token": "oauth-live-token", "refresh_token": "r",
					"expires_at": time.Now().UnixMilli() + 3_600_000}}},
				"credential_pool":    map[string]any{},
				"suppressed_sources": tc.suppressed,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(h, "auth.json"), store, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("XAI_API_KEY", "dead-creditless-team-key")
			provider, key, _ := tools.ResolveXAIHTTPCredentials(nil, true)
			if provider != tc.want {
				t.Fatalf("provider %q want %q", provider, tc.want)
			}
			if (key == "dead-creditless-team-key") != (tc.want == "xai") {
				t.Fatalf("key %q for provider %q", key, provider)
			}
		})
	}
}
