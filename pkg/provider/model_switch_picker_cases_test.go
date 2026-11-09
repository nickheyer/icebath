package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Unsigned RS256 shaped token expiring after the offset
func fakePickerJWT(expiryOffset time.Duration) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	payload, _ := json.Marshal(map[string]any{"exp": time.Now().Add(expiryOffset).Unix(), "sub": "test"})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".fakesig"
}

// Picker home with the real overlays, offline static catalogs and no provider key in the env
func pickerCredentialHome(t *testing.T, providers map[string]any) context.Context {
	t.Helper()
	overlays := icebathOverlays
	ctx, _ := pickerTestHome(t, nil)
	icebathOverlays = overlays
	cachedProviderModelIDsFn = func(ctx context.Context, slug string, _ CachedModelsOptions) []string {
		return StaticCatalog(ctx, slug)
	}
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "no_codex"))
	writeAuthStore(t, os.Getenv("ICEBATH_HOME"), map[string]any{"version": 2, "providers": providers})
	for _, name := range []string{"OPENROUTER_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
		"NOUS_API_KEY", "DEEPSEEK_API_KEY", "COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GEMINI_API_KEY"} {
		t.Setenv(name, "")
	}
	return ctx
}

func pickerSlugs(rows []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, r := range rows {
		out[r["slug"].(string)] = true
	}
	return out
}

func TestNormalPathStillWorks(t *testing.T) {
	ctx := pickerCredentialHome(t, map[string]any{"openai-codex": map[string]any{
		"tokens": map[string]any{"access_token": fakePickerJWT(time.Hour), "refresh_token": "fake-refresh"}, "last_refresh": "2026-04-12T00:00:00Z"}})
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "openai-codex", MaxModels: intPtr(10)})
	if !pickerSlugs(rows)["openai-codex"] {
		t.Fatalf("slugs %v", pickerSlugs(rows))
	}
}

func TestClaudeCodeFileDetectedByModelPicker(t *testing.T) {
	ctx := pickerCredentialHome(t, map[string]any{})
	claudeDir := filepath.Join(t.TempDir(), ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	creds, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{"accessToken": fakePickerJWT(time.Hour), "refreshToken": "fake-refresh",
		"expiresAt": time.Now().UnixMilli() + 3_600_000}})
	if err := os.WriteFile(filepath.Join(claudeDir, ".credentials.json"), creds, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "anthropic", MaxModels: intPtr(10)})
	anthropic := pickerRow(rows, "anthropic")
	if anthropic == nil {
		t.Fatalf("anthropic not found in /model picker providers: %v", pickerSlugs(rows))
	}
	if anthropic["is_current"] != true || anthropic["total_models"].(int) <= 0 {
		t.Fatalf("anthropic row %v", anthropic)
	}
}

func TestNoCodexWhenNoCredentials(t *testing.T) {
	ctx := pickerCredentialHome(t, map[string]any{})
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "openrouter", MaxModels: intPtr(10)})
	if pickerSlugs(rows)["openai-codex"] {
		t.Fatal("openai-codex should not appear without any credentials")
	}
}
