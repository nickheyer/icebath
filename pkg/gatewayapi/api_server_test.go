package gatewayapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

func TestDefaultConfig(t *testing.T) {
	adapter := makeAdapter(t, "", nil)
	if adapter.host != "127.0.0.1" || adapter.apiKey != "" || adapter.Platform != gateway.PlatformAPIServer {
		t.Fatalf("host=%v key=%q platform=%v", adapter.host, adapter.apiKey, adapter.Platform)
	}
}

func TestNoKeyConfiguredAllowsAll(t *testing.T) {
	adapter := makeAdapter(t, "", nil)
	request := gateway.NewWebRequest(httptest.NewRequest("GET", "/v1/models", nil), 0)
	if resp := adapter.checkAuth(context.Background(), request); resp != nil {
		t.Fatalf("auth refused with %d", resp.Status)
	}
}

func TestNonASCIIBearerTokenReturns401Not500(t *testing.T) {
	adapter := makeAdapter(t, "sk-test123", nil)
	wire := httptest.NewRequest("GET", "/v1/models", nil)
	wire.Header.Set("Authorization", "Bearer ské-not-the-key")
	request := gateway.NewWebRequest(wire, 0)
	resp := adapter.checkAuth(context.Background(), request)
	if resp == nil || resp.Status != http.StatusUnauthorized {
		t.Fatalf("resp = %v, want 401", resp)
	}
}

func TestSecurityHeadersPresent(t *testing.T) {
	cli := newTestClient(t, createApp(t, makeAdapter(t, "", nil)))
	resp, _ := cli.do("GET", "/health", nil, nil)
	want := map[string]string{"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'",
		"Permissions-Policy": "camera=(), microphone=(), geolocation=()", "Strict-Transport-Security": "max-age=31536000; includeSubDomains",
		"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "X-XSS-Protection": "0", "Referrer-Policy": "no-referrer"}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	for header, value := range want {
		if got := resp.Header.Get(header); got != value {
			t.Fatalf("%s = %q, want %q", header, got, value)
		}
	}
}

func TestHealthReportsVersion(t *testing.T) {
	cli := newTestClient(t, createApp(t, makeAdapter(t, "", nil)))
	resp, body := cli.do("GET", "/health", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	info, err := updaterecord.GetVersionInfo()
	if err != nil {
		t.Fatal(err)
	}
	version, ok := jsonBody(t, body)["version"].(string)
	if !ok || version != info.BaseVersion {
		t.Fatalf("version = %v, want %q", jsonBody(t, body)["version"], info.BaseVersion)
	}
}

func TestModelsReturnsHermesAgent(t *testing.T) {
	cli := newTestClient(t, createApp(t, makeAdapter(t, "", nil)))
	resp, body := cli.do("GET", "/v1/models", nil, nil)
	data := jsonBody(t, body)
	models, _ := data["data"].([]any)
	if resp.StatusCode != http.StatusOK || data["object"] != "list" || len(models) != 1 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	first := models[0].(map[string]any)
	if first["id"] != "icebath-agent" || first["owned_by"] != "icebath" {
		t.Fatalf("model = %v", first)
	}
}

func TestModelsReturnsProfileName(t *testing.T) {
	adapter := makeAdapter(t, "", nil)
	adapter.modelName = "lucas"
	cli := newTestClient(t, createApp(t, adapter))
	resp, body := cli.do("GET", "/v1/models", nil, nil)
	models, _ := jsonBody(t, body)["data"].([]any)
	if resp.StatusCode != http.StatusOK || len(models) == 0 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	first := models[0].(map[string]any)
	if first["id"] != "lucas" || first["root"] != "lucas" {
		t.Fatalf("model = %v", first)
	}
}

func TestInvalidJSONReturns400(t *testing.T) {
	cli := newTestClient(t, createApp(t, makeAdapter(t, "", nil)))
	resp, body := cli.do("POST", "/v1/chat/completions", "not json", map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(errorMessage(t, body), "Invalid JSON") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
}

func TestMissingMessagesReturns400(t *testing.T) {
	cli := newTestClient(t, createApp(t, makeAdapter(t, "", nil)))
	resp, body := cli.do("POST", "/v1/chat/completions", map[string]any{"model": "test"}, nil)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(errorMessage(t, body), "messages") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
}

func TestChatCompletionsRequiresAuth(t *testing.T) {
	cli := newTestClient(t, createApp(t, makeAdapter(t, "sk-secret", nil)))
	resp, body := cli.do("POST", "/v1/chat/completions", map[string]any{"model": "test",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
}

func TestBrowserOriginRejectedByDefault(t *testing.T) {
	cli := newTestClient(t, createApp(t, makeAdapter(t, "", nil)))
	resp, _ := cli.do("GET", "/health", nil, map[string]string{"Origin": "http://evil.example"})
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("status=%d allow-origin=%q", resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
	}
}

func TestCORSOptionsPreflightAllowedForConfiguredOrigin(t *testing.T) {
	cli := newTestClient(t, createApp(t, makeAdapter(t, "", []any{"http://localhost:3000"})))
	resp, _ := cli.do("OPTIONS", "/v1/chat/completions", nil, map[string]string{"Origin": "http://localhost:3000",
		"Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "Authorization, Content-Type, Idempotency-Key"})
	allowed := resp.Header.Get("Access-Control-Allow-Headers")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Access-Control-Allow-Origin") != "http://localhost:3000" ||
		!strings.Contains(allowed, "Authorization") || !strings.Contains(allowed, "Idempotency-Key") {
		t.Fatalf("status=%d headers=%v", resp.StatusCode, resp.Header)
	}
}

func TestDeterministic(t *testing.T) {
	ctx := context.Background()
	if deriveChatSessionID(ctx, "sys", "hello", "") != deriveChatSessionID(ctx, "sys", "hello", "") {
		t.Fatal("same inputs derived different ids")
	}
}

func TestDifferentSystemPrompt(t *testing.T) {
	ctx := context.Background()
	if deriveChatSessionID(ctx, "You are a pirate.", "Hello", "") == deriveChatSessionID(ctx, "You are a robot.", "Hello", "") {
		t.Fatal("different system prompts derived one id")
	}
}

func TestRoutedProfileNamespacesTheIDWithoutMovingDefault(t *testing.T) {
	ctx := context.Background()
	sum := sha256.Sum256([]byte("sys\nhello"))
	legacy := "api-" + hex.EncodeToString(sum[:])[:16]
	if deriveChatSessionID(ctx, "sys", "hello", "") != legacy || deriveChatSessionID(ctx, "sys", "hello", "default") != legacy {
		t.Fatal("default id moved")
	}
	research := deriveChatSessionID(ctx, "sys", "hello", "research")
	if research == legacy || research != deriveChatSessionID(ctx, "sys", "hello", "research") {
		t.Fatalf("research id = %q", research)
	}
}

func TestMasksSecretValueButPreservesStructure(t *testing.T) {
	secret := "sk-api-server-leak-1234567890"
	out := redactAPIErrorText(errors.New("auth failed OPENAI_API_KEY="+secret), -1)
	if strings.Contains(out, secret) || !strings.Contains(out, "OPENAI_API_KEY=") {
		t.Fatalf("out = %q", out)
	}
}

func TestLimitTruncatesAfterRedaction(t *testing.T) {
	if out := redactAPIErrorText(strings.Repeat("x", 500), 50); len(out) != 50 {
		t.Fatalf("len = %d", len(out))
	}
}

// Adapter with the model routes configured
func makeRoutingAdapter(t *testing.T, routes map[string]any) *APIServerAdapter {
	t.Helper()
	adapter, err := NewAPIServerAdapter(context.Background(), &gateway.PlatformConfig{Enabled: true,
		Extra: map[string]any{"model_routes": routes}})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func TestRouteWithoutModelIsDropped(t *testing.T) {
	adapter := makeRoutingAdapter(t, map[string]any{"bad": map[string]any{"provider": "openrouter"}})
	if adapter.modelRoutes != nil && len(adapter.modelRoutes.routes) != 0 {
		t.Fatalf("routes = %v", adapter.modelRoutes.routes)
	}
}

func TestModelsEndpointRouteAliasFieldsAndNoSecrets(t *testing.T) {
	adapter := makeRoutingAdapter(t, map[string]any{"my-alias": map[string]any{"model": "openai/gpt-5", "api_key": "sk-route-secret"}})
	cli := newTestClient(t, createApp(t, adapter))
	_, body := cli.do("GET", "/v1/models", nil, nil)
	models, _ := jsonBody(t, body)["data"].([]any)
	var alias map[string]any
	for _, model := range models {
		if entry := model.(map[string]any); entry["id"] == "my-alias" {
			alias = entry
		}
	}
	if alias == nil || alias["root"] != "openai/gpt-5" || alias["parent"] != adapter.modelName {
		t.Fatalf("alias = %v", alias)
	}
	if strings.Contains(string(body), "sk-route-secret") {
		t.Fatal("route api_key leaked through /v1/models")
	}
}

func TestCapabilitiesAdvertisesPluginSafeContract(t *testing.T) {
	adapter := makeAdapter(t, "", nil)
	t.Cleanup(func() {
		if adapter.runIdempotencyStore != nil {
			adapter.runIdempotencyStore.Close()
		}
	})
	cli := newTestClient(t, createApp(t, adapter))
	resp, body := cli.do("GET", "/v1/capabilities", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	data := jsonBody(t, body)
	auth, _ := data["auth"].(map[string]any)
	runtime, _ := data["runtime"].(map[string]any)
	features, _ := data["features"].(map[string]any)
	endpoints, _ := data["endpoints"].(map[string]any)
	idempotency, _ := features["runs_idempotency"].(map[string]any)
	runStatus, _ := endpoints["run_status"].(map[string]any)
	checks := []struct {
		got, want any
	}{
		{data["object"], "hermes.api_server.capabilities"}, {data["platform"], "icebath-agent"}, {data["model"], "icebath-agent"},
		{auth["type"], "bearer"}, {auth["required"], false}, {runtime["mode"], "server_agent"}, {runtime["tool_execution"], "server"},
		{runtime["split_runtime"], false}, {features["chat_completions"], true}, {features["run_status"], true},
		{features["run_events_sse"], true}, {idempotency["supported"], true}, {idempotency["durable"], true},
		{features["model_options"], true}, {features["session_continuity_header"], "X-Hermes-Session-Id"},
		{runStatus["path"], "/v1/runs/{run_id}"},
	}
	for i, check := range checks {
		if check.got != check.want {
			t.Fatalf("check %d: got %v, want %v (body %s)", i, check.got, check.want, body)
		}
	}
	for name, path := range map[string]string{"model_options": "/api/model/options", "skills": "/v1/skills", "toolsets": "/v1/toolsets"} {
		endpoint, _ := endpoints[name].(map[string]any)
		if len(endpoint) != 2 || endpoint["method"] != "GET" || endpoint["path"] != path {
			t.Fatalf("endpoint %s = %v", name, endpoint)
		}
	}
}

func TestInvalidPreviousResponseIDReturns404(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	adapter := makeAdapter(t, "", nil)
	cli := newTestClient(t, createApp(t, adapter))
	resp, body := cli.do("POST", "/v1/responses", map[string]any{"model": "icebath-agent", "input": "follow up",
		"previous_response_id": "resp_nonexistent"}, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
}

func TestTraversalSessionIDHeaderRejected(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cli := newTestClient(t, createApp(t, makeAdapter(t, "sk-secret", nil)))
	for _, bad := range []string{"../../../../etc/pwned", "/abs/path", "..\\win"} {
		resp, body := cli.do("POST", "/v1/chat/completions", map[string]any{"model": "icebath-agent",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}}},
			map[string]string{"X-Hermes-Session-Id": bad, "Authorization": "Bearer sk-secret"})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%q answered %d: %s", bad, resp.StatusCode, body)
		}
	}
}

func TestEnvOverrideCORSOrigins(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("API_SERVER_ENABLED", "true")
	t.Setenv("API_SERVER_KEY", "opensslrandhex32strongkey")
	t.Setenv("API_SERVER_CORS_ORIGINS", "http://localhost:3000, http://127.0.0.1:3000")
	config, err := gateway.LoadGatewayConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	platform := config.Platforms[gateway.PlatformAPIServer]
	if platform == nil {
		t.Fatal("api_server platform not configured")
	}
	origins, _ := platform.Extra["cors_origins"].([]any)
	if len(origins) != 2 || origins[0] != "http://localhost:3000" || origins[1] != "http://127.0.0.1:3000" {
		t.Fatalf("cors_origins = %#v", platform.Extra["cors_origins"])
	}
}

func TestAPIServerInConnectedPlatforms(t *testing.T) {
	ctx := context.Background()
	config := gateway.DefaultGatewayConfig(ctx)
	config.Platforms[gateway.PlatformAPIServer] = &gateway.PlatformConfig{Enabled: true, Extra: map[string]any{"key": "opensslrandhex32strongkey"}}
	for _, platform := range config.GetConnectedPlatforms(ctx) {
		if platform == gateway.PlatformAPIServer {
			return
		}
	}
	t.Fatal("api_server not among the connected platforms")
}
