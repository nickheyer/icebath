package gatewayapi

import (
	"context"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Completed, partial, failed and error flags of an agent result, the defaults for a non dict
func resultFlags(result map[string]any) (bool, bool, bool, any) {
	if result == nil {
		return true, false, false, nil
	}
	completed := true
	if v, ok := result["completed"]; ok {
		completed = pyrepr.Truthy(v)
	}
	completed = completed && !pyrepr.Truthy(result["interrupted"])
	return completed, pyrepr.Truthy(result["partial"]), pyrepr.Truthy(result["failed"]), result["error"]
}

// Whether the turn ran out of its iteration budget
func budgetExhausted(result map[string]any) bool {
	reason := ""
	if result != nil && pyrepr.Truthy(result["turn_exit_reason"]) {
		reason = pyrepr.Str(result["turn_exit_reason"])
	}
	return strings.HasPrefix(reason, "budget_exhausted") || strings.HasPrefix(reason, "max_iterations_reached(")
}

// OpenAI finish reason, length for truncation or a spent budget, error for failure, cancelled or stop otherwise
func finishReason(completed, isPartial, isFailed bool, errMsg any, agentError error, exhausted bool) string {
	errText := ""
	if pyrepr.Truthy(errMsg) {
		errText = pyrepr.Str(errMsg)
	}
	if isPartial && errText != "" && strings.Contains(strings.ToLower(errText), "truncat") {
		return "length"
	}
	if agentError != nil || isFailed || (!completed && errText != "") {
		return "error"
	}
	if exhausted {
		return "length"
	}
	if !completed {
		return "cancelled"
	}
	return "stop"
}

var responsesFingerprintKeys = []string{"input", "instructions", "previous_response_id", "conversation", "conversation_history",
	"model", "provider", "model_options", "tools"}

// Whether a stored idempotency record carries its answer
func settled(record *pyjson.Object) bool {
	return record != nil && record.GetOr("response", nil) != nil
}

// Fingerprint keys of a responses body, truncation joining only when sent
func responsesFingerprintKeysFor(body *pyjson.Object) []string {
	keys := append([]string(nil), responsesFingerprintKeys...)
	if body.GetOr("truncation", nil) != nil {
		keys = append(keys, "truncation")
	}
	return keys
}

// Response id and creation time, durable for an idempotency key and fresh otherwise
func responseIdentity(ctx context.Context, store *gateway.ResponseStore, durableKey string) (string, int64, error) {
	if durableKey != "" {
		created, err := store.RequestCreatedAt(ctx, durableKey)
		if err != nil {
			return "", 0, err
		}
		var at int64
		if created != nil {
			at = *created
		}
		return gateway.APIDurableResponseID(durableKey), at, nil
	}
	return "resp_" + hexUUID()[:28], int64(apiNow()), nil
}

// Binding match and the stored replay record, read as one store unit
func matchingReplay(ctx context.Context, store *gateway.ResponseStore, key, binding string) (bool, *pyjson.Object, error) {
	matches, err := store.RequestKeyMatches(ctx, key, binding)
	if err != nil || !matches {
		return false, nil, err
	}
	record, err := store.Get(ctx, key)
	if err != nil {
		return true, nil, err
	}
	obj, _ := record.(*pyjson.Object)
	return true, obj, nil
}

// Response snapshot with its id and body
type responseSnapshot struct {
	id   string
	data *pyjson.Object
}

// Non streaming responses persistence as one store unit, body then conversation pointer then replay record
func storeNonstreamResponse(ctx context.Context, store *gateway.ResponseStore, snapshot *responseSnapshot, replayKey string, replayRecord *pyjson.Object, conversation string, terminalReplay bool) error {
	_, err := store.Atomically(ctx, func(ctx context.Context) (any, error) {
		if snapshot != nil {
			advance := conversation != ""
			if advance && terminalReplay {
				current, err := store.GetConversation(ctx, conversation)
				if err != nil {
					return nil, err
				}
				advance = current == "" || current == snapshot.id
			}
			if err := store.Put(ctx, snapshot.id, snapshot.data); err != nil {
				return nil, err
			}
			if advance {
				if err := store.SetConversation(ctx, conversation, snapshot.id); err != nil {
					return nil, err
				}
			}
		}
		if replayRecord != nil {
			if err := store.Put(ctx, replayKey, replayRecord); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	return err
}

// Responses status of a turn result
func responseStatus(result map[string]any) string {
	switch {
	case pyrepr.Truthy(result["interrupted"]):
		return "cancelled"
	case pyrepr.Truthy(result["failed"]) || pyrepr.Truthy(result["error"]):
		return "failed"
	case result["completed"] == false:
		return "incomplete"
	}
	return "completed"
}

// Hermes completion extras with the error code the finish reason implies
func hermesExtras(completed, isPartial, isFailed bool, errMsg any, finish string, exhausted bool) *pyjson.Object {
	code := "agent_error"
	if finish == "length" {
		code = "output_truncated"
		if exhausted {
			code = "iteration_budget_exhausted"
		}
	}
	return pyjson.ObjectOf("completed", completed, "partial", isPartial, "failed", isFailed, "error", errMsg, "error_code", code)
}

// Localized notice that the final reply was transformed after streaming
func transformedNotice(ctx context.Context) string {
	return i18n.T(ctx, "platform.api_server.transformed_notice", "", nil)
}

// Text a stream still owes after an output transform, the suffix when it only appended
func postStreamTransform(result map[string]any) (string, bool) {
	if result == nil || !pyrepr.Truthy(result["response_transformed"]) {
		return "", false
	}
	final, _ := result["final_response"].(string)
	original, _ := result["pre_transform_response"].(string)
	if original != "" && strings.HasPrefix(final, original) {
		return final[len(original):], true
	}
	return final, false
}

// Responses message output item with one output_text part
func messageItem(text any) *pyjson.Object {
	return pyjson.ObjectOf("type", "message", "role", "assistant",
		"content", []any{pyjson.ObjectOf("type", "output_text", "text", text)})
}

// Head of the text plus a marker of how much was cut
func capTextKeep(text string, keep int) string {
	runes := []rune(text)
	return string(runes[:keep]) + "...[" + strconv.Itoa(len(runes)-keep) + " more chars]"
}

// Copy of the history with long tool outputs and string tool call arguments cut down
func capHistoryToolOutputs(history []any, maxChars int) []any {
	if maxChars <= 0 {
		return history
	}
	out := make([]any, 0, len(history))
	for _, raw := range history {
		msg, ok := raw.(*pyjson.Object)
		if !ok {
			out = append(out, raw)
			continue
		}
		role, _ := msg.Get("role")
		if content, isStr := msg.GetOr("content", nil).(string); role == "tool" && isStr && len([]rune(content)) > maxChars {
			msg = msg.Copy()
			msg.Set("content", capTextKeep(content, maxChars))
		}
		if calls, isList := msg.GetOr("tool_calls", nil).([]any); role == "assistant" && isList {
			capped := make([]any, 0, len(calls))
			for _, rawCall := range calls {
				call, _ := rawCall.(*pyjson.Object)
				var fn *pyjson.Object
				if call != nil {
					fn, _ = call.GetOr("function", nil).(*pyjson.Object)
				}
				var argsText string
				var isText bool
				if fn != nil {
					argsText, isText = fn.GetOr("arguments", nil).(string)
				}
				if isText && len([]rune(argsText)) > maxChars {
					parsed, err := pyjson.Loads(argsText)
					if args, isObj := parsed.(*pyjson.Object); err == nil && isObj {
						for _, k := range args.Keys {
							if v, isStr := args.GetOr(k, nil).(string); isStr && len([]rune(v)) > maxChars {
								args.Set(k, capTextKeep(v, maxChars))
							}
						}
						encoded, _ := pyjson.Dumps(args)
						newFn := fn.Copy()
						newFn.Set("arguments", encoded)
						newCall := call.Copy()
						newCall.Set("function", newFn)
						rawCall = newCall
					}
				}
				capped = append(capped, rawCall)
			}
			msg = msg.Copy()
			msg.Set("tool_calls", capped)
		}
		out = append(out, msg)
	}
	return out
}

// Completed Responses reasoning output item
func reasoningItem(text string) *pyjson.Object {
	return pyjson.ObjectOf("id", "rs_"+hexUUID()[:24], "type", "reasoning", "status", "completed",
		"summary", []any{pyjson.ObjectOf("type", "summary_text", "text", text)})
}

// Whether an input item is an echoed back reasoning output item
func isReasoningInputItem(item any) bool {
	obj, ok := item.(*pyjson.Object)
	if !ok {
		return false
	}
	t, _ := obj.Get("type")
	return t == "reasoning"
}

// Reasoning the model produced on this turn joined from the persisted assistant messages
func turnReasoningText(history []any, userMessage any, result map[string]any) string {
	messages, ok := result["messages"].([]any)
	if !ok {
		return ""
	}
	start := responseMessagesTurnStartIndex(history, userMessage, result)
	var parts []string
	for _, raw := range messages[min(start, len(messages)):] {
		m := asMap(raw)
		if m == nil || m["role"] != "assistant" {
			continue
		}
		if r, isStr := m["reasoning"].(string); isStr && strings.TrimSpace(r) != "" {
			parts = append(parts, r)
		}
	}
	return strings.Join(parts, "\n\n")
}

// Trims large tool payloads in place so the completed envelope stays small
func trimToolItems(items []*pyjson.Object) []*pyjson.Object {
	for _, item := range items {
		switch t, _ := item.Get("type"); t {
		case "function_call":
			func() {
				defer func() { _ = recover() }()
				raw := item.GetOr("arguments", nil)
				var args any = raw
				if text, isStr := raw.(string); isStr {
					parsed, err := pyjson.Loads(text)
					if err != nil {
						return
					}
					args = parsed
				} else if raw == nil {
					args = pyjson.NewObject()
				}
				obj, isObj := args.(*pyjson.Object)
				if !isObj {
					return
				}
				for _, k := range []string{"content", "query", "pattern", "old_string", "new_string"} {
					if v, isStr := obj.GetOr(k, nil).(string); isStr && len([]rune(v)) > 500 {
						obj.Set(k, "["+strconv.Itoa(len([]rune(v)))+" chars — truncated for response.completed]")
					}
				}
				encoded, err := pyjson.Dumps(obj)
				if err != nil {
					return
				}
				item.Set("arguments", encoded)
			}()
		case "function_call_output":
			output, isList := item.GetOr("output", []any{}).([]any)
			if isList && len(output) > 0 {
				if first, isObj := output[0].(*pyjson.Object); isObj {
					if ft, _ := first.Get("type"); ft == "input_text" {
						text, _ := first.GetOr("text", "").(string)
						if len([]rune(text)) > 1000 {
							first.Set("text", capTextKeep(text, 500))
							item.Set("output", []any{first})
						}
					}
				}
			}
		}
	}
	return items
}

// Map view of a message in either representation, nil otherwise
func asMap(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case *pyjson.Object:
		return m.Vals
	}
	return nil
}
