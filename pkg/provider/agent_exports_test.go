package provider

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestAgentExportsAnswerLikeTheirHermesFunctions(t *testing.T) {
	snapshotProviderRegistry(t)
	registerPluginForTest(t, Profile{Name: "acme-acp", AuthType: AuthTypeExternalProcess, BaseURL: "acp://acme", ProcessCommand: "acme"})
	ctx := context.Background()
	if !IsExternalProcessProvider(ctx, " ACME-ACP ") || IsExternalProcessProvider(ctx, "openrouter") || IsExternalProcessProvider(ctx, "") {
		t.Fatal("external process provider")
	}
	registerTestProfile(t, Profile{Name: "pinned-responses", BaseURL: "https://pinned.example/v1", Hostnames: []string{"pinned.example"}, ResponsesDropReplayItemIDs: true})
	if !DropsReplayItemIDs("https://pinned.example/v1") || DropsReplayItemIDs("https://generic.example/v1") {
		t.Fatal("drops replay item ids")
	}
	if AnthropicMaxOutput("claude-opus-4.6") != 128_000 || AnthropicMaxOutput("claude-sonnet-4-20250514") != 64_000 {
		t.Fatal("anthropic max output", AnthropicMaxOutput("claude-opus-4.6"))
	}
	if LongDecodeStaleMultiplier(map[string]any{"extra_body": map[string]any{"max_output_tokens": 262_144}}) != 8 || LongDecodeStaleMultiplier(nil) != 1 {
		t.Fatal("long decode multiplier")
	}
	if !ModelForcesMaxCompletionTokens("openai/gpt-5.4") || !ModelForcesMaxCompletionTokens("o3-mini") || ModelForcesMaxCompletionTokens("claude-sonnet-4") {
		t.Fatal("max completion tokens")
	}
	for _, c := range []struct {
		provider, base string
		want           bool
	}{{"openai-codex", "", true}, {"OpenAI-Codex", "", false}, {"", "https://chatgpt.com/backend-api/codex", true},
		{"", "https://evil.com/chatgpt.com/backend-api/codex", false}, {"custom", "https://sub.chatgpt.com/backend-api/codex/responses", true}} {
		if got := ResponsesRouteIsCodexBackend(c.provider, c.base); got != c.want {
			t.Fatalf("%q %q got %v", c.provider, c.base, got)
		}
	}
}

func TestPreflightKwargsRunsOnlyOnTheResponsesTransport(t *testing.T) {
	out, err := PreflightKwargs(GetTransport(string(ModeCodexResponses)), map[string]any{"model": "gpt-5.4", "instructions": "be brief",
		"input": []any{map[string]any{"role": "user", "content": "hi"}}, "store": false}, false, false, false)
	if err != nil || out["model"] != "gpt-5.4" {
		t.Fatal(out, err)
	}
	for mode, class := range map[APIMode]string{ModeChatCompletions: "ChatCompletionsTransport", ModeAnthropicMessages: "AnthropicTransport", modeBedrockConverse: "BedrockTransport"} {
		_, err := PreflightKwargs(GetTransport(string(mode)), map[string]any{}, false, false, false)
		if err == nil || err.Error() != "AttributeError: '"+class+"' object has no attribute 'preflight_kwargs'" {
			t.Fatalf("%s: %v", mode, err)
		}
	}
}

func TestProfileModelAPIModeAnswersTheHookAndWarnsOncePerProvider(t *testing.T) {
	ctx := context.Background()
	if mode, ok := ProfileModelAPIMode(ctx, Profile{Name: "plain"}, "m"); ok || mode != "" {
		t.Fatal("a profile without a per model hook answers None")
	}
	codex := Profile{Name: "hooked", APIModeForModelHook: func(model, _ string) APIMode {
		if strings.HasPrefix(model, "gpt-5") {
			return ModeCodexResponses
		}
		return ""
	}}
	if mode, ok := ProfileModelAPIMode(ctx, codex, "gpt-5.4"); !ok || mode != "codex_responses" {
		t.Fatal(mode, ok)
	}
	if _, ok := ProfileModelAPIMode(ctx, codex, "llama"); ok {
		t.Fatal("a hook answering None is no per model wire")
	}
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	apiModeWarnedMu.Lock()
	delete(apiModeWarned, "raising-hook")
	apiModeWarnedMu.Unlock()
	raising := Profile{Name: "raising-hook", Mode: ModeAnthropicMessages, APIModeForModelHook: func(string, string) APIMode { panic("catalog down") }}
	for range 2 {
		if mode, ok := ProfileModelAPIMode(ctx, raising, "m"); !ok || mode != "anthropic_messages" {
			t.Fatal(mode, ok)
		}
	}
	undeclared := Profile{Name: "raising-hook", APIModeForModelHook: raising.APIModeForModelHook}
	if mode, _ := ProfileModelAPIMode(ctx, undeclared, "m"); mode != "chat_completions" {
		t.Fatal(mode)
	}
	if n := strings.Count(buf.String(), "provider raising-hook: api_mode_for_model failed; using its declared api_mode"); n != 1 {
		t.Fatalf("warned %d times\n%s", n, buf.String())
	}
}

func TestDisplayOverrideableKeysAreTheGlobalDisplayDefaults(t *testing.T) {
	keys := DisplayOverrideableKeys()
	if len(keys) != 13 || !keys["tool_progress"] || !keys["busy_steer_ack_enabled"] || !keys["live_status"] || keys["webhook"] {
		t.Fatal(keys)
	}
}

func TestGetAnthropicKeyPrefersTheHomeDotenvInRegistryOrder(t *testing.T) {
	dir := t.TempDir()
	ctx := home.WithOverride(context.Background(), dir)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_TOKEN", "sk-ant-shell-token")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	if got := GetAnthropicKey(ctx); got != "sk-ant-shell-token" {
		t.Fatalf("got %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("ANTHROPIC_TOKEN=sk-ant-dotenv-token\nCLAUDE_CODE_OAUTH_TOKEN=oauth\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := GetAnthropicKey(ctx); got != "sk-ant-dotenv-token" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("ANTHROPIC_TOKEN", "")
	if err := os.WriteFile(filepath.Join(dir, ".env"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := GetAnthropicKey(ctx); got != "" {
		t.Fatalf("got %q", got)
	}
}
