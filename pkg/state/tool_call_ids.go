package state

import "strings"

// Field of a tool call entry that may be a dict or a struct
func ToolCallField(tc any, key string) any { return partField(tc, key) }

// Every wire spelling of one tool call identifier including each part of a bridge id
func expandToolIDVariants(values ...any) map[string]bool {
	variants := map[string]bool{}
	for _, raw := range values {
		s, _ := raw.(string)
		value := pyStrip(s)
		if value == "" {
			continue
		}
		variants[value] = true
		for _, part := range strings.Split(value, "|") {
			if p := pyStrip(part); p != "" {
				variants[p] = true
			}
		}
	}
	return variants
}

// All pairing id variants a tool call entry carries
func ToolCallIDVariants(tc any) map[string]bool {
	return expandToolIDVariants(ToolCallField(tc, "call_id"), ToolCallField(tc, "id"), ToolCallField(tc, "response_item_id"))
}

// All matching variants of a tool result tool_call_id
func ToolResultIDVariants(toolCallID any) map[string]bool {
	return expandToolIDVariants(toolCallID)
}
