package tools

import (
	"context"
)

// Runs one inline or registry tool through every pipeline step the options do not skip
func (e *Executor) InvokeTool(ctx context.Context, name string, args map[string]any, taskID, toolCallID string, opts InvokeToolOptions) (string, error) {
	return e.invokeTool(ctx, name, args, "", taskID, toolCallID, opts)
}

// Task list tool over the given store with the list and merge args of one call
func RunTaskList(store *TodoStore, args map[string]any) string {
	return todoTool(store, args["todos"], argB(args, "merge", false))
}
