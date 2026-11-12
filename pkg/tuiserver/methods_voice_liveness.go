package tuiserver

import "context"

var voiceLivenessRegistry = &handlerRegistry{}

func init() {
	voiceLivenessRegistry.method("ping", ping)
	voiceLivenessRegistry.install()
}

// Cheapest liveness probe answering pong, so a client can tell a half open socket while agents are mid turn
func ping(_ context.Context, rid any, _ map[string]any) map[string]any {
	return ok(rid, map[string]any{"pong": true})
}
