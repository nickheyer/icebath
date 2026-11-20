package gatewayrun

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Completion event of one finished async delegation with a single goal
func delegationCompletion(goal string) map[string]any {
	return map[string]any{"type": "async_delegation", "delegation_id": "deleg_1", "parent_session_id": "s1",
		"goals": []any{goal}, "results": []any{map[string]any{"task_index": 0, "status": "completed", "summary": "Two nits.",
			"api_calls": 3, "duration_seconds": 12}}}
}

// Card the gateway publishes at the claim of a local completion built from the producer metadata
func claimCard(t *testing.T, evt map[string]any, text string) *pyjson.Object {
	t.Helper()
	metadata := watchNotificationMetadata(evt, &gateway.SessionSource{Platform: gateway.PlatformLocal, ChatID: "s1"})
	display, err := gateway.AutomationDisplayMetadata(func(key string) (any, bool) {
		value, ok := metadata[key]
		return value, ok
	})
	if err != nil {
		t.Fatalf("display metadata refused: %v", err)
	}
	descriptor := pyjson.ObjectOf("identity", "i", "owner", "s1", "route", "r", "target", "s1")
	descriptor.Update(display)
	return gateway.AutomationAdmissionDisplay(pyjson.ObjectOf("text", text, "local_automation_v1", descriptor))
}

func TestALongSubagentGoalStillAdmitsItsCompletionCard(t *testing.T) {
	card := claimCard(t, delegationCompletion(strings.Repeat("audit ", 1000)), "model text")
	if kind, _ := card.Get("display_kind"); kind != "async_delegation_complete" {
		t.Fatalf("card %s", picksDump(card))
	}
	if title, _ := card.Get("display_text"); !strings.HasPrefix(title.(string), "Subagent Task Completed: audit") {
		t.Fatalf("title %q", title)
	}
	review := claimCard(t, delegationCompletion("Review the work just discussed"), "review text")
	want := pyjson.ObjectOf("display_kind", "async_delegation_complete",
		"display_text", "Subagent Task Completed: Review the work just discussed", "text", "review text")
	if got := picksDump(review); got != picksDump(want) {
		t.Fatalf("review card %s", got)
	}
}
