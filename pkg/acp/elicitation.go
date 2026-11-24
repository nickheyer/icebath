package acp

import (
	"errors"
	"fmt"
	"strconv"

	acputil "github.com/nickheyer/icebath/pkg/utils/acp"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

const (
	ElicitationMethod = "elicitation/create"
	SkipOption        = "skip"
)

// Reports whether raw initialize params advertise form elicitation under either capability key
func ClientSupportsFormElicitation(initializeParams any) bool {
	params, ok := initializeParams.(*jsonutil.Object)
	if !ok {
		return false
	}
	for _, key := range []string{"clientCapabilities", "capabilities"} {
		caps, _ := params.Get(key)
		capObj, ok := caps.(*jsonutil.Object)
		if !ok {
			continue
		}
		el, _ := capObj.Get("elicitation")
		elObj, ok := el.(*jsonutil.Object)
		if !ok {
			continue
		}
		if form, present := elObj.Get("form"); present && form != nil {
			return true
		}
	}
	return false
}

func promptChoices(prompt *jsonutil.Object) []any {
	v, _ := prompt.Get("choices")
	items, _ := v.([]any)
	return append([]any{}, items...)
}

// Elicitation form params asking the clarify question, an enum or multi select array when choices exist
func BuildClarifyElicitation(sessionID string, prompt *jsonutil.Object) (*jsonutil.Object, error) {
	question, ok := prompt.Get("question")
	if !ok {
		return nil, errors.New("the clarify prompt has no question")
	}
	choices := promptChoices(prompt)
	schema := jsonutil.NewObject()
	schema.Set("type", "string")
	if len(choices) > 0 {
		schema.Set("enum", choices)
		if ms, _ := prompt.Get("multi_select"); anyutil.IsTrue(ms) {
			arr := jsonutil.NewObject()
			arr.Set("type", "array")
			arr.Set("items", schema)
			schema = arr
		}
	}
	props := jsonutil.NewObject()
	props.Set("answer", schema)
	requested := jsonutil.NewObject()
	requested.Set("type", "object")
	requested.Set("properties", props)
	requested.Set("required", []any{"answer"})
	out := jsonutil.NewObject()
	out.Set("sessionId", sessionID)
	out.Set("mode", "form")
	out.Set("message", question)
	out.Set("requestedSchema", requested)
	return out, nil
}

// Answer of an elicitation response, empty for decline or cancel, false when not answered
func ElicitedAnswer(response any) (string, bool) {
	resp, ok := response.(*jsonutil.Object)
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
	if content, _ := resp.Get("content"); anyutil.IsTrue(content) {
		if obj, ok := content.(*jsonutil.Object); ok {
			value, _ = obj.Get("answer")
		}
	}
	if items, ok := value.([]any); ok {
		for _, it := range items {
			if _, ok := it.(string); !ok {
				return "", false
			}
		}
		text, err := jsonutil.Encode(items)
		return text, err == nil
	}
	s, ok := value.(string)
	return s, ok
}

// Permission card for a clarify prompt, one option per choice plus Skip and each option's answer
func BuildClarifyPermission(prompt *jsonutil.Object) (*acputil.ToolCallProgress, []acputil.PermissionOption, map[string]string, error) {
	question, ok := prompt.Get("question")
	if !ok {
		return nil, nil, nil, errors.New("the clarify prompt has no question")
	}
	q, ok := question.(string)
	if !ok {
		return nil, nil, nil, fmt.Errorf("title: Input should be a valid string")
	}
	choices := promptChoices(prompt)
	var options []acputil.PermissionOption
	answers := map[string]string{}
	for i, c := range choices {
		name, ok := c.(string)
		if !ok {
			return nil, nil, nil, fmt.Errorf("name: Input should be a valid string")
		}
		id := "choice-" + strconv.Itoa(i)
		options = append(options, acputil.PermissionOption{OptionID: id, Kind: "allow_once", Name: name})
		answers[id] = name
	}
	options = append(options, acputil.PermissionOption{OptionID: SkipOption, Kind: "reject_once", Name: "Skip"})
	answers[SkipOption] = ""
	promptID, ok := prompt.Get("prompt_id")
	if !ok {
		return nil, nil, nil, errors.New("the clarify prompt has no prompt_id")
	}
	raw := jsonutil.NewObject()
	raw.Set("question", q)
	raw.Set("choices", choices)
	tc := acputil.UpdateToolCall("clarify-"+anyutil.String(promptID), acputil.ToolCallFields{
		Title: new(q), Kind: new("other"), Status: new("pending"),
		Content:  []acputil.ToolCallContent{acputil.ToolContent(acputil.TextBlock(q))},
		RawInput: raw,
	})
	return tc, options, answers, nil
}
