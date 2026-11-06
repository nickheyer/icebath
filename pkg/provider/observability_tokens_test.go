package provider

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/observability"
)

func TestObservabilityEstimateToolsTokensHookIsTheProviderEstimate(t *testing.T) {
	defs := []map[string]any{{"type": "function", "function": map[string]any{"name": "read_file", "description": "Read a file from disk", "parameters": map[string]any{"type": "object"}}}}
	if observability.EstimateToolsTokensHook == nil {
		t.Fatal("the provider package installs the hook")
	}
	if got, want := observability.EstimateToolsTokensHook(defs), EstimateToolsTokens(defs); got != want || got == 0 {
		t.Fatalf("hook %d estimate %d", got, want)
	}
	if observability.EstimateToolsTokensHook(nil) != EstimateToolsTokens(nil) {
		t.Fatal("empty definitions agree")
	}
}

func TestObservabilityConversationContextHookReadsThePortalLineage(t *testing.T) {
	if observability.ConversationContextHook == nil {
		t.Fatal("the provider package installs the hook")
	}
	ctx := WithPortalConversationContext(context.Background(), "root-conv-1")
	if got := observability.ConversationContextHook(ctx); got != "root-conv-1" {
		t.Fatalf("hook read %q", got)
	}
	if got := observability.ConversationContextHook(context.Background()); got != "" {
		t.Fatalf("unset lineage read %q", got)
	}
}
