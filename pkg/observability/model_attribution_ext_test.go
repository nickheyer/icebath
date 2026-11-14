package observability_test

import (
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
)

func TestContextPeakIsOneRowPerConversationAcrossCompressionRotation(t *testing.T) {
	ctx := observability.DirectRuntimeForTest(t)
	public := "anthropic/claude-sonnet-4"
	convo := provider.WithPortalConversationContext(ctx, "s1")
	observability.AttributionTurnForTest(convo, "s1", "t1", "openrouter", public, 176_000, nil)
	observability.AttributionTurnForTest(convo, "child", "c1", "openrouter", public, 199_000, map[string]any{"parent_session_id": "s1"})
	observability.RotateSegment(convo, "s1", "s1c")
	observability.AttributionTurnForTest(convo, "s1c", "t2", "openrouter", public, 30_000, nil)
	observability.FinalizeSessionForTest(ctx, "child")
	observability.FinalizeSessionForTest(ctx, "s1")
	if got := observability.StoredRowsForTest(t, ctx, observability.ContextPeakMetric); len(got) != 0 {
		t.Fatalf("emitted before the lineage closed %v", got)
	}
	observability.FinalizeSessionForTest(ctx, "s1c")
	want := []string{"limit_hit=no,model=" + public + ",peak_fill_bucket=75_to_90,provider=openrouter,window_bucket=128k_to_256k|1"}
	if got := observability.StoredRowsForTest(t, ctx, observability.ContextPeakMetric); !slices.Equal(got, want) {
		t.Fatalf("context peak rows %v", got)
	}
}
