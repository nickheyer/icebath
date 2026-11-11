package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Emits tool.complete with the prepared preview, summary and todo snapshot, then repaints changed automation state
func onToolComplete(ctx context.Context, sid, toolCallID, name string, args map[string]any, result string) {
	session := tuigateway.SessionRecord(sid)
	var metadata map[string]any
	prepared := false
	if session != nil {
		toolSlotsMu.Lock()
		slot := toolSlot[map[string]any](session, "tool_result_metadata")
		metadata, prepared = slot[toolCallID]
		delete(slot, toolCallID)
		toolSlotsMu.Unlock()
	}
	if connectorLifecycleIsStale(ctx, sid, name, args) {
		return
	}
	payload := map[string]any{"tool_id": toolCallID, "name": name, "args": args}
	if labels := toolLabels(ctx, name, args); labels != nil {
		payload["labels"] = labels
	}
	if !prepared {
		metadata, _ = prepareToolResultMetadata(ctx, sid, toolCallID, name, args, result)["tool_result_metadata"].(map[string]any)
		if session != nil {
			toolSlotsMu.Lock()
			delete(toolSlot[map[string]any](session, "tool_result_metadata"), toolCallID)
			toolSlotsMu.Unlock()
		}
	}
	for key, value := range metadata {
		payload[key] = value
	}
	var durationS *float64
	if session != nil {
		toolSlotsMu.Lock()
		started := toolSlot[float64](session, "tool_started_at")
		startedAt, ok := started[toolCallID]
		delete(started, toolCallID)
		toolSlotsMu.Unlock()
		if ok && startedAt != 0 {
			elapsed := nowSeconds() - startedAt
			durationS = &elapsed
			payload["duration_s"] = elapsed
		}
	}
	if decoded, err := pyjson.Loads(result); err == nil {
		payload["result"] = decoded
	} else {
		payload["result"] = result
	}
	if summary := toolSummary(name, result, durationS); summary != "" {
		payload["summary"] = summary
	}
	if sessionVerbose(sid) {
		if text := tuigateway.ToolResultText(result); text != "" {
			payload["result_text"] = text
		}
	}
	var todoState *pyjson.Object
	if tools.IsTodoToolName(name) {
		todoState = tuigateway.NormalizeTodoState(payload["result"])
	}
	if todoState != nil {
		for _, key := range todoState.Keys {
			payload[key] = todoState.GetOr(key, nil)
		}
		if session != nil {
			cacheTodoState(session, todoState)
		}
	}
	if toolProgressEnabled(sid) || truthy(payload["inline_diff"]) || toolLifecycleRequiredForUI(name) ||
		tools.IsTodoToolName(name) || connectorToolLifecycle(name, args) ||
		(toolResultNeedsUser(result) && !tools.IsNeutralFailure(name, result)) {
		emitToolLifecycle(ctx, "tool.complete", sid, name, args, payload)
	}
	if todoState != nil {
		emitEvent(ctx, "todo.updated", sid, objectMap(todoState))
	}
	if session != nil {
		publishSessionControlSnapshot(ctx, sid, session, false, true)
	}
}
