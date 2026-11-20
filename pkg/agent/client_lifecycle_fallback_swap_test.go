package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

func TestFallbackRebuildPreservesRotatingAndStaticCredentials(t *testing.T) {
	for _, mode := range []string{"chat_completions", "codex_responses"} {
		for _, dynamic := range []bool{false, true} {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			token := "first-token"
			var captured []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured = append(captured, r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"fixture","choices":[]}`))
			}))
			var source any = token
			if dynamic {
				source = func() (string, error) { return token, nil }
			}
			client := provider.NewOpenAIClient(source, srv.URL+"/v1", provider.OpenAIClientOptions{})
			a := &AIAgent{}
			if err := swapFallbackClients(context.Background(), a, client, "fixture", "fixture", client.BaseURL(), mode); err != nil {
				t.Fatal(err)
			}
			built, err := openAIClientFromKwargs(a.clientKwargs)
			if err != nil {
				t.Fatal(err)
			}
			rebuilt := built.(provider.AuxClient)
			for _, value := range []string{"first-token", "rotated-token"} {
				token = value
				if _, err := rebuilt.CreateChatCompletion(context.Background(), map[string]any{"model": "fixture", "messages": []any{}}); err != nil {
					t.Fatal(err)
				}
			}
			want := []string{"Bearer first-token", "Bearer first-token"}
			if dynamic {
				want[1] = "Bearer rotated-token"
			}
			if len(captured) != 2 || captured[0] != want[0] || captured[1] != want[1] {
				t.Fatalf("%s dynamic=%v: captured %v want %v", mode, dynamic, captured, want)
			}
			if dynamic {
				src, ok := a.clientKwargs["api_key"].(func() (string, error))
				if !ok || a.apiKey != "" {
					t.Fatalf("rotating source not kept: %T %q", a.clientKwargs["api_key"], a.apiKey)
				}
				if minted, _ := src(); minted != "rotated-token" {
					t.Fatalf("kept source mints %q", minted)
				}
			} else if a.apiKey != "first-token" || a.clientKwargs["api_key"] != "first-token" {
				t.Fatalf("static key not kept: %q %v", a.apiKey, a.clientKwargs["api_key"])
			}
			if a.client != client {
				t.Fatal("fallback client not installed")
			}
			_ = client.Close()
			_ = rebuilt.Close()
			srv.Close()
		}
	}
}

func TestAnthropicFallbackKeepsCallableWithoutTreatingItAsOauthText(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	client := provider.NewOpenAIClient(func() (string, error) { return "fixture-token", nil }, "http://localhost:1234/v1", provider.OpenAIClientOptions{})
	a := &AIAgent{client: "stale", clientKwargs: map[string]any{"api_key": "old"}}
	if err := swapFallbackClients(context.Background(), a, client, "anthropic", "fixture", client.BaseURL(), "anthropic_messages"); err != nil {
		t.Fatal(err)
	}
	src, ok := a.anthropicKeyValue().(*provider.TaggedTokenSource)
	if !ok {
		t.Fatalf("anthropic credential %T", a.anthropicKeyValue())
	}
	if minted, err := src.Mint(context.Background()); err != nil || minted != "fixture-token" {
		t.Fatalf("kept source mints %q %v", minted, err)
	}
	if a.apiKey != "" || a.anthropicAPIKey != "" {
		t.Fatalf("callable rendered as key text: %q %q", a.apiKey, a.anthropicAPIKey)
	}
	if a.isAnthropicOAuth {
		t.Fatal("callable treated as OAuth text")
	}
	if a.anthropicClient == nil || a.client != nil || len(a.clientKwargs) != 0 || a.anthropicBaseURL != client.BaseURL() {
		t.Fatalf("anthropic install: client=%v kwargs=%v base=%q", a.client, a.clientKwargs, a.anthropicBaseURL)
	}
	closeClientQuietly(a.anthropicClient)
	_ = client.Close()
}

func TestFallbackToBedrockBindsRuntimeNotGenericClient(t *testing.T) {
	for _, mode := range []string{"anthropic_messages", "bedrock_converse"} {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		t.Setenv("AWS_ACCESS_KEY_ID", "AKIAFAKEFAKEFAKEFAKE")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "fake-secret")
		t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
		runtimeEU := "https://bedrock-runtime.eu-west-1.amazonaws.com"
		fb := provider.NewOpenAIClient("aws-sdk", runtimeEU, provider.OpenAIClientOptions{})
		a := &AIAgent{model: "us.amazon.nova-pro-v1:0", provider: "bedrock", apiMode: mode, client: "stale",
			clientKwargs: map[string]any{"api_key": "old"}, anthropicClient: "stale"}
		a.setBaseURL(runtimeEU)
		if err := swapFallbackClients(context.Background(), a, fb, "bedrock", a.model, runtimeEU, mode); err != nil {
			t.Fatal(err)
		}
		if a.client != nil || len(a.clientKwargs) != 0 {
			t.Fatalf("%s: generic client installed %v %v", mode, a.client, a.clientKwargs)
		}
		if a.bedrockRegion != "eu-west-1" || a.apiKey != "aws-sdk" || a.anthropicAPIKey != "aws-sdk" || a.anthropicBaseURL != runtimeEU || a.isAnthropicOAuth {
			t.Fatalf("%s: binding %q %q %q %q %v", mode, a.bedrockRegion, a.apiKey, a.anthropicAPIKey, a.anthropicBaseURL, a.isAnthropicOAuth)
		}
		if (mode == "anthropic_messages") != (a.anthropicClient != nil) {
			t.Fatalf("%s: anthropic client %T", mode, a.anthropicClient)
		}
		_ = fb.Close()
	}
}
