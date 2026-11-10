package cron

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestIdentityIsScopedAndNotInheritedByDelegatedChildren(t *testing.T) {
	ctx := context.Background()
	record := map[string]any{"source": "builtin", "scheduled_instant": nil, "started_at": "2026-10-04T00:00:00"}
	if CurrentCronExecution(ctx) != nil {
		t.Fatal("identity outside any fire")
	}
	fired := enterCronExecution(ctx, &Job{ID: "j", Name: "J"}, "exec-1", record)
	got := CurrentCronExecution(fired)
	if got == nil || got.ExecutionID != "exec-1" || got.JobName != "J" || got.Source != "builtin" || got.ScheduledInstant != nil || deref(got.StartedAt) != "2026-10-04T00:00:00" {
		t.Fatalf("identity %+v", got)
	}
	if CurrentCronExecution(tools.WithDelegatedChildContext(fired, true)) != nil {
		t.Fatal("a delegated child inherited the cron identity")
	}
	if CurrentCronExecution(ctx) != nil {
		t.Fatal("identity leaked past the fire")
	}
}
