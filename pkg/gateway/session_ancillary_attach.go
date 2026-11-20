package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Ancillary verbs of a connection keyed by method name
func ancillaryHandlers(c *AuthorityConnection) map[string]controlHandler {
	out := map[string]controlHandler{}
	for name, kind := range map[string]string{"session.control.read": "control", "process.list": "processes",
		"subagent.list": "subagents", "subagent.tail": "tail"} {
		out[name] = func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return ancillaryRead(ctx, c, ref, params, kind)
		}
	}
	bind := func(fn func(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error)) controlHandler {
		return func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return fn(ctx, c, ref, params)
		}
	}
	out["process.stop"] = bind(stopProcesses)
	out["process.kill"] = bind(killProcess)
	out["reload.mcp"] = bind(reloadMCP)
	out["tools.show"] = bind(showTools)
	out["tools.configure"] = bind(configureTools)
	out["skills.reload"] = bind(reloadSkills)
	for _, name := range []string{"subagent.steer", "subagent.interrupt"} {
		steer := name == "subagent.steer"
		out[name] = func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return subagentControl(ctx, c, ref, params, steer)
		}
	}
	return out
}
