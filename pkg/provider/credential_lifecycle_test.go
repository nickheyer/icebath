package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// ---- tests/hermes_cli/test_credential_lifecycle.py (lifecycle half of the PUT and DELETE /api/env cases) ----

func credentialLifecycleHome(t *testing.T) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), "cred_home")
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	return h
}

func credentialLifecycleWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func credentialLifecycleRead(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestDeleteClearsProviderModelsCache(t *testing.T) {
	h := credentialLifecycleHome(t)
	key := "zk-" + strings.Repeat("a", 24)
	t.Setenv("ZAI_API_KEY", key)
	credentialLifecycleWrite(t, filepath.Join(h, ".env"), "ZAI_API_KEY="+key+"\n")
	raw, _ := json.Marshal(map[string]any{"credential_pool": map[string]any{"zai": []any{map[string]any{
		"id": "e1", "label": "env", "auth_type": "api_key", "priority": 0, "source": "env:ZAI_API_KEY", "access_token": key}}}})
	credentialLifecycleWrite(t, filepath.Join(h, "auth.json"), string(raw))
	cachePath := filepath.Join(h, "provider_models_cache.json")
	credentialLifecycleWrite(t, cachePath, `{"zai": {"models": ["glm-5"], "ts": 0}}`)
	result, err := RemoveProviderEnvCredential(context.Background(), h, "ZAI_API_KEY")
	if err != nil || !result.OK || !result.Found || !containsString(result.PoolPruned, "zai") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(cachePath); err == nil {
		var cache map[string]any
		_ = json.Unmarshal([]byte(credentialLifecycleRead(t, cachePath)), &cache)
		if _, ok := cache["zai"]; ok {
			t.Fatalf("cache kept zai: %v", cache)
		}
	}
	var auth map[string]any
	_ = json.Unmarshal([]byte(credentialLifecycleRead(t, filepath.Join(h, "auth.json"))), &auth)
	if pool, _ := auth["credential_pool"].(map[string]any); pool["zai"] != nil || os.Getenv("ZAI_API_KEY") != "" {
		t.Fatalf("auth store or process env kept the key: %v", auth)
	}
	if _, ok := config.LoadEnv(h)["ZAI_API_KEY"]; ok {
		t.Fatal(".env kept the key")
	}
}

func TestUpdateRotatesConfigYAMLModelMirror(t *testing.T) {
	h := credentialLifecycleHome(t)
	old, next := "sk-oe-"+strings.Repeat("f", 24), "sk-oe-"+strings.Repeat("g", 24)
	credentialLifecycleWrite(t, filepath.Join(h, ".env"), "OPENAI_API_KEY="+old+"\n")
	credentialLifecycleWrite(t, filepath.Join(h, "config.yaml"), "model:\n  provider: custom\n  default: my-model\n  base_url: https://llm.example.test/v1\n  api_key: "+old+"\n")
	result, err := SaveProviderEnvCredential(context.Background(), h, "OPENAI_API_KEY", next)
	if err != nil || !result.OK || result.Key != "OPENAI_API_KEY" || !containsString(result.ConfigUpdates, "model.api_key") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	cfg := credentialLifecycleRead(t, filepath.Join(h, "config.yaml"))
	if strings.Contains(cfg, old) || !strings.Contains(cfg, next) {
		t.Fatalf("config.yaml mirror not rotated:\n%s", cfg)
	}
	if config.LoadEnv(h)["OPENAI_API_KEY"] != next {
		t.Fatal(".env not updated")
	}
}

func TestPutAPIEnvMaterializesCredentialPoolEntry(t *testing.T) {
	h := credentialLifecycleHome(t)
	key := "ocg-" + strings.Repeat("e", 28)
	t.Setenv("OPENCODE_GO_API_KEY", "")
	credentialLifecycleWrite(t, filepath.Join(h, "auth.json"), `{"credential_pool": {}}`)
	result, err := SaveProviderEnvCredential(context.Background(), h, "OPENCODE_GO_API_KEY", key)
	if err != nil || !result.OK || result.Key != "OPENCODE_GO_API_KEY" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var auth map[string]any
	_ = json.Unmarshal([]byte(credentialLifecycleRead(t, filepath.Join(h, "auth.json"))), &auth)
	pool, _ := auth["credential_pool"].(map[string]any)
	entries, _ := pool["opencode-go"].([]any)
	onDisk := false
	for _, e := range entries {
		if row, ok := e.(map[string]any); ok && row["source"] == "env:OPENCODE_GO_API_KEY" {
			onDisk = true
		}
	}
	if !onDisk {
		t.Fatalf("credential_pool.opencode-go env reference missing on disk: %v", pool)
	}
	loaded := LoadPool(credentialLifecycleEnv(context.Background(), h), "opencode-go")
	var matched []PooledCredential
	for _, e := range loaded.Entries() {
		if e.AccessToken == key && e.Source == "env:OPENCODE_GO_API_KEY" {
			matched = append(matched, e)
		}
	}
	if len(matched) == 0 || matched[0].AuthType != "api_key" {
		t.Fatalf("load_pool did not surface the saved token: %+v", loaded.Entries())
	}
}

func TestUpdateRotatesKeyedProvidersMirror(t *testing.T) {
	h := credentialLifecycleHome(t)
	old, next := "sk-kp-"+strings.Repeat("n", 24), "sk-kp-"+strings.Repeat("o", 24)
	credentialLifecycleWrite(t, filepath.Join(h, ".env"), "OPENAI_API_KEY="+old+"\n")
	credentialLifecycleWrite(t, filepath.Join(h, "config.yaml"), "providers:\n  myendpoint:\n    base_url: https://llm.example.test/v1\n    api_key: "+old+"\n")
	result, err := SaveProviderEnvCredential(context.Background(), h, "OPENAI_API_KEY", next)
	if err != nil || !containsString(result.ConfigUpdates, "providers.myendpoint.api_key") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	cfg := credentialLifecycleRead(t, filepath.Join(h, "config.yaml"))
	if strings.Contains(cfg, old) || !strings.Contains(cfg, next) {
		t.Fatalf("keyed providers mirror not rotated:\n%s", cfg)
	}
}

func TestDeleteScrubsKeyedProvidersMirror(t *testing.T) {
	h := credentialLifecycleHome(t)
	old := "sk-kp-" + strings.Repeat("p", 24)
	credentialLifecycleWrite(t, filepath.Join(h, ".env"), "OPENAI_API_KEY="+old+"\n")
	credentialLifecycleWrite(t, filepath.Join(h, "config.yaml"), "providers:\n  myendpoint:\n    base_url: https://llm.example.test/v1\n    api_key: "+old+"\n")
	result, err := RemoveProviderEnvCredential(context.Background(), h, "OPENAI_API_KEY")
	if err != nil || !containsString(result.ConfigScrubbed, "providers.myendpoint.api_key") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if cfg := credentialLifecycleRead(t, filepath.Join(h, "config.yaml")); strings.Contains(cfg, old) {
		t.Fatalf("delete must clear the credential from every store:\n%s", cfg)
	}
}

func TestScrubNeverTouchesProvidersBaseURLAlias(t *testing.T) {
	h := credentialLifecycleHome(t)
	old := "https://llm.example.test/v1"
	credentialLifecycleWrite(t, filepath.Join(h, ".env"), "OPENAI_API_KEY="+old+"\n")
	credentialLifecycleWrite(t, filepath.Join(h, "config.yaml"), "providers:\n  myendpoint:\n    api: "+old+"\n    model: my-model\n")
	result, err := RemoveProviderEnvCredential(context.Background(), h, "OPENAI_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	if cfg := credentialLifecycleRead(t, filepath.Join(h, "config.yaml")); !strings.Contains(cfg, old) || containsString(result.ConfigScrubbed, "providers.myendpoint.api") {
		t.Fatalf("providers.<id>.api is a base_url and must survive:\n%s %v", cfg, result.ConfigScrubbed)
	}
}
