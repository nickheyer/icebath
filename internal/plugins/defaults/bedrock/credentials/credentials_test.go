package credentials

import (
	"testing"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	bedrockconfig "github.com/nickheyer/icebath/internal/plugins/defaults/bedrock/config"
	"github.com/nickheyer/icebath/internal/provider"
)

func TestBearerTokenRoutesAsAnAPIKey(t *testing.T) {
	t.Setenv(bedrockconfig.BaseURLEnv, "")
	t.Setenv(bedrockconfig.APIKeyEnv, "bedrock-key")
	cfg := config.New(t.TempDir())
	bedrockconfig.Register(cfg)
	registry := new(credentials.Registry)
	Register(registry, cfg)

	credential, ok := registry.Credential(bedrockconfig.Name)
	if !ok || credential.APIKey != "bedrock-key" || credential.Auth != provider.AuthAPIKey || credential.Source != bedrockconfig.APIKeyEnv {
		t.Fatalf("Credential() = %+v, %v, want the bearer token as an api key from %s", credential, ok, bedrockconfig.APIKeyEnv)
	}
	profile := provider.Profile{Name: bedrockconfig.Name, BaseURL: cfg.GetString(bedrockconfig.BaseURL), Mode: provider.ModeBedrockConverse, Auth: provider.AuthAWSSigV4}
	route, err := registry.Route(profile, "us.anthropic.claude-sonnet-5")
	if err != nil || route.APIKey != "bedrock-key" || route.Auth != provider.AuthAPIKey || route.Mode != provider.ModeBedrockConverse || route.BaseURL != "https://bedrock-runtime.us-east-1.amazonaws.com" {
		t.Fatalf("Route() = %+v, %v, want a bearer authenticated converse route", route, err)
	}
	if _, ok := registry.Credential("vertex"); ok {
		t.Fatal("Credential() answered for another provider")
	}

	t.Setenv(bedrockconfig.APIKeyEnv, "")
	if _, ok := registry.Credential(bedrockconfig.Name); ok {
		t.Fatal("Credential() answered without a bearer token")
	}
	if _, err := registry.Route(profile, ""); err == nil {
		t.Fatal("Route() succeeded without a bearer token")
	}
}
