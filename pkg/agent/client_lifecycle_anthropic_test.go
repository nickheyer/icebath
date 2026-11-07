package agent

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

type fakeAnthropicClient struct{ closed int }

func (c *fakeAnthropicClient) Close() error { c.closed++; return nil }

func installAnthropicRefresh(t *testing.T, token string) *[]string {
	t.Helper()
	built := &[]string{}
	prevBuild, prevResolve := anthropicClientBuilder, resolveAnthropicToken
	anthropicClientBuilder = func(_ context.Context, apiKey any, _ string, _ float64, _ bool) (any, error) {
		*built = append(*built, apiKey.(string))
		return &fakeAnthropicClient{}, nil
	}
	resolveAnthropicToken = func(provider.PoolEnv, string) string { return token }
	t.Cleanup(func() { anthropicClientBuilder, resolveAnthropicToken = prevBuild, prevResolve })
	return built
}

func anthropicRefreshAgent(providerName, key, baseURL string, oauth bool) *AIAgent {
	return &AIAgent{apiMode: "anthropic_messages", provider: providerName, apiKey: "test-key-1234567890", baseURL: "https://openrouter.ai/api/v1",
		anthropicAPIKey: key, anthropicBaseURL: baseURL, anthropicClient: &fakeAnthropicClient{}, isAnthropicOAuth: oauth}
}

func TestThirdPartyProviderRefreshIsNoop(t *testing.T) {
	built := installAnthropicRefresh(t, "sk-ant-oauth-example-1234567890abcdef")
	a := anthropicRefreshAgent("minimax", "***", "", false)
	ok, err := a.tryRefreshAnthropicClientCredentials(context.Background())
	if ok || err != nil || a.isAnthropicOAuth || len(*built) != 0 {
		t.Fatalf("refreshed=%v err=%v oauth=%v built=%v", ok, err, a.isAnthropicOAuth, *built)
	}
}

func TestThirdPartyEndpointSkipsRefresh(t *testing.T) {
	for _, baseURL := range []string{"https://llmbox.bytedance.net", "http://127.0.0.1:8080/anthropic.com", "https://llmbox.bytedance.net/anthropic"} {
		built := installAnthropicRefresh(t, "sk-ant-oauth-example-1234567890abcdef")
		a := anthropicRefreshAgent("anthropic", "custom-api-key", baseURL, false)
		ok, err := a.tryRefreshAnthropicClientCredentials(context.Background())
		if ok || err != nil || a.anthropicAPIKey != "custom-api-key" || a.isAnthropicOAuth || len(*built) != 0 {
			t.Fatalf("%s refreshed=%v err=%v key=%q oauth=%v", baseURL, ok, err, a.anthropicAPIKey, a.isAnthropicOAuth)
		}
	}
}

func TestAcceptedNativeProxyKeepsRotatingAnthropicToken(t *testing.T) {
	oldToken, newToken := "sk-ant-oat01-old-token-aaaaaaaa", "sk-ant-oat01-new-token-bbbbbbbb"
	for _, baseURL := range []string{"https://api.claude.com", "https://llm.corp.example/anthropic"} {
		installAnthropicRefresh(t, newToken)
		a := anthropicRefreshAgent("anthropic", oldToken, baseURL, true)
		previous := a.anthropicClient.(*fakeAnthropicClient)
		a.primaryRuntime = map[string]any{"anthropic_api_key": oldToken, "is_anthropic_oauth": false}
		ok, err := a.tryRefreshAnthropicClientCredentials(context.Background())
		if !ok || err != nil || a.anthropicAPIKey != newToken || previous.closed != 1 || a.anthropicClient == any(previous) {
			t.Fatalf("%s refreshed=%v err=%v key=%q closed=%d", baseURL, ok, err, a.anthropicAPIKey, previous.closed)
		}
		if want := map[string]any{"anthropic_api_key": newToken, "is_anthropic_oauth": a.isAnthropicOAuth}; !reflect.DeepEqual(a.primaryRuntime, want) {
			t.Fatalf("%s primary runtime %v", baseURL, a.primaryRuntime)
		}
	}
}

type keyedScriptedEngine struct {
	scriptedEngine
	apiKey string
}

func (e *keyedScriptedEngine) APIKey() string       { return e.apiKey }
func (e *keyedScriptedEngine) SetAPIKey(key string) { e.apiKey = key }

func TestCompressionBeforeAnyRequestSendsTheRefreshedToken(t *testing.T) {
	oldToken, newToken := "sk-ant-oat01-old", "sk-ant-oat01-new"
	installAnthropicRefresh(t, newToken)
	a, _, _ := newCompressContextAgent(t, nil)
	var seen, announced []string
	engine := &keyedScriptedEngine{apiKey: oldToken}
	engine.stubEngine = *newStubEngine(200000, 0.5)
	engine.compress = func([]map[string]any) []map[string]any {
		seen = append(seen, engine.apiKey)
		return []map[string]any{{"role": "user", "content": "[summary]"}, {"role": "assistant", "content": "tail"}}
	}
	a.contextCompressor = engine
	a.apiMode, a.provider, a.anthropicBaseURL, a.anthropicClient = "anthropic_messages", "anthropic", "https://api.anthropic.com", &fakeAnthropicClient{}
	a.apiKey, a.anthropicAPIKey = oldToken, oldToken
	a.primaryRuntime = map[string]any{"api_key": oldToken, "anthropic_api_key": oldToken, "compressor_api_key": oldToken}
	a.statusCallback = func(string, string) { announced = append(announced, a.anthropicAPIKey) }
	system := "system"
	approx := 100_000
	if _, _, err := compressContext(context.Background(), a, []map[string]any{{"role": "user", "content": "q"}, {"role": "assistant", "content": "a"}}, &system, compressContextOptions{ApproxTokens: &approx, Force: true}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seen, []string{newToken}) || len(announced) == 0 || announced[0] != oldToken {
		t.Fatalf("seen %v announced %v", seen, announced)
	}
	if a.anthropicAPIKey != newToken || a.apiKey != newToken {
		t.Fatalf("keys %q %q", a.anthropicAPIKey, a.apiKey)
	}
	want := map[string]any{"api_key": newToken, "anthropic_api_key": newToken, "compressor_api_key": newToken, "is_anthropic_oauth": a.isAnthropicOAuth}
	if !reflect.DeepEqual(a.primaryRuntime, want) {
		t.Fatalf("primary runtime %v", a.primaryRuntime)
	}
}

func TestAuxiliaryMainRouteUsesRefreshedToken(t *testing.T) {
	oldToken, newToken := "sk-ant...aaaa", "sk-ant...bbbb"
	installAnthropicRefresh(t, newToken)
	a := anthropicRefreshAgent("anthropic", oldToken, "https://api.anthropic.com", true)
	a.model, a.baseURL, a.apiKey = "claude-opus-4-6", "https://api.anthropic.com", oldToken
	ctx := publishRuntimeMain(context.Background(), a)
	refreshed := make(chan bool, 1)
	go func() {
		ok, _ := a.tryRefreshAnthropicClientCredentials(ctx)
		refreshed <- ok
	}()
	if !<-refreshed {
		t.Fatal("refresh did not rotate")
	}
	if _, runtime := provider.ScopedRuntimeMain(ctx, nil); runtime["api_key"] != newToken {
		t.Fatalf("published runtime %v", runtime)
	}
	scoped, _ := provider.ScopedRuntimeMain(ctx, map[string]any{"provider": "anthropic", "api_key": newToken, "model": "m"})
	provider.RotateRuntimeMainAPIKey(scoped, newToken, "sk-ant...cccc")
	if _, runtime := provider.ScopedRuntimeMain(scoped, nil); runtime["api_key"] != "sk-ant...cccc" {
		t.Fatalf("scoped runtime %v", runtime)
	}
	if _, runtime := provider.ScopedRuntimeMain(ctx, nil); runtime["api_key"] != newToken {
		t.Fatalf("outer runtime moved %v", runtime)
	}
}
