package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestARaisingRefreshHookKeepsTheAPIErrorAndWalksTheFallbackChain(t *testing.T) {
	for _, broken := range []string{"is_stale_credential_error", "refresh_runtime_credentials"} {
		t.Run(broken, func(t *testing.T) {
			hostGone := func(hook string) {
				if hook == broken {
					panic(errors.New("plugin host closed the connection"))
				}
			}
			name := "acme-hosted-" + broken
			t.Cleanup(provider.SnapshotProviderRegistryForTests())
			provider.RegisterProvider(provider.Profile{Name: name,
				IsStaleCredentialErrorHook: func(int, string) bool {
					hostGone("is_stale_credential_error")
					return true
				},
				RefreshRuntimeCredentialsHook: func(context.Context, string) (string, string, bool, error) {
					hostGone("refresh_runtime_credentials")
					return "fresh-token", "", true, nil
				},
				CredentialRefreshNoticeHook: func(string) string {
					hostGone("credential_refresh_notice")
					return ""
				},
			})
			a := newFixtureAgent(t, AIAgentOptions{Provider: "custom", BaseURL: "https://api.acme-hosted.example/v1", Model: "m",
				FallbackModel: []map[string]any{{"provider": "custom", "model": "fallback-m", "base_url": "http://127.0.0.1:9/fallback/v1", "api_key": "fb-key"}}})
			a.provider, a.apiKey = name, "stale-token"
			saved := settleNonretryableResult
			t.Cleanup(func() { settleNonretryableResult = saved })
			settleNonretryableResult = func(context.Context, *AIAgent, *LoopState, *apiErrorTurn, string) (map[string]any, error) {
				return map[string]any{"failed": true}, nil
			}
			apiErr := &provider.APIError{Provider: name, Model: "m", StatusCode: 400, Message: "Unsupported parameter: 'max_tokens'",
				Body: `{"error": {"message": "Unsupported parameter: 'max_tokens'", "type": "invalid_request_error"}}`}
			cl := provider.ClassifyError(provider.ClassifyInput{Err: apiErr, Status: 400, Body: apiErr.Body, Provider: name, Model: "m"})
			status := 400
			s := &LoopState{Retry: &TurnRetryState{}, MaxRetries: 3, APICallCount: 1, Messages: []*state.Message{}}
			e := &apiErrorTurn{apiError: apiErr, classified: cl, statusCode: &status, errorContext: map[string]any{},
				errorMsg: apiErr.Error(), providerName: name, baseURL: a.baseURL, model: "m"}
			v, err := settleUnrecoveredError(context.Background(), a, s, e)
			if err != nil {
				t.Fatal(err)
			}
			if v.Action != "break" || !a.fallbackActivated || a.model != "fallback-m" {
				t.Fatalf("verdict %q activated=%v model=%q", v.Action, a.fallbackActivated, a.model)
			}
			if a.apiKey == "fresh-token" {
				t.Fatal("a raising hook adopted the refreshed key")
			}
			if !s.Retry.ProviderStaleRefreshAttempted && broken == "refresh_runtime_credentials" {
				t.Fatal("stale refresh not attempted")
			}
		})
	}
}

func TestTryRefreshProviderRuntimeCredentialsAdoptsTheMintedKeyAndBaseURL(t *testing.T) {
	name := "acme-runtime-refresh"
	t.Cleanup(provider.SnapshotProviderRegistryForTests())
	provider.RegisterProvider(provider.Profile{Name: name,
		RefreshRuntimeCredentialsHook: func(_ context.Context, reason string) (string, string, bool, error) {
			if reason != "unauthorized" {
				return "", "", false, nil
			}
			return "fresh", "https://api.acme-runtime.example/v2/", true, nil
		}})
	a := newFixtureAgent(t, AIAgentOptions{Provider: "custom", BaseURL: "https://api.acme-runtime.example/v1", Model: "m"})
	a.provider, a.apiKey = name, "stale-token"
	var rebuiltWith map[string]any
	saved := openAIClientConstructor
	t.Cleanup(func() { openAIClientConstructor = saved })
	openAIClientConstructor = func(kwargs map[string]any) (any, error) {
		rebuiltWith = kwargs
		return &struct{}{}, nil
	}
	if ok, err := a.tryRefreshProviderRuntimeCredentials(context.Background(), "stale"); err != nil || ok {
		t.Fatalf("a profile answering nothing refreshed: %v %v", ok, err)
	}
	ok, err := a.tryRefreshProviderRuntimeCredentials(context.Background(), "unauthorized")
	if err != nil || !ok {
		t.Fatalf("refresh %v %v", ok, err)
	}
	if a.apiKey != "fresh" || a.baseURL != "https://api.acme-runtime.example/v2" {
		t.Fatalf("adopted %q %q", a.apiKey, a.baseURL)
	}
	if rebuiltWith["api_key"] != "fresh" {
		t.Fatalf("client rebuilt with %v", rebuiltWith["api_key"])
	}
}
