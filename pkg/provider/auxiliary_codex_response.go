package provider

// Text parts, tool calls, usage, and chat finish reason of a Responses result for chat completions consumers
func auxParseCodexFinalResponse(final map[string]any, issuerKind, issuerModel string) ([]string, []codexToolCall, map[string]any, string, error) {
	var output []any
	switch items := final["output"].(type) {
	case []any:
		output = items
	case []map[string]any:
		for _, item := range items {
			output = append(output, item)
		}
	}
	if len(output) == 0 {
		output = []any{map[string]any{"type": "message", "content": []any{}}}
	}
	normalized := map[string]any{
		"output":             output,
		"output_text":        final["output_text"],
		"status":             final["status"],
		"incomplete_details": final["incomplete_details"],
		"error":              final["error"],
	}
	msg, finish, err := normalizeCodexResponse(normalized, issuerKind, issuerModel, false)
	if err != nil {
		return nil, nil, nil, "", err
	}
	if finish != "tool_calls" && (finish == "incomplete" || lowerOrEmpty(normalized["status"]) == "incomplete") {
		finish = "length"
	}
	var textParts []string
	if msg.Content != "" {
		textParts = []string{msg.Content}
	}
	var usage map[string]any
	if u, ok := final["usage"].(map[string]any); ok && len(u) > 0 {
		field := func(key string) any {
			if v := u[key]; auxTruthy(v) {
				return v
			}
			return 0
		}
		usage = map[string]any{"prompt_tokens": field("input_tokens"), "completion_tokens": field("output_tokens"),
			"total_tokens": field("total_tokens")}
	}
	return textParts, msg.ToolCalls, usage, finish, nil
}
