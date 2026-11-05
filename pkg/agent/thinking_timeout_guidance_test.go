package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

type timeoutReason struct{ Value string }

type duckClassified struct {
	Reason     any
	StatusCode any
}

func duckTimeout(reason string) duckClassified {
	return duckClassified{Reason: timeoutReason{Value: reason}}
}

func reasoningSession(model string) provider.ClassifyInput {
	return provider.ClassifyInput{
		Err:      errors.New("server disconnected without sending complete message"),
		Provider: "nvidia", Model: model, ApproxTokens: 130_000, ContextLength: 200_000, NumMessages: 250,
	}
}

func TestAllKnownReasoningModelsOverride(t *testing.T) {
	for _, model := range []string{"nvidia/nemotron-3-ultra-550b-a55b", "deepseek/deepseek-r1"} {
		result := provider.ClassifyError(reasoningSession(model))
		if result.Reason != provider.ReasonTimeout || result.ShouldCompress {
			t.Fatalf("%s: %s compress=%v", model, result.Reason, result.ShouldCompress)
		}
	}
}

func TestNonReasoningModelLargeSessionStillRoutesToContextOverflow(t *testing.T) {
	result := provider.ClassifyError(reasoningSession("gpt-4o"))
	if result.Reason != provider.ReasonContextOverflow || !result.ShouldCompress {
		t.Fatalf("got %s compress=%v", result.Reason, result.ShouldCompress)
	}
}

func TestKnownReasoningModelsMatch(t *testing.T) {
	cases := [][2]string{
		{"nvidia/nemotron-3-ultra-550b-a55b", "connection reset by peer"},
		{"openai/o3-mini", "remote protocol error"},
		{"anthropic/claude-opus-4-6", "peer closed connection"},
		{"deepseek/deepseek-r1", "connection lost"},
		{"x-ai/grok-4-fast-reasoning", "server disconnected"},
	}
	for _, c := range cases {
		msg := c[1]
		if !IsThinkingTimeout(duckTimeout("timeout"), c[0], &msg) {
			t.Fatalf("%s %q did not match", c[0], msg)
		}
	}
	msg := "Connection Reset by peer"
	if !IsThinkingTimeout(&provider.Classified{Reason: provider.ReasonTimeout}, "deepseek/deepseek-r1", &msg) {
		t.Fatal("a provider verdict matches too, case insensitively")
	}
	if IsThinkingTimeout(duckTimeout("rate_limit"), "deepseek/deepseek-r1", &msg) {
		t.Fatal("only the timeout reason matches")
	}
	if IsThinkingTimeout(duckTimeout("timeout"), "gpt-4o", &msg) {
		t.Fatal("only reasoning models match")
	}
}

func TestEmptyErrorMsgReturnsFalse(t *testing.T) {
	empty := ""
	if IsThinkingTimeout(duckTimeout("timeout"), "nvidia/nemotron-3-ultra-550b-a55b", &empty) {
		t.Fatal("empty message matched")
	}
}

func TestNoneErrorMsgReturnsFalse(t *testing.T) {
	if IsThinkingTimeout(duckTimeout("timeout"), "nvidia/nemotron-3-ultra-550b-a55b", nil) {
		t.Fatal("None message matched")
	}
}

func TestGuidanceMentionsConfigPath(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	text := BuildThinkingTimeoutGuidance(context.Background(), "nvidia", "nvidia/nemotron-3-ultra-550b-a55b", "")
	if !strings.Contains(text, "providers.nvidia.models.nvidia/nemotron-3-ultra-550b-a55b.stale_timeout_seconds") {
		t.Fatalf("guidance: %q", text)
	}
	if !strings.HasPrefix(BuildThinkingTimeoutGuidance(context.Background(), "p", "m", "Nemotron"), "Nemotron was thinking") {
		t.Fatal("the prose label leads when given")
	}
}
