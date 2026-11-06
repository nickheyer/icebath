package observability_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
)

const engagementPublic = "anthropic/claude-sonnet-4"

func TestACompressedConversationIsOneSessionRowWithItsWholeVolume(t *testing.T) {
	ctx := observability.DirectRuntimeForTest(t)
	convo := provider.WithPortalConversationContext(ctx, "s1")
	observability.EngagementTurnForTest(convo, "s1", "t1", "cli", 2)
	observability.EngagementTurnForTest(convo, "s1", "t2", "cli", 1)
	observability.RotateSegment(convo, "s1", "s1c")
	observability.EngagementTurnForTest(convo, "s1c", "t3", "cli", 0)
	observability.FinalizeSessionForTest(ctx, "s1")
	if got := observability.StoredRowsForTest(t, ctx, observability.SessionMetric); len(got) != 0 {
		t.Fatalf("a segment reported on its own %v", got)
	}
	observability.FinalizeSessionForTest(ctx, "s1c")
	rows := observability.StoredRowsForTest(t, ctx, observability.SessionMetric)
	if len(rows) != 1 || !strings.HasSuffix(rows[0], "|1") {
		t.Fatalf("session rows %v", rows)
	}
	for _, want := range []string{"turn_count_bucket=3_to_5", "model_call_count_bucket=3_to_5", "tool_call_count_bucket=3_to_5", "message_count_bucket=6_to_10"} {
		if !strings.Contains(rows[0], want) {
			t.Fatalf("session row %s lacks %s", rows[0], want)
		}
	}
}

func TestOnlyCompressionJoinsSegmentsAndTheRetiredSegmentClosesAtHandOff(t *testing.T) {
	ctx := observability.DirectRuntimeForTest(t)
	convo := provider.WithPortalConversationContext(ctx, "g1")
	observability.EngagementTurnForTest(convo, "g1", "t1", "telegram", 0)
	observability.RotateSegment(convo, "g1", "g1c")
	observability.EngagementTurnForTest(convo, "g1c", "t2", "telegram", 0)
	observability.CloseSessionRun(convo, "g1c")
	observability.EngagementTurnForTest(convo, "g2", "t3", "telegram", 0)
	observability.CloseSessionRun(convo, "g2")
	observability.EngagementTurnForTest(convo, "g3", "t4", "telegram", 0)
	observability.CloseSessionRun(convo, "g3")
	observability.InvokeHookForTest(convo, "pre_llm_call", map[string]any{"session_id": "m1", "task_id": "t5", "platform": "cli"})
	observability.RotateSegment(convo, "m1", "m1c")
	observability.FinalizeSessionForTest(convo, "m1c")
	observability.FinishTaskRun(convo, "m1", "t5", "cli", map[string]any{"completed": true}, nil)
	var got []string
	for _, row := range observability.StoredRowsForTest(t, ctx, observability.SessionMetric) {
		platform, turns := "", ""
		for _, part := range strings.Split(strings.SplitN(row, "|", 2)[0], ",") {
			switch {
			case strings.HasPrefix(part, "platform="):
				platform = strings.TrimPrefix(part, "platform=")
			case strings.HasPrefix(part, "turn_count_bucket="):
				turns = strings.TrimPrefix(part, "turn_count_bucket=")
			}
		}
		got = append(got, platform+"|"+turns+"|"+strings.SplitN(row, "|", 2)[1])
	}
	slices.Sort(got)
	if want := []string{"none|1|1", "telegram|1|2", "telegram|2|1"}; !slices.Equal(got, want) {
		t.Fatalf("session rows %v", got)
	}
	if !observability.RuntimeHoldsNothingForTest(ctx) {
		t.Fatal("runtime kept lineages or sessions open")
	}
}

func TestTurnsBeforeSwitchCountTheModelLeftAcrossRotation(t *testing.T) {
	ctx := observability.DirectRuntimeForTest(t)
	convo := provider.WithPortalConversationContext(ctx, "s1")
	observability.EngagementTurnForTest(convo, "s1", "t1", "cli", 0)
	observability.EngagementTurnForTest(convo, "s1c", "t2", "cli", 0)
	observability.EngagementTurnForTest(convo, "s1c", "t3", "cli", 0)
	observability.RecordModelSwitch(ctx, "openrouter", "openrouter", "cli", engagementPublic, "s1c")
	observability.RecordModelSwitch(ctx, "openrouter", "openrouter", "cli", "openai/gpt-5", "s1c")
	want := []string{"model=" + engagementPublic + ",provider=openrouter,turns_before_switch_bucket=2_to_3|1"}
	if got := observability.StoredRowsForTest(t, ctx, observability.ModelSwitchAfterMetric); !slices.Equal(got, want) {
		t.Fatalf("switch after %v", got)
	}
}
