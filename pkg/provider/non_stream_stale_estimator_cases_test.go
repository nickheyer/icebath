package provider

import (
	"strings"
	"testing"
)

func TestEstimatorResponsesApiInput(t *testing.T) {
	payload := map[string]any{"model": "gpt-5.5", "instructions": strings.Repeat("i", 1000), "input": strings.Repeat("x", 4000),
		"tools": []any{map[string]any{"name": "t", "description": strings.Repeat("d", 200)}}}
	if tokens := EstimateRequestContextTokens(t.Context(), payload); tokens < 1200 {
		t.Fatalf("Responses API estimator returned %d", tokens)
	}
}

func TestEstimatorEmptyInputs(t *testing.T) {
	for _, payload := range []any{map[string]any{}, []any{}, nil} {
		if tokens := EstimateRequestContextTokens(t.Context(), payload); tokens != 0 {
			t.Fatalf("%v -> %d", payload, tokens)
		}
	}
}

func TestOpenaiCodexStaleFloorTiers(t *testing.T) {
	small, large := CodexStaleTimeoutFloor(55000), CodexStaleTimeoutFloor(120000)
	if !(0 < small && small <= large) {
		t.Fatalf("small %v large %v", small, large)
	}
}
