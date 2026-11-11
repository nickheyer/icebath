package tuigateway

import (
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// One optional subagent payload field with its presence test and coercion, nil from coerce dropping it
type subagentField struct {
	key     string
	present func(any) bool
	coerce  func(any) (any, error)
}

func subagentNotNone(v any) bool { return v != nil }

func subagentStr(v any) (any, error) { return pyrepr.Str(v), nil }

func subagentInt(v any) (any, error) {
	n, err := pyint.Int64(v)
	if err != nil {
		return nil, err
	}
	return n, nil
}

func subagentFloat(v any) (any, error) {
	f, err := pyint.Float(v)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Integer of a rollup value, nil for junk from older emitters so the field is omitted
func subagentIntOrSkip(v any) (any, error) {
	n, err := pyint.Int64(v)
	if err != nil {
		return nil, nil
	}
	return n, nil
}

func subagentStrList(v any) (any, error) {
	items, err := subagentList(v)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(items))
	for _, item := range items {
		out = append(out, pyrepr.Str(item))
	}
	return out, nil
}

func subagentList(v any) ([]any, error) {
	switch typed := v.(type) {
	case []any:
		return append([]any(nil), typed...), nil
	case []string:
		out := make([]any, 0, len(typed))
		for _, s := range typed {
			out = append(out, s)
		}
		return out, nil
	case []map[string]any:
		out := make([]any, 0, len(typed))
		for _, m := range typed {
			out = append(out, m)
		}
		return out, nil
	case string:
		out := make([]any, 0, len(typed))
		for _, r := range typed {
			out = append(out, string(r))
		}
		return out, nil
	}
	return nil, fmt.Errorf("TypeError: '%T' object is not iterable", v)
}

func subagentListCoerce(v any) (any, error) { return subagentList(v) }

// Optional subagent payload fields in wire order
var subagentFields = []subagentField{
	{"subagent_id", pyrepr.Truthy, subagentStr}, {"parent_id", pyrepr.Truthy, subagentStr},
	{"child_session_id", pyrepr.Truthy, subagentStr}, {"delegation_id", pyrepr.Truthy, subagentStr},
	{"depth", subagentNotNone, subagentInt}, {"model", pyrepr.Truthy, subagentStr},
	{"tool_count", subagentNotNone, subagentInt}, {"toolsets", pyrepr.Truthy, subagentStrList},
	{"input_tokens", subagentNotNone, subagentIntOrSkip}, {"output_tokens", subagentNotNone, subagentIntOrSkip},
	{"reasoning_tokens", subagentNotNone, subagentIntOrSkip}, {"api_calls", subagentNotNone, subagentIntOrSkip},
	{"files_read", pyrepr.Truthy, subagentStrList}, {"files_written", pyrepr.Truthy, subagentStrList},
	{"output_tail", pyrepr.Truthy, subagentListCoerce}, {"tool_name", pyrepr.Truthy, subagentStr},
	{"text", pyrepr.Truthy, subagentStr}, {"status", pyrepr.Truthy, subagentStr},
	{"summary", pyrepr.Truthy, subagentStr}, {"duration_seconds", subagentNotNone, subagentFloat},
}

func orDefault(v any, fallback any) any {
	if pyrepr.Truthy(v) {
		return v
	}
	return fallback
}

// Subagent event payload with goal and task position first, the chain of thought dropped while reasoning is hidden
func SubagentPayload(eventType string, name, preview any, kw map[string]any, showReasoning bool) (*pyjson.Object, error) {
	payload := pyjson.NewObject()
	payload.Set("goal", pyrepr.Str(orDefault(kw["goal"], "")))
	taskCount, err := pyint.Int64(orDefault(kw["task_count"], 1))
	if err != nil {
		return nil, err
	}
	payload.Set("task_count", taskCount)
	taskIndex, err := pyint.Int64(orDefault(kw["task_index"], 0))
	if err != nil {
		return nil, err
	}
	payload.Set("task_index", taskIndex)
	source := make(map[string]any, len(kw)+2)
	for k, v := range kw {
		source[k] = v
	}
	source["tool_name"], source["text"] = name, preview
	for _, field := range subagentFields {
		raw := source[field.key]
		if !field.present(raw) {
			continue
		}
		val, err := field.coerce(raw)
		if err != nil {
			return nil, err
		}
		if val != nil {
			payload.Set(field.key, val)
		}
	}
	if eventType == "subagent.thinking" && !showReasoning {
		payload.Delete("text")
	}
	if pyrepr.Truthy(preview) && eventType == "subagent.tool" {
		payload.Set("tool_preview", pyrepr.Str(preview))
		payload.Set("text", pyrepr.Str(preview))
	}
	return payload, nil
}
