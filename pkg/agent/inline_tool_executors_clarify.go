package agent

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/tools"
)

func init() {
	tools.RegisterInlineToolExecutor("clarify", clarifyInlineExecutor)
}

// Clarify through the agent's clarify callback, the session asker answering when the agent carries none
func clarifyInlineExecutor(ctx context.Context, host tools.ExecutorHost, args map[string]any, _ tools.InlineToolContext) (string, error) {
	var callback any
	if h, ok := host.(agentExecutorHost); ok && h.AIAgent != nil {
		callback = h.clarifyCallback
	}
	switch cb := callback.(type) {
	case nil:
		return tools.ClarifyTool(ctx, host.Env(), args["questions"]), nil
	case tools.ClarifyCallback:
		return tools.ClarifyToolWithCallback(ctx, host.Env(), args["questions"], cb), nil
	case func(context.Context, []any) (map[string]any, error):
		return tools.ClarifyToolWithCallback(ctx, host.Env(), args["questions"], cb), nil
	}
	return "", fmt.Errorf("clarify_callback is a %T, not a clarify callback", callback)
}
