package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Agent usage stats
func usageStats(ctx context.Context, a *agent.AIAgent) *jsonutil.Object {
	return agent.UsageStats(ctx, a)
}

// Publishes one session usage frame on the viewer stream and records it on the execution result at turn end
func publishUsage(ctx context.Context, t *TurnRunner, a *agent.AIAgent, record bool) {
	if a == nil {
		return
	}
	defer func() { _ = recover() }()
	stats := usageStats(ctx, a)
	frame, _ := jsonutil.Plain(gateway.UsageFrame(stats)).(map[string]any)
	t.publishExecution("session.usage", frame)
	if record {
		if captured := gateway.ExecutionResultOf(ctx); captured != nil {
			captured.Set("session_usage", stats)
		}
	}
}
