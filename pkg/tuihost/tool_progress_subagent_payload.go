package tuihost

import (
	"fmt"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Text of a truthy progress value, nil for an absent or falsy one
func subagentText(v any) *string {
	if !anyutil.IsTrue(v) {
		return nil
	}
	text := anyutil.String(v)
	return &text
}

// Integer of a present progress value, refused when it is no integer
func subagentInt(v any) (*int64, error) {
	if v == nil {
		return nil, nil
	}
	n, err := anyutil.ToInt(v)
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// Integer of a usage rollup value, nil for junk so the field is left out
func subagentCount(v any) *int64 {
	if v == nil {
		return nil
	}
	n, err := anyutil.ToInt(v)
	if err != nil {
		return nil
	}
	return &n
}

// Number of a present progress value, refused when it is no number
func subagentFloat(v any) (*float64, error) {
	if v == nil {
		return nil, nil
	}
	f, err := anyutil.ToFloat(v)
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// Items of a progress list value, a text read as its characters
func subagentItems(v any) ([]any, error) {
	switch typed := v.(type) {
	case []any:
		return typed, nil
	case []string:
		out := make([]any, len(typed))
		for i, s := range typed {
			out[i] = s
		}
		return out, nil
	case []map[string]any:
		out := make([]any, len(typed))
		for i, m := range typed {
			out[i] = m
		}
		return out, nil
	case string:
		out := make([]any, 0, len(typed))
		for _, r := range typed {
			out = append(out, string(r))
		}
		return out, nil
	}
	return nil, fmt.Errorf("the subagent list is %s, expected a list", errchain.Kind(v))
}

// Texts of a truthy progress list value, nil for a falsy one
func subagentTexts(v any) ([]string, error) {
	if !anyutil.IsTrue(v) {
		return nil, nil
	}
	items, err := subagentItems(v)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = anyutil.String(item)
	}
	return out, nil
}

// Output tail rows of a finished child, nil for a falsy value
func subagentOutputTail(v any) ([]tuigateway.SubagentOutputTailEntry, error) {
	if !anyutil.IsTrue(v) {
		return nil, nil
	}
	items, err := subagentItems(jsonutil.Plain(v))
	if err != nil {
		return nil, err
	}
	out := make([]tuigateway.SubagentOutputTailEntry, len(items))
	for i, item := range items {
		if err := tuigateway.Strict(item, &out[i]); err != nil {
			return nil, fmt.Errorf("output_tail[%d]: %w", i, err)
		}
	}
	return out, nil
}

// Subagent event payload of an agent progress callback, the chain of thought dropped while reasoning is hidden
func SubagentEvent(eventType string, name, preview any, kw map[string]any, showReasoning bool) (tuigateway.SubagentEventPayload, error) {
	var p tuigateway.SubagentEventPayload
	if anyutil.IsTrue(kw["goal"]) {
		p.Goal = anyutil.String(kw["goal"])
	}
	taskCount, err := anyutil.ToInt(orDefault(kw["task_count"], 1))
	if err != nil {
		return p, err
	}
	taskIndex, err := anyutil.ToInt(orDefault(kw["task_index"], 0))
	if err != nil {
		return p, err
	}
	p.TaskCount, p.TaskIndex = taskCount, taskIndex
	p.SubagentID, p.ParentID = subagentText(kw["subagent_id"]), subagentText(kw["parent_id"])
	p.ChildSessionID, p.DelegationID = subagentText(kw["child_session_id"]), subagentText(kw["delegation_id"])
	p.Model, p.ToolName, p.Text, p.Summary = subagentText(kw["model"]), subagentText(name), subagentText(preview), subagentText(kw["summary"])
	if p.Depth, err = subagentInt(kw["depth"]); err != nil {
		return p, err
	}
	if p.ToolCount, err = subagentInt(kw["tool_count"]); err != nil {
		return p, err
	}
	if p.DurationSeconds, err = subagentFloat(kw["duration_seconds"]); err != nil {
		return p, err
	}
	if p.CostUSD, err = subagentFloat(kw["cost_usd"]); err != nil {
		return p, err
	}
	p.InputTokens, p.OutputTokens = subagentCount(kw["input_tokens"]), subagentCount(kw["output_tokens"])
	p.ReasoningTokens, p.APICalls = subagentCount(kw["reasoning_tokens"]), subagentCount(kw["api_calls"])
	if p.Toolsets, err = subagentTexts(kw["toolsets"]); err != nil {
		return p, err
	}
	if p.FilesRead, err = subagentTexts(kw["files_read"]); err != nil {
		return p, err
	}
	if p.FilesWritten, err = subagentTexts(kw["files_written"]); err != nil {
		return p, err
	}
	if p.OutputTail, err = subagentOutputTail(kw["output_tail"]); err != nil {
		return p, err
	}
	if status := subagentText(kw["status"]); status != nil {
		typed := tuigateway.SubagentStatus(*status)
		p.Status = &typed
	}
	if eventType == "subagent.thinking" && !showReasoning {
		p.Text = nil
	}
	if anyutil.IsTrue(preview) && eventType == "subagent.tool" {
		p.ToolPreview = subagentText(preview)
	}
	return p, nil
}

// Subagent event payload as the ordered object the session event stream publishes
func SubagentPayload(eventType string, name, preview any, kw map[string]any, showReasoning bool) (*jsonutil.Object, error) {
	payload, err := SubagentEvent(eventType, name, preview, kw, showReasoning)
	if err != nil {
		return nil, err
	}
	object, _ := jsonutil.Ordered(tuigateway.Plain(payload)).(*jsonutil.Object)
	return object, nil
}

func orDefault(v any, fallback any) any {
	if anyutil.IsTrue(v) {
		return v
	}
	return fallback
}
