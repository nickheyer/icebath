package tools

import (
	"context"
	"strings"
)

// CloseTerminalTool asks the desktop GUI to close a background process's read only tab
func CloseTerminalTool(env *Env, processID string) string {
	pid := strings.TrimSpace(processID)
	if pid == "" {
		return toolErrorJSON("process_id is required (the background process whose tab to close).", nil)
	}
	return JSON(terminalRegistry().RequestCloseTerminal(env, pid))
}

var closeTerminalSchema = map[string]any{
	"name": "close_terminal",
	"description": "Hide a background process's desktop terminal tab; the process keeps running. " +
		"Tabs mirror terminal(background=true) runs. " +
		"This does NOT kill the process \u2014 it only drops the tab/view; the output " +
		"keeps buffering and the user can reopen it from the status stack. Use it " +
		"to tidy up when a background process's live terminal is no longer worth " +
		"showing. To actually stop the process, use process(action='kill') instead.",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"process_id": map[string]any{
				"type": "string",
				"description": "The background process's session id (from terminal(background=true) " +
					"output or process(action='list')) whose tab should be closed.",
			},
		},
		"required": []any{"process_id"},
	},
}

func init() {
	Register(Def{
		Name:    "close_terminal",
		Toolset: "desktop_ui",
		Schema:  closeTerminalSchema,
		Handler: func(ctx context.Context, call *Call) string {
			id, _ := call.Args["process_id"].(string)
			return CloseTerminalTool(call.Env, id)
		},
		Emoji: "\U0001F5A5\ufe0f",
	})
}
