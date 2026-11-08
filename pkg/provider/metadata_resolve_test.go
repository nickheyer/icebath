package provider

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestToolModelContextLengthRoutesTheQueryThroughTheResolver(t *testing.T) {
	bedrockIsolatedEnv(t)
	bedrockStubProbe(t, 0)
	ctx := context.Background()
	base := "https://bedrock-runtime.us-east-2.amazonaws.com"
	SaveContextLength(ctx, "us.anthropic.claude-sonnet-4-6", base, 200_000, "")
	q := tools.ContextLengthQuery{Model: "us.anthropic.claude-sonnet-4-6", BaseURL: base, Provider: "bedrock"}
	if got := ToolModelContextLength(ctx, q); got != 1_000_000 {
		t.Fatalf("provider aware window %d", got)
	}
	q.ConfigContextLength = 150_000
	if got := ToolModelContextLength(ctx, q); got != 150_000 {
		t.Fatalf("explicit model.context_length lost to %d", got)
	}
}
