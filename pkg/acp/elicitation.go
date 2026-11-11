package acp

import (
	"fmt"
	"strconv"

	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const (
	ElicitationMethod = "elicitation/create"
	SkipOption        = "skip"
)

// Reports whether raw initialize params advertise form elicitation under either capability key
func ClientSupportsFormElicitation(initializeParams any) bool {
	params, ok := initializeParams.(*pyjson.Object)
	if !ok {
		return false
	}
	for _, key := range []string{"clientCapabilities", "capabilities"} {
		caps, _ := params.Get(key)
		capObj, ok := caps.(*pyjson.Object)
		if !ok {
			continue
		}
		el, _ := capObj.Get("elicitation")
		elObj, ok := el.(*pyjson.Object)
		if !ok {
			continue
		}
		if form, present := elObj.Get("form"); present && form != nil {
			return true
		}
	}
	return false
}

func promptChoices(prompt *pyjson.Object) []any {
	v, _ := prompt.Get("choices")
	items, _ := v.([]any)
	return append([]any{}, items...)
}

// Elicitation form params asking the clarify question, an enum or multi select array when choices exist
func BuildClarifyElicitation(sessionID string, prompt *pyjson.Object) (*pyjson.Object, error) {
	question, ok := prompt.Get("question")
	if !ok {
		return nil, fmt.Errorf("KeyError: 'question'")
	}
	choices := promptChoices(prompt)
	schema := pyjson.NewObject()
	schema.Set("type", "string")
	if len(choices) > 0 {
		schema.Set("enum", choices)
		if ms, _ := prompt.Get("multi_select"); truthy(ms) {
			arr := pyjson.NewObject()
			arr.Set("type", "array")
			arr.Set("items", schema)
			schema = arr
		}
	}
	props := pyjson.NewObject()
	props.Set("answer", schema)
	requested := pyjson.NewObject()
	requested.Set("type", "object")
	requested.Set("properties", props)
	requested.Set("required", []any{"answer"})
	out := pyjson.NewObject()
	out.Set("sessionId", sessionID)
	out.Set("mode", "form")
	out.Set("message", question)
	out.Set("requestedSchema", requested)
	return out, nil
}

// Answer of an elicitation response, empty for decline or cancel, false when not answered
func ElicitedAnswer(response any) (string, bool) {
	resp, ok := response.(*pyjson.Object)
	if !ok {
		return "", false
	}
	action, _ := resp.Get("action")
	if action != "accept" && action != "decline" && action != "cancel" {
		return "", false
	}
	if action != "accept" {
		return "", true
	}
	var value any
	if content, _ := resp.Get("content"); truthy(content) {
		if obj, ok := content.(*pyjson.Object); ok {
			value, _ = obj.Get("answer")
		}
	}
	if items, ok := value.([]any); ok {
		for _, it := range items {
			if _, ok := it.(string); !ok {
				return "", false
			}
		}
		text, err := pyjson.Dumps(items, pyjson.EnsureASCII(false))
		return text, err == nil
	}
	s, ok := value.(string)
	return s, ok
}

// Permission card for a clarify prompt, one option per choice plus Skip and each option's answer
func BuildClarifyPermission(prompt *pyjson.Object) (*pyacp.ToolCallProgress, []pyacp.PermissionOption, map[string]string, error) {
	question, ok := prompt.Get("question")
	if !ok {
		return nil, nil, nil, fmt.Errorf("KeyError: 'question'")
	}
	q, ok := question.(string)
	if !ok {
		return nil, nil, nil, fmt.Errorf("title: Input should be a valid string")
	}
	choices := promptChoices(prompt)
	var options []pyacp.PermissionOption
	answers := map[string]string{}
	for i, c := range choices {
		name, ok := c.(string)
		if !ok {
			return nil, nil, nil, fmt.Errorf("name: Input should be a valid string")
		}
		id := "choice-" + strconv.Itoa(i)
		options = append(options, pyacp.PermissionOption{OptionID: id, Kind: "allow_once", Name: name})
		answers[id] = name
	}
	options = append(options, pyacp.PermissionOption{OptionID: SkipOption, Kind: "reject_once", Name: "Skip"})
	answers[SkipOption] = ""
	promptID, ok := prompt.Get("prompt_id")
	if !ok {
		return nil, nil, nil, fmt.Errorf("KeyError: 'prompt_id'")
	}
	raw := pyjson.NewObject()
	raw.Set("question", q)
	raw.Set("choices", choices)
	tc := pyacp.UpdateToolCall("clarify-"+pyStr(promptID), pyacp.ToolCallFields{
		Title: pyacp.Ptr(q), Kind: pyacp.Ptr("other"), Status: pyacp.Ptr("pending"),
		Content:  []pyacp.ToolCallContent{pyacp.ToolContent(pyacp.TextBlock(q))},
		RawInput: raw,
	})
	return tc, options, answers, nil
}
