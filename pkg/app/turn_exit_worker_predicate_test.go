package app

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/kanban"
)

func TestExitMappingComposesWithTheStrippedWorkerPredicate(t *testing.T) {
	failed := map[string]any{"failed": true, "failure_retryable": true, "failure_reason": "rate_limit", "error": "peer closed connection",
		"final_response": "API call failed after 3 retries: peer closed connection", "completed": false, "messages": []any{}}
	t.Setenv("ICEBATH_KANBAN_TASK", "   ")
	if got := singleQueryExitCode(failed, false, false); got != 1 {
		t.Fatalf("blank worker id mapped a quota wall to %d", got)
	}
	t.Setenv("ICEBATH_KANBAN_TASK", "  t_probe  ")
	if got := singleQueryExitCode(failed, false, false); got != kanban.KanbanRateLimitExitCode {
		t.Fatalf("worker quota wall mapped to %d", got)
	}
}
