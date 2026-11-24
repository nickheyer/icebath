package gatewayapi

import (
	"context"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Completed, partial, failed and error flags of an agent result, the defaults for a non dict
func resultFlags(result map[string]any) (bool, bool, bool, any) {
	if result == nil {
		return true, false, false, nil
	}
	completed := true
	if v, ok := result["completed"]; ok {
		completed = anyutil.IsTrue(v)
	}
	completed = completed && !anyutil.IsTrue(result["interrupted"])
	return completed, anyutil.IsTrue(result["partial"]), anyutil.IsTrue(result["failed"]), result["error"]
}

// Whether the turn ran out of its iteration budget
func budgetExhausted(result map[string]any) bool {
	reason := ""
	if result != nil && anyutil.IsTrue(result["turn_exit_reason"]) {
		reason = anyutil.String(result["turn_exit_reason"])
	}
	return strings.HasPrefix(reason, "budget_exhausted") || strings.HasPrefix(reason, "max_iterations_reached(")
}

// OpenAI finish reason, length for truncation or a spent budget, error for failure, cancelled or stop otherwise
func finishReason(completed, isPartial, isFailed bool, errMsg any, agentError error, exhausted bool) string {
	errText := ""
	if anyutil.IsTrue(errMsg) {
		errText = anyutil.String(errMsg)
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
func settled(record *jsonutil.Object) bool {
	return record != nil && record.ValueOr("response", nil) != nil
}

// Fingerprint keys of a responses body, truncation joining only when sent
func responsesFingerprintKeysFor(body *jsonutil.Object) []string {
	keys := append([]string(nil), responsesFingerprintKeys...)
	if body.ValueOr("truncation", nil) != nil {
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
func matchingReplay(ctx context.Context, store *gateway.ResponseStore, key, binding string) (bool, *jsonutil.Object, error) {
	matches, err := store.RequestKeyMatches(ctx, key, binding)
	if err != nil || !matches {
		return false, nil, err
	}
	record, err := store.Get(ctx, key)
	if err != nil {
		return true, nil, err
	}
	obj, _ := record.(*jsonutil.Object)
	return true, obj, nil
}

// Response snapshot with its id and body
type responseSnapshot struct {
	id   string
	data *jsonutil.Object
}

// Non streaming responses persistence as one store unit, body then conversation pointer then replay record
func storeNonstreamResponse(ctx context.Context, store *gateway.ResponseStore, snapshot *responseSnapshot, replayKey string, replayRecord *jsonutil.Object, conversation string, terminalReplay bool) error {
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
	case anyutil.IsTrue(result["interrupted"]):
		return "cancelled"
	case anyutil.IsTrue(result["failed"]) || anyutil.IsTrue(result["error"]):
		return "failed"
	case result["completed"] == false:
		return "incomplete"
	}
	return "completed"
}

// Icebath completion extras with the error code the finish reason implies
func completionExtras(completed, isPartial, isFailed bool, errMsg any, finish string, exhausted bool) *jsonutil.Object {
	code := "agent_error"
	if finish == "length" {
		code = "output_truncated"
		if exhausted {
			code = "iteration_budget_exhausted"
		}
	}
	return jsonutil.ObjectOf("completed", completed, "partial", isPartial, "failed", isFailed, "error", errMsg, "error_code", code)
}

// Localized notice that the final reply was transformed after streaming
func transformedNotice(ctx context.Context) string {
	return i18n.T(ctx, "platform.api_server.transformed_notice", "", nil)
}

// Text a stream still owes after an output transform, the suffix when it only appended
func postStreamTransform(result map[string]any) (string, bool) {
	if result == nil || !anyutil.IsTrue(result["response_transformed"]) {
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
func messageItem(text any) *jsonutil.Object {
	return jsonutil.ObjectOf("type", "message", "role", "assistant",
		"content", []any{jsonutil.ObjectOf("type", "output_text", "text", text)})
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
		msg, ok := raw.(*jsonutil.Object)
		if !ok {
			out = append(out, raw)
			continue
		}
		role, _ := msg.Get("role")
		if content, isStr := msg.ValueOr("content", nil).(string); role == "tool" && isStr && len([]rune(content)) > maxChars {
			msg = msg.Clone()
			msg.Set("content", capTextKeep(content, maxChars))
		}
		if calls, isList := msg.ValueOr("tool_calls", nil).([]any); role == "assistant" && isList {
			capped := make([]any, 0, len(calls))
			for _, rawCall := range calls {
				call, _ := rawCall.(*jsonutil.Object)
				var fn *jsonutil.Object
				if call != nil {
					fn, _ = call.ValueOr("function", nil).(*jsonutil.Object)
				}
				var argsText string
				var isText bool
				if fn != nil {
					argsText, isText = fn.ValueOr("arguments", nil).(string)
				}
				if isText && len([]rune(argsText)) > maxChars {
					parsed, err := jsonutil.Decode(argsText)
					if args, isObj := parsed.(*jsonutil.Object); err == nil && isObj {
						for _, k := range args.Keys() {
							if v, isStr := args.ValueOr(k, nil).(string); isStr && len([]rune(v)) > maxChars {
								args.Set(k, capTextKeep(v, maxChars))
							}
						}
						encoded, _ := jsonutil.Encode(args)
						newFn := fn.Clone()
						newFn.Set("arguments", encoded)
						newCall := call.Clone()
						newCall.Set("function", newFn)
						rawCall = newCall
					}
				}
				capped = append(capped, rawCall)
			}
			msg = msg.Clone()
			msg.Set("tool_calls", capped)
		}
		out = append(out, msg)
	}
	return out
}

// Completed Responses reasoning output item
func reasoningItem(text string) *jsonutil.Object {
	return jsonutil.ObjectOf("id", "rs_"+hexUUID()[:24], "type", "reasoning", "status", "completed",
		"summary", []any{jsonutil.ObjectOf("type", "summary_text", "text", text)})
}

// Whether an input item is an echoed back reasoning output item
func isReasoningInputItem(item any) bool {
	obj, ok := item.(*jsonutil.Object)
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
		m, _ := jsonutil.AsMap(raw)
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
func trimToolItems(items []*jsonutil.Object) []*jsonutil.Object {
	for _, item := range items {
		switch t, _ := item.Get("type"); t {
		case "function_call":
			func() {
				defer func() { _ = recover() }()
				raw := item.ValueOr("arguments", nil)
				var args any = raw
				if text, isStr := raw.(string); isStr {
					parsed, err := jsonutil.Decode(text)
					if err != nil {
						return
					}
					args = parsed
				} else if raw == nil {
					args = jsonutil.NewObject()
				}
				obj, isObj := args.(*jsonutil.Object)
				if !isObj {
					return
				}
				for _, k := range []string{"content", "query", "pattern", "old_string", "new_string"} {
					if v, isStr := obj.ValueOr(k, nil).(string); isStr && len([]rune(v)) > 500 {
						obj.Set(k, "["+strconv.Itoa(len([]rune(v)))+" chars — truncated for response.completed]")
					}
				}
				encoded, err := jsonutil.Encode(obj)
				if err != nil {
					return
				}
				item.Set("arguments", encoded)
			}()
		case "function_call_output":
			output, isList := item.ValueOr("output", []any{}).([]any)
			if isList && len(output) > 0 {
				if first, isObj := output[0].(*jsonutil.Object); isObj {
					if ft, _ := first.Get("type"); ft == "input_text" {
						text, _ := first.ValueOr("text", "").(string)
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
