package browser

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const dialogDescription = "Accept or dismiss a native JS dialog (alert/confirm/prompt) blocking the page.\n\n" +
	"**Workflow:** when a page is blocked by a native dialog the CDP " +
	"supervisor captured, call this tool with ``action='accept'`` or " +
	"``action='dismiss'``.\n\n" +
	"**Prompt dialogs:** pass ``prompt_text`` to supply the response " +
	"string. Ignored for alert/confirm/beforeunload.\n\n" +
	"**Multiple dialogs:** if more than one dialog is queued (rare — " +
	"happens when a second dialog fires while the first is still open), " +
	"pass ``dialog_id`` (the error lists the candidates) to disambiguate.\n\n" +
	"**Availability:** only present when a CDP endpoint is attached — " +
	"a local Chromium-family browser via ``/browser connect``, or " +
	"``browser.cdp_url`` in config.yaml."

// Static browser_dialog schema
func BrowserDialogSchema() map[string]any {
	return map[string]any{
		"name":        "browser_dialog",
		"description": dialogDescription,
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{"type": "string", "enum": []any{"accept", "dismiss"},
					"description": "'accept' clicks OK / returns the prompt text. " +
						"'dismiss' clicks Cancel / returns null from prompt(). " +
						"For ``beforeunload`` dialogs: 'accept' allows the " +
						"navigation, 'dismiss' keeps the page."},
				"prompt_text": map[string]any{"type": "string", "description": "Response string for a ``prompt()`` dialog. Ignored for " +
					"other dialog types. Defaults to empty string."},
				"dialog_id": map[string]any{"type": "string", "description": "Specific dialog to respond to, from " +
					"the candidate ids a previous call listed. Required " +
					"only when multiple dialogs are queued."},
			},
			"required": []any{"action"},
		},
	}
}

func dumpsASCII(v any) string {
	text, err := pyjson.Dumps(v)
	if err != nil {
		return tools.ErrorResult(err)
	}
	return text
}

// Answers a pending dialog on the task's CDP supervisor
func browserDialog(action, promptText, dialogID any, taskID string) string {
	if taskID == "" {
		taskID = "default"
	}
	sup := supervisors.get(taskID)
	if sup == nil {
		return dumpsASCII(pyjson.ObjectOf("success", false, "error", "No CDP supervisor is attached to this task: no browser "+
			"session has been started yet. Call browser_exec or "+
			"/browser connect first."))
	}
	actionName, isStr := action.(string)
	var res *pyjson.Object
	if !isStr {
		res = supervisorFail("action must be 'accept' or 'dismiss', got " + pyrepr.Repr(action))
	} else {
		var prompt *string
		if promptText != nil {
			text := pyrepr.Str(promptText)
			prompt = &text
		}
		id := ""
		if pyrepr.Truthy(dialogID) {
			id = pyrepr.Str(dialogID)
		}
		res = sup.respondToDialog(actionName, prompt, id, 10.0)
	}
	if pyrepr.Truthy(anyGet(res, "ok")) {
		dialog, ok := res.Get("dialog")
		if !ok {
			dialog = pyjson.NewObject()
		}
		return dumpsASCII(pyjson.ObjectOf("success", true, "action", action, "dialog", dialog))
	}
	errVal, ok := res.Get("error")
	if !ok {
		errVal = "unknown error"
	}
	return dumpsASCII(pyjson.ObjectOf("success", false, "error", errVal))
}

// Same gate as browser_cdp so the two tools appear together
func browserDialogCheck(env *tools.Env) bool { return browserCDPCheck(env) }

func handleBrowserDialog(_ context.Context, call *tools.Call) string {
	action, ok := call.Args["action"]
	if !ok {
		action = ""
	}
	return browserDialog(action, call.Args["prompt_text"], call.Args["dialog_id"], call.TaskID)
}

func registerBrowserDialogTool() {
	tools.Register(tools.Def{Name: "browser_dialog", Toolset: "browser-cdp", Schema: BrowserDialogSchema(), Handler: handleBrowserDialog,
		Check: browserDialogCheck, Emoji: "\U0001F4AC"})
}
