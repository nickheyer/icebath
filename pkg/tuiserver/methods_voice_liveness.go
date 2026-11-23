package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Cheapest liveness probe answering pong, so a client can tell a half open socket while agents are mid turn
func ping(_ context.Context, rid any, _ map[string]any) map[string]any {
	return ok(rid, tuigateway.Plain(tuigateway.PingResult{Pong: true}))
}
