package provider

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestModelForcesMaxCompletionTokensExportMatchesTheModelFamilies(t *testing.T) {
	if !config.ModelForcesMaxCompletionTokens("openai/gpt-5.4") || !config.ModelForcesMaxCompletionTokens("o3-mini") || config.ModelForcesMaxCompletionTokens("claude-sonnet-4") {
		t.Fatal("max completion tokens")
	}
}

func TestModelConsumesThoughtSignatureIsTheRouteSignatureRule(t *testing.T) {
	cases := []struct {
		model, baseURL, provider string
	}{
		{"gemini-2.5-pro", "", ""}, {"gpt-5", "https://generativelanguage.googleapis.com/v1beta", ""},
		{"gpt-5", "", "vertex"}, {"gpt-5", "https://api.openai.com/v1", "openai"}, {"mistral-large", "https://api.mistral.ai/v1", "mistral"},
	}
	for _, c := range cases {
		if ModelConsumesThoughtSignature(c.model, c.baseURL, c.provider) != modelConsumesThoughtSignature(c.model, c.baseURL, c.provider) {
			t.Fatalf("export differs for %+v", c)
		}
	}
	if !ModelConsumesThoughtSignature("gemini-2.5-pro", "", "") || ModelConsumesThoughtSignature("mistral-large", "https://api.mistral.ai/v1", "mistral") {
		t.Fatal("gemini model or mistral route misclassified")
	}
}

func TestEffortLadderIndexMatchesTheLadder(t *testing.T) {
	for _, level := range []string{"none", "high", "ultra", "bogus", ""} {
		if EffortLadderIndex(level) != ladderIndex(level) {
			t.Fatalf("export differs for %q", level)
		}
	}
	if EffortLadderIndex("xhigh") <= EffortLadderIndex("high") || EffortLadderIndex("bogus") != -1 {
		t.Fatal("ladder order")
	}
}
