package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Agent usage stats with the window plan gauge when the agent tracks one
func usageStats(ctx context.Context, a *agent.AIAgent) *pyjson.Object {
	stats := agent.UsageStats(ctx, a)
	if a == nil {
		return stats
	}
	if capture := a.CreditsCapture(); capture != nil {
		if gauge := gateway.PlanGauge(capture.Credits()); gauge != nil {
			stats.Set("plan_gauge", gauge)
		}
	}
	return stats
}

// Publishes one session usage frame on the viewer stream and records it on the execution result at turn end
func publishUsage(ctx context.Context, t *TurnRunner, a *agent.AIAgent, record bool) {
	if a == nil {
		return
	}
	defer func() { _ = recover() }()
	stats := usageStats(ctx, a)
	frame, _ := pyjson.Plain(gateway.UsageFrame(stats)).(map[string]any)
	t.publishExecution("session.usage", frame)
	if record {
		if captured := gateway.ExecutionResultOf(ctx); captured != nil {
			captured.Set("session_usage", stats)
		}
	}
}
