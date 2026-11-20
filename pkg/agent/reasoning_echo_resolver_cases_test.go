package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Points ICEBATH_HOME at a profile declaring the named custom provider llamacpp-k3
func writeReasoningEchoHome(t *testing.T, reasoningEcho bool) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), "icebath")
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{"model:", "  default: kimi-k3", "  provider: llamacpp-k3"}
	if reasoningEcho {
		lines = append(lines, "  reasoning_echo: true")
	}
	lines = append(lines, "providers:", "  llamacpp-k3:", "    base_url: http://127.0.0.1:8098/v1", "    key_env: LLAMACPP_KEY")
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	return h
}

// Agent whose echo flag is materialized by the init config read on the resolved custom route
func reasoningEchoInitAgent() *AIAgent {
	a := &AIAgent{apiMode: "chat_completions", provider: "custom", baseURL: "http://127.0.0.1:8098/v1", model: "kimi-k3"}
	a.baseURLLower = a.baseURL
	a.reasoningEchoFlag = readReasoningEchoFromConfig(context.Background())
	return a
}

func TestReasoningEchoResolverE2ENamedCustomProviderResolvesToCustom(t *testing.T) {
	h := writeReasoningEchoHome(t, true)
	rt, err := provider.ResolveRuntimeProvider(context.Background(), h, "llamacpp-k3", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if rt.Provider != "custom" || strings.TrimRight(rt.BaseURL, "/") != "http://127.0.0.1:8098/v1" {
		t.Fatalf("runtime %q %q", rt.Provider, rt.BaseURL)
	}
}

func TestReasoningEchoResolverE2EFlagMaterializesFromRealConfigAndEchoes(t *testing.T) {
	writeReasoningEchoHome(t, true)
	a := reasoningEchoInitAgent()
	if a.reasoningEchoFlag != true || !a.reasoningEchoOptIn() || !a.needsThinkingReasoningPad() {
		t.Fatalf("flag %v opt in %v pad %v", a.reasoningEchoFlag, a.reasoningEchoOptIn(), a.needsThinkingReasoningPad())
	}
	source := map[string]any{"role": "assistant", "content": "calling a tool", "reasoning_content": "the model's chain of thought"}
	apiMsg := map[string]any{"role": "assistant", "content": "calling a tool", "reasoning_content": "the model's chain of thought"}
	a.CopyReasoningContentForAPI(source, apiMsg)
	if apiMsg["reasoning_content"] != "the model's chain of thought" {
		t.Fatalf("api message %v", apiMsg)
	}
}

func TestReasoningEchoResolverE2EFlagAbsentStillReplaysButNeverPads(t *testing.T) {
	writeReasoningEchoHome(t, false)
	a := reasoningEchoInitAgent()
	if a.reasoningEchoFlag != false || a.reasoningEchoOptIn() {
		t.Fatalf("flag %v opt in %v", a.reasoningEchoFlag, a.reasoningEchoOptIn())
	}
	source := map[string]any{"role": "assistant", "content": "calling a tool", "reasoning_content": "the model's chain of thought"}
	apiMsg := map[string]any{"role": "assistant", "content": "calling a tool", "reasoning_content": "the model's chain of thought"}
	a.CopyReasoningContentForAPI(source, apiMsg)
	if apiMsg["reasoning_content"] != "the model's chain of thought" {
		t.Fatalf("api message %v", apiMsg)
	}
	bare := map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "c1"}}}
	apiBare := map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "c1"}}}
	a.CopyReasoningContentForAPI(bare, apiBare)
	if _, ok := apiBare["reasoning_content"]; ok {
		t.Fatalf("bare turn padded %v", apiBare)
	}
}
