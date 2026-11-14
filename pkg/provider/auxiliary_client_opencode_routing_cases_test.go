package provider

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// Isolated home with the OpenCode keys and two opencode-go custom entries, one pinning its api mode
func auxOpencodeHome(t *testing.T) context.Context {
	t.Helper()
	auxIsolateClientCache(t)
	t.Setenv("OPENCODE_GO_API_KEY", "sk-go-test")
	t.Setenv("OPENCODE_ZEN_API_KEY", "sk-zen-test")
	return auxMainTestHome(t, "custom_providers:\n  - name: opencode-go-bridge\n    base_url: https://opencode.ai/zen/go/v1\n    api_key: sk-bridge\n"+
		"  - name: opencode-go-pinned\n    base_url: https://opencode.ai/zen/go/v1\n    api_key: sk-pinned\n    api_mode: chat_completions\n")
}

var auxOpencodeWireByModel = []struct {
	model    string
	expected reflect.Type
}{
	{"gpt-5.6-luna", reflect.TypeOf(&CodexAuxiliaryClient{})},
	{"minimax-m2.5", reflect.TypeOf(&AnthropicAuxiliaryClient{})},
	{"glm-5", reflect.TypeOf(&auxOpenAIClient{})},
}

func TestBuiltinOpencodeGoClientFollowsTheModelNotThePersistedMode(t *testing.T) {
	ctx := auxOpencodeHome(t)
	for _, staleAPIMode := range []string{"", "chat_completions"} {
		for _, tc := range auxOpencodeWireByModel {
			client, resolved, err := ResolveProviderClient(ctx, "opencode-go", tc.model, ProviderClientOptions{APIMode: staleAPIMode})
			if err != nil || resolved != tc.model || reflect.TypeOf(client) != tc.expected {
				t.Fatalf("%s stale=%q got (%T, %q, %v)", tc.model, staleAPIMode, client, resolved, err)
			}
			asyncClient, _, err := ResolveProviderClient(ctx, "opencode-go", tc.model, ProviderClientOptions{APIMode: staleAPIMode, AsyncMode: true})
			if err != nil {
				t.Fatal(err)
			}
			_, isAsyncCodex := asyncClient.(*AsyncCodexAuxiliaryClient)
			if isAsyncCodex != (tc.expected == reflect.TypeOf(&CodexAuxiliaryClient{})) {
				t.Fatalf("%s stale=%q async client %T", tc.model, staleAPIMode, asyncClient)
			}
		}
	}
}

func TestNamedCustomOpencodeFamilyEntryFollowsTheModel(t *testing.T) {
	ctx := auxOpencodeHome(t)
	for _, tc := range auxOpencodeWireByModel {
		client, resolved, err := ResolveProviderClient(ctx, "custom:opencode-go-bridge", tc.model, ProviderClientOptions{})
		if err != nil || resolved != tc.model || reflect.TypeOf(client) != tc.expected {
			t.Fatalf("%s got (%T, %q, %v)", tc.model, client, resolved, err)
		}
		want := "https://opencode.ai/zen/go/v1"
		if _, isAnthropic := client.(*AnthropicAuxiliaryClient); isAnthropic {
			want = "https://opencode.ai/zen/go"
		}
		if base := strings.TrimRight(client.BaseURL(), "/"); base != want {
			t.Fatalf("%s base %q want %q", tc.model, base, want)
		}
	}
}

func TestNamedCustomOpencodeFamilyEntryWithDeclaredApiModeIsHonoured(t *testing.T) {
	ctx := auxOpencodeHome(t)
	client, resolved, err := ResolveProviderClient(ctx, "custom:opencode-go-pinned", "gpt-5.6-luna", ProviderClientOptions{})
	if err != nil || resolved != "gpt-5.6-luna" {
		t.Fatalf("got (%T, %q, %v)", client, resolved, err)
	}
	if _, ok := client.(*auxOpenAIClient); !ok {
		t.Fatalf("want the OpenAI wire client, got %T", client)
	}
	if base := strings.TrimRight(client.BaseURL(), "/"); base != "https://opencode.ai/zen/go/v1" {
		t.Fatalf("base %q", base)
	}
}
