package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Exception whose Python class name is the given transport error name
type transportTestErr struct{ name string }

func (e *transportTestErr) Error() string      { return "connection timed out" }
func (e *transportTestErr) PyTypeName() string { return e.name }

// Fixture agent on a custom endpoint whose rebuilds and waits are doubled
func newTransportAgent(t *testing.T, overrides map[string]any) *AIAgent {
	t.Helper()
	a := newFixtureAgent(t, AIAgentOptions{Provider: "custom", BaseURL: "https://my-llm.example.com/v1", APIKey: "test-key-12345678", RequestOverrides: overrides})
	savedCtor, savedSleep, savedAnthropic := openAIClientConstructor, recoverySleep, anthropicClientBuilder
	t.Cleanup(func() {
		openAIClientConstructor, recoverySleep, anthropicClientBuilder = savedCtor, savedSleep, savedAnthropic
	})
	recoverySleep = func(time.Duration) {}
	return a
}

func TestTryRecoverPrimaryTransportRecoveryRestoresRequestOverrides(t *testing.T) {
	original := map[string]any{"extra_body": map[string]any{"reasoning": map[string]any{"effort": "medium"}}}
	a := newTransportAgent(t, original)
	a.requestOverrides = map[string]any{"extra_body": map[string]any{"fallback_only": true}}
	rebuilt := &struct{ name string }{"rebuilt"}
	openAIClientConstructor = func(map[string]any) (any, error) { return rebuilt, nil }
	if !a.tryRecoverPrimaryTransport(context.Background(), &transportTestErr{"ReadTimeout"}, 3) {
		t.Fatal("recovery declined")
	}
	if !reflect.DeepEqual(a.requestOverrides, original) {
		t.Fatalf("overrides %v", a.requestOverrides)
	}
	snapshot, _ := a.primaryRuntime["request_overrides"].(map[string]any)
	a.requestOverrides["probe"] = true
	if _, shared := snapshot["probe"]; shared {
		t.Fatal("overrides alias the snapshot")
	}
	if a.client != rebuilt {
		t.Fatalf("client %v", a.client)
	}
}

func TestTryRecoverPrimaryTransportSkippedWhenAlreadyOnFallback(t *testing.T) {
	a := newTransportAgent(t, nil)
	a.fallbackActivated = true
	if a.tryRecoverPrimaryTransport(context.Background(), &transportTestErr{"ReadTimeout"}, 3) {
		t.Fatal("recovered while on fallback")
	}
}

func TestTryRecoverPrimaryTransportAllowedForNousAnthropicMessages(t *testing.T) {
	a := newTransportAgent(t, nil)
	a.provider, a.apiMode, a.model = "nous", "anthropic_messages", "anthropic/claude-opus-4.8"
	a.setBaseURL("https://inference-api.nousresearch.com/v1")
	for k, v := range map[string]any{"api_mode": "anthropic_messages", "model": "anthropic/claude-opus-4.8", "provider": "nous",
		"base_url": "https://inference-api.nousresearch.com/v1", "anthropic_api_key": "portal-jwt",
		"anthropic_base_url": "https://inference-api.nousresearch.com/v1", "is_anthropic_oauth": false} {
		a.primaryRuntime[k] = v
	}
	rebuilt := &struct{ name string }{"anthropic-client"}
	var gotKey any
	anthropicClientBuilder = func(_ context.Context, key any, _ string, _ float64, _ bool) (any, error) {
		gotKey = key
		return rebuilt, nil
	}
	if !a.tryRecoverPrimaryTransport(context.Background(), &transportTestErr{"ReadTimeout"}, 3) {
		t.Fatal("portal Claude recovery declined")
	}
	if a.anthropicClient != rebuilt || gotKey != "portal-jwt" || a.client != nil {
		t.Fatalf("anthropic client %v key %v client %v", a.anthropicClient, gotKey, a.client)
	}
	a.apiMode = "chat_completions"
	a.primaryRuntime["api_mode"] = "chat_completions"
	if a.tryRecoverPrimaryTransport(context.Background(), &transportTestErr{"ReadTimeout"}, 3) {
		t.Fatal("portal OpenAI wire rides aggregator retries")
	}
}

func TestTryRecoverPrimaryTransportSurvivesRebuildFailure(t *testing.T) {
	a := newTransportAgent(t, nil)
	openAIClientConstructor = func(map[string]any) (any, error) { return nil, errors.New("socket error") }
	if a.tryRecoverPrimaryTransport(context.Background(), &transportTestErr{"ReadTimeout"}, 3) {
		t.Fatal("recovered through a failed rebuild")
	}
}

func TestApiErrorClassNameNamesStatuslessProviderTransportErrors(t *testing.T) {
	cases := map[string]error{
		"APITimeoutError":    &provider.APIError{Kind: provider.ErrTimeout},
		"APIConnectionError": &provider.APIError{Kind: provider.ErrNetwork},
		"ReadTimeout":        &transportTestErr{"ReadTimeout"},
	}
	for want, err := range cases {
		if got := apiErrorClassName(err); got != want || !transientTransportErrors[got] {
			t.Fatalf("%v: got %q", err, got)
		}
	}
	if got := apiErrorClassName(&provider.APIError{StatusCode: 503, Kind: provider.ErrTimeout}); transientTransportErrors[got] {
		t.Fatalf("status error named %q", got)
	}
}
