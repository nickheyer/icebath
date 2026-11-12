package agent

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tools"
)

func init() {
	tools.RegisterInlineToolExecutor("memory", memoryInlineExecutor)
}

// Writes through the agent memory store and mirrors committed writes to the external providers with agent provenance
func memoryInlineExecutor(ctx context.Context, host tools.ExecutorHost, args map[string]any, ictx tools.InlineToolContext) (string, error) {
	h, ok := host.(agentExecutorHost)
	if !ok || h.AIAgent == nil {
		return "", fmt.Errorf("the memory tool runs on an agent executor, not %T", host)
	}
	a := h.AIAgent
	ctx = tools.WithEnvHome(ctx, a.env)
	result := memory.MemoryTool(ctx, a.env, a.memoryStore, args["action"], args["target"], args["content"], args["old_text"], args["new_text"], args["operations"])
	if a.memoryManager != nil {
		a.memoryManager.NotifyMemoryToolWriteContext(ctx, result, args, func() map[string]any {
			return buildMemoryWriteMetadata(a, "", "", ictx.TaskID, ictx.ToolCallID)
		})
	}
	return result, nil
}
