package acp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/logging"
)

var gatewayReplyLog = logging.GetLogger("acp_adapter.gateway_reply")

// Reply text of a message frame, empty when text is absent or null
func replyText(payload *pyjson.Object) string {
	text, _ := dget(payload, "text").(string)
	return text
}

// Open tool call of a session, its name, arguments and the admission that started it
type heldTool struct {
	name      string
	args      *pyjson.Object
	admission string
}

type toolKey struct{ session, callID string }

// Projects a tool start or completion the way the in process callbacks did
func (a *GatewayACPAgent) projectTool(ctx context.Context, kind, sid, editorID, aid string, payload *pyjson.Object) error {
	toolName := "tool"
	for _, key := range []string{"tool_name", "name"} {
		if v := dget(payload, key); truthy(v) {
			toolName = pyStr(v)
			break
		}
	}
	if !dhas(payload, "tool_call_id") {
		return errors.New("KeyError: 'tool_call_id'")
	}
	callID := pyStr(dget(payload, "tool_call_id"))
	key := toolKey{sid, callID}
	if kind == "tool.start" {
		a.closeSegment(aid)
		a.endMessage(sid)
		args := CoerceToolArgs(dget(payload, "args"))
		a.toolArgs.Set(key, heldTool{toolName, args, aid})
		if a.conn != nil {
			return a.sessionUpdate(ctx, editorID, BuildToolStart(callID, toolName, args, nil))
		}
		return nil
	}
	held, ok := a.toolArgs.Get(key)
	if ok {
		a.toolArgs.Delete(key)
	} else {
		held = heldTool{toolName, pyjson.NewObject(), aid}
	}
	if a.conn != nil {
		var result *string
		if s, ok := dget(payload, "result").(string); ok {
			result = &s
		}
		return a.sessionUpdate(ctx, editorID, BuildToolComplete(callID, held.name, result, held.args, nil, dget(payload, "is_error") == true))
	}
	return nil
}

// Shows the unseen remainder of a settled reply, then closes its message id and the tool calls it left open
func (a *GatewayACPAgent) finishReply(ctx context.Context, sid, editorID, aid string, payload *pyjson.Object) error {
	if remainder := a.finalRemainder(aid, payload); remainder != "" && a.conn != nil {
		if err := a.sessionUpdate(ctx, editorID, a.chunk(sid, remainder, false)); err != nil {
			return err
		}
	}
	a.endMessage(sid)
	return a.flushOpenTools(ctx, sid, editorID, aid)
}

// Closes as failed every call the admission started and never completed
func (a *GatewayACPAgent) flushOpenTools(ctx context.Context, sid, editorID, aid string) error {
	var open []toolKey
	for key, held := range a.toolArgs.All() {
		if key.session == sid && held.admission == aid {
			open = append(open, key)
		}
	}
	for _, key := range open {
		held, _ := a.toolArgs.Get(key)
		a.toolArgs.Delete(key)
		if a.conn != nil {
			if err := a.sessionUpdate(ctx, editorID, BuildToolAbandoned(key.callID, held.name)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Assistant or thought chunk carrying the session's current message id
func (a *GatewayACPAgent) chunk(sid, text string, thought bool) pyacp.SessionUpdate {
	ids, ok := a.messageIDs[sid]
	if !ok {
		ids = &AssistantMessageIDAllocator{}
		a.messageIDs[sid] = ids
	}
	id := ids.Current()
	if thought {
		update := pyacp.UpdateAgentThoughtText(text)
		update.MessageID = &id
		return update
	}
	update := pyacp.UpdateAgentMessageText(text)
	update.MessageID = &id
	return update
}

func (a *GatewayACPAgent) endMessage(sid string) {
	if ids, ok := a.messageIDs[sid]; ok {
		ids.Close()
	}
}

// Projects a reasoning delta as a thought chunk or a task list snapshot as the native plan update
func (a *GatewayACPAgent) projectSideUpdate(ctx context.Context, kind, sid, editorID string, payload *pyjson.Object) error {
	var update pyacp.SessionUpdate
	if kind == "reasoning.delta" {
		if text := replyText(payload); text != "" {
			update = a.chunk(sid, text, true)
		}
	} else if todos, ok := dget(payload, "todos").([]any); ok {
		wrapped := pyjson.NewObject()
		wrapped.Set("todos", todos)
		text, err := pyjson.Dumps(wrapped)
		if err != nil {
			return err
		}
		if plan := buildPlanUpdateFromTodoResult(text); plan != nil {
			update = plan
		}
	}
	if update != nil && a.conn != nil {
		return a.sessionUpdate(ctx, editorID, update)
	}
	return nil
}

// Moves the live stream of an admission into its closed segments
func (a *GatewayACPAgent) closeSegment(aid string) {
	streamed := a.streamed[aid]
	delete(a.streamed, aid)
	if pystr.Strip(streamed) != "" {
		a.segments[aid] = append(a.segments[aid], streamed)
	}
}

// Part of the settled reply the editor has not been shown yet
func (a *GatewayACPAgent) finalRemainder(aid string, payload *pyjson.Object) string {
	text := replyText(payload)
	streamed := a.streamed[aid]
	delete(a.streamed, aid)
	shown := a.segments[aid]
	delete(a.segments, aid)
	cut, hasCut := a.cut[aid]
	delete(a.cut, aid)
	if text == "" || (dget(payload, "outcome") == "cancelled" && strings.HasPrefix(text, agent.InterruptWaitingForModelPrefix)) {
		return ""
	}
	segments := map[string]bool{}
	for _, s := range shown {
		segments[pystr.Strip(s)] = true
	}
	if dget(payload, "response_reused") == true {
		painted := false
		if hasCut || truthy(dget(payload, "receipt")) {
			painted = segments[pystr.Strip(text)] || pystr.Strip(text) == pystr.Strip(streamed)
		} else {
			painted = streamed != "" || len(shown) > 0
		}
		if painted {
			return ""
		}
	}
	if hasCut {
		if cut == "" && segments[pystr.Strip(text)] {
			return ""
		}
		return ReplayGapOwedReply(cut, text)
	}
	if strings.HasPrefix(text, streamed) {
		if streamed == "" && segments[pystr.Strip(text)] {
			return ""
		}
		return text[len(streamed):]
	}
	if pystr.Strip(text) == pystr.Strip(streamed) {
		return ""
	}
	return "\n\n" + text
}

// Closes the pending bubble of an answered permission request, completed for an allow and failed otherwise
func (a *GatewayACPAgent) closeCard(ctx context.Context, editorID string, toolCall *pyacp.ToolCallProgress, options []pyacp.PermissionOption, response *pyacp.RequestPermissionResponse) error {
	status := "failed"
	if permissionAllowed(response, options) {
		status = "completed"
	}
	err := a.sessionUpdate(ctx, editorID, pyacp.UpdateToolCall(toolCall.ToolCallID, pyacp.ToolCallFields{Status: &status}))
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		gatewayReplyLog.Debug(fmt.Sprintf("ACP permission card %s not closed\n%s", toolCall.ToolCallID, err))
	}
	return nil
}
