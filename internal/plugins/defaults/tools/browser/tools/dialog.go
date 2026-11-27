package tools

import (
	"cmp"
	"context"
	"fmt"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/tools"
)

const dialogAbout = "Accept or dismiss a native JS dialog (alert/confirm/prompt) blocking the page.\n\n" +
	"**Workflow:** when a page is blocked by a native dialog the CDP supervisor captured, call this tool with ``action='accept'`` or " +
	"``action='dismiss'``.\n\n" +
	"**Prompt dialogs:** pass ``prompt_text`` to supply the response string. Ignored for alert/confirm/beforeunload.\n\n" +
	"**Multiple dialogs:** if more than one dialog is queued (rare — happens when a second dialog fires while the first is still open), pass " +
	"``dialog_id`` (the error lists the candidates) to disambiguate.\n\n" +
	"**Availability:** only present when a CDP endpoint is attached — a local Chromium-family browser via ``/browser connect``, or " +
	"``browser.cdp_url`` in config.yaml."

func dialogSchema() map[string]any {
	return map[string]any{"description": dialogAbout, "parameters": map[string]any{"type": "object", "required": []string{"action"},
		"properties": map[string]any{
			"action": map[string]any{"type": "string", "enum": []string{"accept", "dismiss"}, "description": "'accept' clicks OK / returns the " +
				"prompt text. 'dismiss' clicks Cancel / returns null from prompt(). For ``beforeunload`` dialogs: 'accept' allows the navigation, " +
				"'dismiss' keeps the page."},
			"prompt_text": tools.Property("string", "Response string for a ``prompt()`` dialog. Ignored for other dialog types. Defaults to empty "+
				"string."),
			"dialog_id": tools.Property("string", "Specific dialog to respond to, from the candidate ids a previous call listed. Required only when "+
				"multiple dialogs are queued."),
		}}}
}

// Answers a pending dialog on the task's supervisor
func (r *registrant) dialog(_ context.Context, call *tools.Call) string {
	s := r.supervisors.get(cmp.Or(call.SessionID, "default"))
	if s == nil {
		return orderedJSON(map[string]any{"success": false, "error": "No CDP supervisor is attached to this task: no browser session has been " +
			"started yet. Call browser_exec or /browser connect first."}, "success", "error")
	}
	action := call.Args["action"]
	var res map[string]any
	if name, ok := action.(string); !ok {
		res = failed(fmt.Sprintf("action must be 'accept' or 'dismiss', got %v", action))
	} else {
		var prompt *string
		if text, ok := call.Args["prompt_text"]; ok && text != nil {
			t := fmt.Sprint(text)
			prompt = &t
		}
		id := ""
		if v := call.Args["dialog_id"]; config.Truthy(v) {
			id = fmt.Sprint(v)
		}
		res = s.answer(name, prompt, id)
	}
	if res["ok"] == true {
		dialog := res["dialog"]
		if dialog == nil {
			dialog = map[string]any{}
		}
		return orderedJSON(map[string]any{"success": true, "action": action, "dialog": dialog}, "success", "action", "dialog")
	}
	return orderedJSON(map[string]any{"success": false, "error": cmp.Or(fmt.Sprint(res["error"]), "unknown error")}, "success", "error")
}
