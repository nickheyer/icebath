package acp

import (
	"context"
	"iter"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Client side calls the agent makes over its connection
type Client interface {
	SessionUpdater
	RequestPermission(ctx context.Context, options []pyacp.PermissionOption, sessionID string, toolCall *pyacp.ToolCallUpdate, meta *pyjson.Object) (*pyacp.RequestPermissionResponse, error)
}

// Agent seam that takes the callback fed with reasoning text during a prompt
type ReasoningCallbackSetter interface {
	SetReasoningCallback(cb any)
}

// Python dict like value of a history message, keys in order
func historyDict(v any) (*pyjson.Object, bool) {
	switch x := v.(type) {
	case *pyjson.Object:
		return x, x != nil
	case map[string]any:
		o := pyjson.NewObject()
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			o.Set(k, historyValue(x[k]))
		}
		return o, true
	case interface{ PyItems() iter.Seq2[any, any] }:
		o := pyjson.NewObject()
		for k, val := range x.PyItems() {
			o.Set(pyStr(k), historyValue(val))
		}
		return o, true
	}
	return nil, false
}

// Python list like value of a history message
func historyList(v any) ([]any, bool) {
	switch x := v.(type) {
	case []any:
		return x, true
	case []map[string]any:
		out := make([]any, len(x))
		for i, m := range x {
			out[i] = m
		}
		return out, true
	}
	return nil, false
}

// History value with every nested dict as an ordered object and every list as a plain list
func historyValue(v any) any {
	if d, ok := historyDict(v); ok {
		return d
	}
	if l, ok := historyList(v); ok {
		out := make([]any, len(l))
		for i, item := range l {
			out[i] = historyValue(item)
		}
		return out
	}
	return v
}

// Persisted content or reasoning as one stripped string, empty when there is nothing to emit
func flattenHistoryText(value any) string {
	if s, ok := value.(string); ok {
		return pystr.Strip(s)
	}
	items, ok := historyList(value)
	if !ok {
		return ""
	}
	var parts []string
	for _, item := range items {
		if d, ok := historyDict(item); ok {
			if text, ok := d.GetOr("text", nil).(string); ok {
				parts = append(parts, text)
			} else if content, ok := d.GetOr("content", nil).(string); ok && d.GetOr("type", nil) == "text" {
				parts = append(parts, content)
			}
		} else if s, ok := item.(string); ok {
			parts = append(parts, s)
		}
	}
	var kept []string
	for _, part := range parts {
		if stripped := pystr.Strip(part); stripped != "" {
			kept = append(kept, stripped)
		}
	}
	return pystr.Strip(strings.Join(kept, "\n"))
}

// First non empty of reasoning_content and reasoning
func historyReasoningText(message map[string]any) string {
	for _, key := range []string{"reasoning_content", "reasoning"} {
		if text := flattenHistoryText(message[key]); text != "" {
			return text
		}
	}
	return ""
}

// Meta of a replayed compaction summary, nil for ordinary content
func historySummaryMeta(message map[string]any, text string) *pyjson.Object {
	kind := state.ClassifySummaryContent(text)
	if kind == "" && truthy(message[state.CompressedSummaryMetadataKey]) {
		kind = "standalone"
	}
	key := map[string]string{"standalone": "compactionSummary", "merged": "containsCompactionSummary"}[kind]
	if key == "" {
		return nil
	}
	inner := pyjson.NewObject()
	inner.Set(key, true)
	meta := pyjson.NewObject()
	meta.Set("icebath", inner)
	return meta
}

// Function name and arguments of an OpenAI style tool call
func historyToolCallNameArgs(toolCall *pyjson.Object) (string, *pyjson.Object) {
	function, ok := historyDict(toolCall.GetOr("function", nil))
	if !ok {
		function = pyjson.NewObject()
	}
	name := "unknown_tool"
	for _, candidate := range []any{function.GetOr("name", nil), toolCall.GetOr("name", nil)} {
		if truthy(candidate) {
			name = pyStr(candidate)
			break
		}
	}
	var raw any = pyjson.NewObject()
	for _, candidate := range []any{function.GetOr("arguments", nil), toolCall.GetOr("arguments", nil), toolCall.GetOr("args", nil)} {
		if truthy(candidate) {
			raw = candidate
			break
		}
	}
	return name, CoerceToolArgs(raw)
}

// User or assistant chunk of a history message, nil when it has no text
func historyMessageChunk(role string, message map[string]any) pyacp.SessionUpdate {
	text := flattenHistoryText(message["content"])
	if text == "" {
		return nil
	}
	meta := historySummaryMeta(message, text)
	if role == "user" {
		return &pyacp.UserMessageChunk{Content: pyacp.TextBlock(text), Meta: meta}
	}
	return &pyacp.AgentMessageChunk{Content: pyacp.TextBlock(text), Meta: meta}
}

type replayedCall struct {
	name string
	args *pyjson.Object
}

// Session updates that reconstruct a persisted transcript in order, text, thoughts and tool call pairs
func historyReplayUpdates(history []map[string]any) []pyacp.SessionUpdate {
	var updates []pyacp.SessionUpdate
	active := map[string]replayedCall{}
	for _, message := range history {
		role := ""
		if truthy(message["role"]) {
			role = pyStr(message["role"])
		}
		switch role {
		case "user":
			if chunk := historyMessageChunk(role, message); chunk != nil {
				updates = append(updates, chunk)
			}
		case "assistant":
			if thought := historyReasoningText(message); thought != "" {
				updates = append(updates, pyacp.UpdateAgentThoughtText(thought))
			}
			if chunk := historyMessageChunk(role, message); chunk != nil {
				updates = append(updates, chunk)
			}
			toolCalls, _ := historyList(message["tool_calls"])
			for _, raw := range toolCalls {
				toolCall, ok := historyDict(raw)
				if !ok {
					continue
				}
				id := ""
				for _, key := range []string{"id", "call_id", "tool_call_id"} {
					if v := toolCall.GetOr(key, nil); truthy(v) {
						id = pystr.Strip(pyStr(v))
						break
					}
				}
				if id == "" {
					continue
				}
				name, args := historyToolCallNameArgs(toolCall)
				active[id] = replayedCall{name, args}
				updates = append(updates, BuildToolStart(id, name, args, nil))
			}
		case "tool":
			id, name := "", ""
			if truthy(message["tool_call_id"]) {
				id = pystr.Strip(pyStr(message["tool_call_id"]))
			}
			if truthy(message["tool_name"]) {
				name = pystr.Strip(pyStr(message["tool_name"]))
			}
			var args *pyjson.Object
			if call, ok := active[id]; ok {
				delete(active, id)
				name, args = call.name, call.args
			}
			if id == "" || name == "" {
				continue
			}
			var resultText *string
			if s, ok := message["content"].(string); ok {
				resultText = &s
			}
			updates = append(updates, BuildToolComplete(id, name, resultText, args, nil, false))
			if name == "todo" && resultText != nil {
				if plan := buildPlanUpdateFromTodoResult(*resultText); plan != nil {
					updates = append(updates, plan)
				}
			}
		}
	}
	return updates
}

// Acknowledges only an auth method initialize advertised, nil otherwise
func authenticate(ctx context.Context, methodID string) *pyacp.AuthenticateResponse {
	normalized := pystr.Lower(pystr.Strip(methodID))
	providerName := DetectProvider(ctx)
	if normalized == TerminalSetupAuthMethodID {
		if providerName != "" {
			return &pyacp.AuthenticateResponse{}
		}
		return nil
	}
	if providerName == "" || normalized != providerName {
		return nil
	}
	return &pyacp.AuthenticateResponse{}
}

// Permission request of a connection in the form the approval bridges call
func requestPermissionVia(conn Client) RequestPermissionFunc {
	return func(ctx context.Context, sessionID string, toolCall *pyacp.ToolCallProgress, options []pyacp.PermissionOption) (*pyacp.RequestPermissionResponse, error) {
		update := &pyacp.ToolCallUpdate{Meta: toolCall.Meta, Content: toolCall.Content, Kind: toolCall.Kind, Locations: toolCall.Locations,
			RawInput: toolCall.RawInput, RawOutput: toolCall.RawOutput, Status: toolCall.Status, Title: toolCall.Title, ToolCallID: toolCall.ToolCallID}
		return conn.RequestPermission(ctx, options, sessionID, update, nil)
	}
}
