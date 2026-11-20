package provider

import (
	"context"
	"testing"
)

func TestResolveResponsesReasoningClampsAnEnabledNone(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ model, want string }{{"gpt-6-astra", "low"}, {"gpt-5.6-sol", "none"}} {
		got, enabled := ResolveResponsesReasoning(ctx, tc.model, "custom", "https://api.openai.com/v1", false, false, false, "none")
		if got != tc.want || !enabled {
			t.Fatalf("%s got %q %v", tc.model, got, enabled)
		}
	}
	if got, enabled := ResolveResponsesReasoning(ctx, "gpt-5.6-sol", "custom", "https://api.openai.com/v1", false, false, true, ""); got != "none" || enabled {
		t.Fatalf("disabled got %q %v", got, enabled)
	}
}
