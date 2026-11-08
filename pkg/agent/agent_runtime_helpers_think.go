package agent

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

var (
	toolCallTagNames      = []string{"tool_call", "tool_calls", "tool_result", "function_call", "function_calls"}
	thinkTagAlternation   = strings.Join(provider.ThinkTagNames, "|")
	toolCallTagAltern     = strings.Join(toolCallTagNames, "|")
	droppedFlattenTypes   = map[string]bool{"thinking": true, "reasoning": true, "redacted_thinking": true}
	thinkStripPatterns    = buildThinkStripPatterns()
	inlineReasoningBlocks = buildInlineReasoningPatterns()
)

const (
	toolTagNSPrefix = `(?:[\w.-]+:)?`
	dotallNoCase    = regexp2.Singleline | regexp2.IgnoreCase
)

// Ordered strip passes of closed pairs, tool call blocks, named functions, unterminated and orphan tags
func buildThinkStripPatterns() []*regexp2.Regexp {
	var out []*regexp2.Regexp
	for _, name := range provider.ThinkTagNames {
		out = append(out, regexp2.MustCompile(`<`+name+`>.*?</`+name+`>`, dotallNoCase))
	}
	for _, name := range toolCallTagNames {
		out = append(out, regexp2.MustCompile(`<`+toolTagNSPrefix+name+`\b[^>]*>.*?</`+toolTagNSPrefix+name+`>`, dotallNoCase))
	}
	return append(out,
		regexp2.MustCompile(`(?:(?<=^)|(?<=[\n\r.!?:]))[ \t]*<function\b[^>]*\bname\s*=[^>]*>(?:(?:(?!</function>).)*)</function>`, dotallNoCase),
		regexp2.MustCompile(`(?:^|\n)[ \t]*<(?:`+thinkTagAlternation+`)\b[^>]*>.*$`, dotallNoCase),
		regexp2.MustCompile(`</?(?:`+thinkTagAlternation+`)>\s*`, regexp2.IgnoreCase),
		regexp2.MustCompile(`</(?:`+toolTagNSPrefix+`(?:`+toolCallTagAltern+`|function))>\s*`, regexp2.IgnoreCase),
		regexp2.MustCompile(`(?:^|\n)[ \t]*<`+toolTagNSPrefix+`(?:`+toolCallTagAltern+`)\b[^>]*>.*$`+
			`|(?:^|\n)[ \t]*[\w.:-]*</?arg_(?:key|value)\b[^\n]*`+
			`|(?:^|\n)[^\n<]*</arg_(?:key|value)>[ \t\r]*(?=\n|$)`, dotallNoCase),
	)
}

// One capture pattern per reasoning tag for inline reasoning embedded in string content
func buildInlineReasoningPatterns() []*regexp2.Regexp {
	out := make([]*regexp2.Regexp, 0, len(provider.ThinkTagNames))
	for _, tag := range provider.ThinkTagNames {
		out = append(out, regexp2.MustCompile(`<`+tag+`>(.*?)</`+tag+`>`, dotallNoCase))
	}
	return out
}

// Flattens list or dict content to text with thinking and reasoning parts dropped
func flattenContentText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, part := range c {
			b.WriteString(flattenContentPart(part))
		}
		return b.String()
	case []map[string]any:
		var b strings.Builder
		for _, part := range c {
			b.WriteString(flattenContentPart(part))
		}
		return b.String()
	case map[string]any:
		v := c["text"]
		if !pyrepr.Truthy(v) {
			v = c["content"]
		}
		if !pyrepr.Truthy(v) {
			return ""
		}
		return pyrepr.Str(v)
	}
	return pyrepr.Str(content)
}

// Text a list part contributes, the string itself or the non empty text of a non reasoning dict
func flattenContentPart(part any) string {
	switch p := part.(type) {
	case string:
		return p
	case map[string]any:
		kind := ""
		if pyrepr.Truthy(p["type"]) {
			kind = pyrepr.Str(p["type"])
		}
		if text, ok := p["text"].(string); ok && text != "" && !droppedFlattenTypes[pystr.Lower(pystr.Strip(kind))] {
			return text
		}
	}
	return ""
}

// Visible text with reasoning blocks, tool call XML and stray tags removed
func (a *AIAgent) stripThinkBlocks(content any) string {
	if !pyrepr.Truthy(content) {
		return ""
	}
	text := flattenContentText(content)
	if text == "" {
		return ""
	}
	for _, re := range thinkStripPatterns {
		text, _ = re.Replace(text, "", -1, -1)
	}
	return text
}

// Reasoning from the structured fields, else thinking blocks or inline tags of the content, empty when absent
func (a *AIAgent) extractReasoning(resp *provider.Response) string {
	if resp == nil || resp.Message == nil {
		return ""
	}
	msg := resp.Message
	var parts []string
	add := func(v any) {
		if text := provider.FlattenMessageText(v, ""); text != "" && !slices.Contains(parts, text) {
			parts = append(parts, text)
		}
	}
	add(msg.Reasoning)
	add(msg.ReasoningContent)
	var details any
	if len(msg.ReasoningDetails) > 0 {
		_ = json.Unmarshal(msg.ReasoningDetails, &details)
	}
	if list, ok := details.([]any); ok {
		for _, d := range list {
			if detail, ok := d.(map[string]any); ok {
				add(firstTruthyKey(detail, "summary", "thinking", "content", "text"))
			}
		}
	}
	if len(parts) == 0 {
		for _, block := range contentBlocks(msg.Content) {
			if block["type"] == "thinking" {
				add(pystr.Strip(provider.FlattenMessageText(firstTruthyKey(block, "thinking", "text", ""), "")))
			}
		}
	}
	if s, ok := msg.Content.(string); ok && len(parts) == 0 && s != "" {
		for _, re := range inlineReasoningBlocks {
			for _, m := range pyFindAll(re, s) {
				add(pystr.Strip(m.GroupByNumber(1).String()))
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// ExtractReasoning is the reasoning of a dict shaped assistant message
func (a *AIAgent) ExtractReasoning(message map[string]any) string {
	return a.extractReasoning(&provider.Response{Message: state.MessageFromDict(message)})
}

// First truthy key value in order, else the last key value, empty text when the last key is empty
func firstTruthyKey(m map[string]any, keys ...string) any {
	var v any
	for i, k := range keys {
		if k == "" && i == len(keys)-1 {
			return ""
		}
		if v = m[k]; pyrepr.Truthy(v) {
			return v
		}
	}
	return v
}

// Dict blocks of list content, nil for string or absent content
func contentBlocks(content any) []map[string]any {
	switch c := content.(type) {
	case []map[string]any:
		return c
	case []any:
		out := make([]map[string]any, 0, len(c))
		for _, b := range c {
			if m, ok := b.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}
