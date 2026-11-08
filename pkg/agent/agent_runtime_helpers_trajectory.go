package agent

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const trajectorySystemPrompt = "You are a function calling AI model. You are provided with function signatures within <tools> </tools> XML tags. " +
	"You may call one or more functions to assist with the user query. If available tools are not relevant in assisting " +
	"with user query, just respond in natural conversational language. Don't make assumptions about what values to plug " +
	"into functions. After calling & executing the functions, you will be provided with function results within " +
	"<tool_response> </tool_response> XML tags. Here are the available tools:\n" +
	"<tools>\n{tools}\n</tools>\n" +
	"For each function call return a JSON object, with the following pydantic model json schema for each:\n" +
	"{'title': 'FunctionCall', 'type': 'object', 'properties': {'name': {'title': 'Name', 'type': 'string'}, " +
	"'arguments': {'title': 'Arguments', 'type': 'object'}}, 'required': ['name', 'arguments']}\n" +
	"Each function call should be enclosed within <tool_call> </tool_call> XML tags.\n" +
	"Example:\n<tool_call>\n{'name': <function-name>,'arguments': <args-dict>}\n</tool_call>"

// Shallow copy with multimodal content as its text summary and image parts as screenshot text parts
func trajectoryNormalizeMsg(msg map[string]any) map[string]any {
	content, ok := msg["content"]
	if !ok {
		return msg
	}
	if _, isList := listOfAny(content); !isList && !tools.IsMultimodalToolResult(content) {
		return msg
	}
	out := make(map[string]any, len(msg))
	for k, v := range msg {
		out[k] = v
	}
	out["content"] = tools.TrajectoryNormalizeContent(content)
	return out
}

// Value under key, the KeyError Python raises when it is missing
func trajectoryItem(m map[string]any, key string) (any, error) {
	v, ok := m[key]
	if !ok {
		return nil, fmt.Errorf("KeyError: %s", pyStrRepr(key))
	}
	return v, nil
}

// Python str strip of a value, the AttributeError a non str raises
func trajectoryStrip(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("AttributeError: '%s' object has no attribute 'strip'", pyrepr.TypeName(v))
	}
	return pystr.Strip(s), nil
}

// Leading think block from native reasoning tokens, empty without reasoning
func trajectoryGPTPrefix(msg map[string]any) (string, error) {
	reasoning := msg["reasoning"]
	if !pyrepr.Truthy(reasoning) {
		return "", nil
	}
	stripped, err := trajectoryStrip(reasoning)
	if err != nil || stripped == "" {
		return "", err
	}
	return "<think>\n" + reasoning.(string) + "\n</think>\n", nil
}

// Every gpt turn carries a think block, empty when the content has none
func withThinkBlock(content string) string {
	if strings.Contains(content, "<think>") {
		return content
	}
	return "<think>\n</think>\n" + content
}

// The gpt turn of an assistant row with tool calls, each call as a tool_call block
func trajectoryToolCallTurn(msg map[string]any) (string, error) {
	content, err := trajectoryGPTPrefix(msg)
	if err != nil {
		return "", err
	}
	if c := msg["content"]; pyrepr.Truthy(c) {
		stripped, err := trajectoryStrip(c)
		if err != nil {
			return "", err
		}
		if stripped != "" {
			content += ConvertScratchpadToThink(c.(string)) + "\n"
		}
	}
	calls, err := trajectoryItem(msg, "tool_calls")
	if err != nil {
		return "", err
	}
	list, _ := listOfAny(calls)
	for _, raw := range list {
		call, ok := raw.(map[string]any)
		if !ok || len(call) == 0 {
			continue
		}
		fnValue, err := trajectoryItem(call, "function")
		if err != nil {
			return "", err
		}
		fn, ok := fnValue.(map[string]any)
		if !ok {
			return "", fmt.Errorf("TypeError: '%s' object is not subscriptable", pyrepr.TypeName(fnValue))
		}
		rawArgs, err := trajectoryItem(fn, "arguments")
		if err != nil {
			return "", err
		}
		arguments := rawArgs
		if s, isStr := rawArgs.(string); isStr {
			parsed, perr := pyjson.Loads(s)
			if perr != nil {
				runes := []rune(s)
				agentRuntimeHelpersLog().Warn(fmt.Sprintf("Unexpected invalid JSON in trajectory conversion: %s", string(runes[:min(len(runes), 100)])))
				parsed = pyjson.NewObject()
			}
			arguments = parsed
		}
		name, err := trajectoryItem(fn, "name")
		if err != nil {
			return "", err
		}
		payload := pyjson.NewObject()
		payload.Set("name", name)
		payload.Set("arguments", arguments)
		encoded, err := pyjson.Dumps(payload, pyjson.EnsureASCII(false))
		if err != nil {
			return "", err
		}
		content += "<tool_call>\n" + encoded + "\n</tool_call>\n"
	}
	return pystr.Rstrip(withThinkBlock(content)), nil
}

// The tool_response blocks of the tool run starting at start and the index after it
func trajectoryToolResponses(msg map[string]any, messages []map[string]any, start int) ([]string, int, error) {
	var responses []string
	calls, _ := listOfAny(msg["tool_calls"])
	j := start
	for j < len(messages) {
		role, err := trajectoryItem(messages[j], "role")
		if err != nil {
			return nil, 0, err
		}
		if role != "tool" {
			break
		}
		toolMsg := messages[j]
		toolContent, err := trajectoryItem(toolMsg, "content")
		if err != nil {
			return nil, 0, err
		}
		if s, ok := toolContent.(string); ok {
			if trimmed := pystr.Strip(s); strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
				if parsed, perr := pyjson.Loads(s); perr == nil {
					toolContent = parsed
				}
			}
		}
		name := any("unknown")
		if idx := len(responses); idx < len(calls) {
			call, ok := calls[idx].(map[string]any)
			if !ok {
				return nil, 0, fmt.Errorf("TypeError: '%s' object is not subscriptable", pyrepr.TypeName(calls[idx]))
			}
			fnValue, err := trajectoryItem(call, "function")
			if err != nil {
				return nil, 0, err
			}
			fn, ok := fnValue.(map[string]any)
			if !ok {
				return nil, 0, fmt.Errorf("TypeError: '%s' object is not subscriptable", pyrepr.TypeName(fnValue))
			}
			if name, err = trajectoryItem(fn, "name"); err != nil {
				return nil, 0, err
			}
		}
		toolCallID, ok := toolMsg["tool_call_id"]
		if !ok {
			toolCallID = ""
		}
		payload := pyjson.NewObject()
		payload.Set("tool_call_id", toolCallID)
		payload.Set("name", name)
		payload.Set("content", toolContent)
		encoded, err := pyjson.Dumps(payload, pyjson.EnsureASCII(false))
		if err != nil {
			return nil, 0, err
		}
		responses = append(responses, "<tool_response>\n"+encoded+"\n</tool_response>")
		j++
	}
	return responses, j, nil
}

// One trajectory turn in from and value order
func trajectoryTurn(from string, value any) *pyjson.Object {
	turn := pyjson.NewObject()
	turn.Set("from", from)
	turn.Set("value", value)
	return turn
}

// Internal history as the ShareGPT trajectory saved for training, text only
func (a *AIAgent) convertToTrajectoryFormat(messages []map[string]any, userQuery string) ([]any, error) {
	normalized := make([]map[string]any, len(messages))
	for i, m := range messages {
		normalized[i] = trajectoryNormalizeMsg(m)
	}
	toolsText, err := FormatToolsForSystemMessage(a)
	if err != nil {
		return nil, err
	}
	trajectory := []any{
		trajectoryTurn("system", strings.Replace(trajectorySystemPrompt, "{tools}", toolsText, 1)),
		trajectoryTurn("human", userQuery),
	}
	for i := 1; i < len(normalized); i++ {
		msg := normalized[i]
		role, err := trajectoryItem(msg, "role")
		if err != nil {
			return nil, err
		}
		switch role {
		case "assistant":
			if pyrepr.Truthy(msg["tool_calls"]) {
				turn, err := trajectoryToolCallTurn(msg)
				if err != nil {
					return nil, err
				}
				trajectory = append(trajectory, trajectoryTurn("gpt", turn))
				responses, j, err := trajectoryToolResponses(msg, normalized, i+1)
				if err != nil {
					return nil, err
				}
				if len(responses) > 0 {
					trajectory = append(trajectory, trajectoryTurn("tool", strings.Join(responses, "\n")))
					i = j - 1
				}
				continue
			}
			prefix, err := trajectoryGPTPrefix(msg)
			if err != nil {
				return nil, err
			}
			content, err := trajectoryItem(msg, "content")
			if err != nil {
				return nil, err
			}
			text := ""
			if pyrepr.Truthy(content) {
				s, ok := content.(string)
				if !ok {
					return nil, fmt.Errorf("TypeError: can only concatenate str (not \"%s\") to str", pyrepr.TypeName(content))
				}
				text = s
			}
			trajectory = append(trajectory, trajectoryTurn("gpt", pystr.Strip(withThinkBlock(prefix+ConvertScratchpadToThink(text)))))
		case "user":
			content, err := trajectoryItem(msg, "content")
			if err != nil {
				return nil, err
			}
			trajectory = append(trajectory, trajectoryTurn("human", content))
		}
	}
	return trajectory, nil
}
