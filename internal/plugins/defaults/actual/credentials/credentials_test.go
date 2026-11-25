package credentials

import (
	"testing"

	"github.com/nickheyer/icebath/internal/config"
	actualconfig "github.com/nickheyer/icebath/internal/plugins/defaults/actual/config"
)

func TestCredentialServesLocalActualWithoutAKey(t *testing.T) {
	t.Setenv(actualconfig.APIKeyEnv, "")
	t.Setenv(actualconfig.BaseURLEnv, "")
	cfg := config.New(t.TempDir())
	actualconfig.Register(cfg)
	src := source{config: cfg}

	if credential, ok := src.Credential(actualconfig.Name); ok {
		t.Fatalf("hosted Actual without a key = %+v, want no credential", credential)
	}

	t.Setenv(actualconfig.BaseURLEnv, "http://localhost:8080/")
	credential, ok := src.Credential(actualconfig.Name)
	if !ok || credential.APIKey != placeholder || credential.Source != "local-offline" || credential.BaseURL != "http://localhost:8080/v1" {
		t.Fatalf("local Actual without a key = %+v, %v, want the no auth placeholder on the /v1 surface", credential, ok)
	}

	t.Setenv(actualconfig.APIKeyEnv, "act-key")
	credential, ok = src.Credential(actualconfig.Name)
	if !ok || credential.APIKey != "act-key" || credential.Source != actualconfig.APIKeyEnv || credential.BaseURL != "http://localhost:8080/v1" {
		t.Fatalf("local Actual with a key = %+v, %v, want the key from the environment", credential, ok)
	}

	t.Setenv(actualconfig.BaseURLEnv, "https://api.actual.inc")
	credential, ok = src.Credential(actualconfig.Name)
	if !ok || credential.APIKey != "act-key" || credential.BaseURL != "https://api.actual.inc/v1" {
		t.Fatalf("hosted Actual with a key = %+v, %v, want the hosted root on its /v1 surface", credential, ok)
	}

	if _, ok := src.Credential("other"); ok {
		t.Fatal("Credential() answered for another provider")
	}
}
