package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

const aliasCredentialsHost = "https://theta.example.com/v1"

func TestOneshotPassesAliasCredentialAliasApiKeyIsPassedToTheResolver(t *testing.T) {
	oneshotTestEnv(t)
	setDirectAliases(t)
	provider.DirectAliases.Clear()
	provider.DirectAliases.Set("theta", provider.DirectAlias{Model: "theta-1", Provider: "custom", BaseURL: aliasCredentialsHost, APIKey: "sk-theta-ALIAS"})
	captured := map[string]string{}
	swapSeam(t, &resolveRuntimeProviderFn, func(_ context.Context, _, _, _, baseURL, apiKey string) (*provider.Runtime, error) {
		captured["explicit_base_url"], captured["explicit_api_key"] = baseURL, apiKey
		return nil, errors.New("stop after credential resolution")
	})
	swapSeam(t, &oneshotLoadConfig, func(context.Context) (map[string]any, error) { return map[string]any{}, nil })
	ctx, env := oneshotToolEnv(context.Background())
	_, _, err := runOneshotAgentTurn(ctx, env, "hi", oneshotRunOptions{Model: "theta", UseConfigToolsets: true})
	if err == nil || !strings.Contains(err.Error(), "stop after credential resolution") {
		t.Fatalf("got %v", err)
	}
	if captured["explicit_base_url"] != aliasCredentialsHost || captured["explicit_api_key"] != "sk-theta-ALIAS" {
		t.Fatalf("captured %v", captured)
	}
}
