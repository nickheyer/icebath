package tools

import (
	"context"

	"github.com/nickheyer/icebath/pkg/state"
)

// Runs one call past its middleware the way a concurrent batch worker executes it
func (e *Executor) InvokeTool(ctx context.Context, name string, args map[string]any, taskID, toolCallID string, messages *[]*state.Message) (string, error) {
	return e.invokeTool(ctx, name, args, "", taskID, toolCallID, messages, nil)
}

// Task list tool over the given store with the list and merge args of one call
func RunTaskList(store *TodoStore, args map[string]any) string {
	return todoTool(store, args["todos"], argB(args, "merge", false))
}
