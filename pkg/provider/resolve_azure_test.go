package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func openAIModelsBody(ids ...string) []byte {
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{"id": id, "object": "model"})
	}
	body, _ := json.Marshal(map[string]any{"object": "list", "data": data})
	return body
}

func anthropicErrorBody() []byte {
	body, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": "model not found"}})
	return body
}

func TestLooksLikeAnthropicPathAndExtractModelIDs(t *testing.T) {
	if !looksLikeAnthropicPath("https://r.services.ai.azure.com/anthropic") || !looksLikeAnthropicPath("https://r.example/anthropic/v1") || looksLikeAnthropicPath("https://r.example/openai/v1") {
		t.Fatal("anthropic path sniff")
	}
	var payload map[string]any
	_ = json.Unmarshal(openAIModelsBody("a", "b"), &payload)
	if got := extractModelIDs(payload); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("got %v", got)
	}
	if got := extractModelIDs(map[string]any{"data": "nope"}); got != nil {
		t.Fatalf("shape mismatch: %v", got)
	}
}

func TestDetectPathSniffWinsWithoutNetwork(t *testing.T) {
	result := DetectAzureEndpoint(context.Background(), "https://my-resource.services.ai.azure.com/anthropic", "k", nil)
	if result.APIMode != ModeAnthropicMessages || !result.IsAnthropic || result.Hostname != "my-resource.services.ai.azure.com" {
		t.Fatalf("got %+v", result)
	}
}

func TestDetectOpenAIModelsProbeSuccessAndAPIVersionFallback(t *testing.T) {
	var calls []string
	var seenKey, seenBearer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.RequestURI())
		seenKey = r.Header.Get("api-key")
		seenBearer = r.Header.Get("Authorization")
		if r.URL.Query().Get("api-version") == "" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(openAIModelsBody("gpt-5.4", "claude-opus-4-6"))
	}))
	defer srv.Close()
	result := DetectAzureEndpoint(context.Background(), srv.URL+"/openai/v1", "key-abc", nil)
	if result.APIMode != ModeChatCompletions || !result.ModelsProbeOK || !reflect.DeepEqual(result.Models, []string{"gpt-5.4", "claude-opus-4-6"}) {
		t.Fatalf("got %+v", result)
	}
	if !strings.Contains(result.Reason, "/models") {
		t.Fatalf("reason: %s", result.Reason)
	}
	if seenKey != "key-abc" || seenBearer != "Bearer key-abc" {
		t.Fatalf("auth headers: %q %q", seenKey, seenBearer)
	}
	var withoutVersion, withVersion bool
	for _, u := range calls {
		if strings.Contains(u, "api-version") {
			withVersion = true
		} else {
			withoutVersion = true
		}
	}
	if !withoutVersion || !withVersion {
		t.Fatalf("calls: %v", calls)
	}
}

func TestEntraModeSendsBearerOnly(t *testing.T) {
	var seenKey, seenBearer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenKey = r.Header.Get("api-key")
		seenBearer = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(openAIModelsBody())
	}))
	defer srv.Close()
	result := DetectAzureEndpoint(context.Background(), srv.URL, "", func(context.Context) (string, error) { return "jwt-token", nil })
	if result.APIMode != ModeChatCompletions || !result.ModelsProbeOK || len(result.Models) != 0 {
		t.Fatalf("got %+v", result)
	}
	if seenKey != "" || seenBearer != "Bearer jwt-token" {
		t.Fatalf("entra headers: %q %q", seenKey, seenBearer)
	}
}

func TestHTTPGetJSONBoundsSuccessBody(t *testing.T) {
	body := openAIModelsBody("test-deployment")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	d := &AzureDetector{}
	status, payload := d.httpGetJSON(context.Background(), srv.URL+"/models", "synthetic-key", nil)
	var want map[string]any
	_ = json.Unmarshal(body, &want)
	if status != 200 || !reflect.DeepEqual(payload, want) {
		t.Fatalf("got %d %v", status, payload)
	}
	body = append(body, []byte(strings.Repeat(" ", azureDetectJSONBodyMaxBytes))...)
	status, payload = d.httpGetJSON(context.Background(), srv.URL+"/models", "synthetic-key", nil)
	if status != 200 || payload != nil {
		t.Fatalf("oversized: %d %v", status, payload)
	}
}

func TestProbeAnthropicMessagesBoundsErrorBody(t *testing.T) {
	body := anthropicErrorBody()
	var seenVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(404)
			return
		}
		seenVersion = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	d := &AzureDetector{}
	if !d.probeAnthropicMessages(context.Background(), srv.URL, "synthetic-key", nil) {
		t.Fatal("anthropic shaped error must count")
	}
	if seenVersion != "2023-06-01" {
		t.Fatalf("anthropic-version: %q", seenVersion)
	}
	body = append(body, []byte(strings.Repeat(" ", azureDetectErrorBodyMaxBytes))...)
	if d.probeAnthropicMessages(context.Background(), srv.URL, "synthetic-key", nil) {
		t.Fatal("oversized body must not supply the signal")
	}
	result := DetectAzureEndpoint(context.Background(), srv.URL, "synthetic-key", nil)
	if result.APIMode != "" || !strings.Contains(result.Reason, "Could not probe") {
		t.Fatalf("got %+v", result)
	}
}

func TestDetectFallsToAnthropicProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/messages") {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"messages: model probe not found"}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	result := DetectAzureEndpoint(context.Background(), srv.URL+"/v1", "k", nil)
	if result.APIMode != ModeAnthropicMessages || !result.IsAnthropic || result.Reason != "Endpoint accepts Anthropic Messages shape" {
		t.Fatalf("got %+v", result)
	}
}

func TestStripTrailingV1(t *testing.T) {
	if got := stripTrailingV1("https://h/openai/v1/"); got != "https://h/openai" {
		t.Fatalf("got %q", got)
	}
	if got := stripTrailingV1("https://h/openai"); got != "https://h/openai" {
		t.Fatalf("got %q", got)
	}
}
