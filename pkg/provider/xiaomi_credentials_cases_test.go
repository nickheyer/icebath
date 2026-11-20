package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// A key a secret source injected belongs to the profile scope that loaded it
func TestResolveCredentialsReadsHomeExternalSecretScope(t *testing.T) {
	h := filepath.Join(t.TempDir(), "icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, ".env"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	config.InvalidateEnvCache()
	t.Setenv("XIAOMI_BASE_URL", "")
	os.Unsetenv("XIAOMI_BASE_URL")
	ResetSecretSourceCache("")
	t.Cleanup(func() { ResetSecretSourceCache("") })
	secretSourceCacheMu.Lock()
	secretValuesByHome[homeKey(h)] = map[string]string{"XIAOMI_API_KEY": "sk-bws-xiaomi-12345678"}
	secretSourceCacheMu.Unlock()
	SetMultiplexActive(true)
	t.Cleanup(func() { SetMultiplexActive(false) })
	ctx := WithSecretScope(context.Background(), BuildProfileSecretScope(h), h)
	creds, err := modelsResolver(ctx).apiKeyProviderCredentials("xiaomi")
	if err != nil {
		t.Fatal(err)
	}
	if creds.APIKey != "sk-bws-xiaomi-12345678" || creds.Source != "XIAOMI_API_KEY" {
		t.Fatalf("creds %+v", creds)
	}
}
