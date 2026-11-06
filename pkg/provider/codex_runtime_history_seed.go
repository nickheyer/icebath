package provider

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// MaxHistorySeedChars caps the rendered history codex resends on every request of a fresh thread
const MaxHistorySeedChars = 32_000

const codexToolResultPreviewChars = 400

const codexHistorySeedHeader = "Prior conversation from this Icebath session (the thread you are continuing was started fresh; " +
	"treat these turns as already having happened):"

// Text of message content, string parts and the text of dict parts joined by newlines
func codexSeedTextOf(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			text := ""
			switch v := p.(type) {
			case string:
				text = v
			default:
				if !cxIsObject(v) {
					continue
				}
				if cxHas(v, "text") {
					text = pyrepr.Str(cxOr(cxGet(v, "text"), ""))
				}
			}
			if text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// One transcript row of the seed, empty for system rows and rows without text
func codexSeedRenderRow(msg map[string]any) string {
	role := msg["role"]
	text := pystr.Strip(codexSeedTextOf(msg["content"]))
	switch role {
	case "user":
		if text != "" {
			return "[USER]\n" + text
		}
	case "assistant":
		var lines []string
		if text != "" {
			lines = append(lines, "[ASSISTANT]\n"+text)
		}
		var names []string
		hasCalls := false
		for _, c := range cxList(cxOr(msg["tool_calls"], []any{})) {
			if !cxIsObject(c) {
				continue
			}
			hasCalls = true
			if name := cxGet(cxOr(cxGet(c, "function"), map[string]any{}), "name"); cxTruthy(name) {
				names = append(names, pyrepr.Str(name))
			}
		}
		if hasCalls {
			lines = append(lines, "[ASSISTANT called tools: "+strings.Join(names, ", ")+"]")
		}
		return strings.Join(lines, "\n")
	case "tool":
		if pystr.Len(text) > codexToolResultPreviewChars {
			text = pystr.Slice(text, 0, codexToolResultPreviewChars) + " …"
		}
		if text != "" {
			return "[TOOL RESULT]\n" + text
		}
	}
	return ""
}

// Prior turns as one text block newest last, never the trailing user row being submitted
func RenderHistorySeed(messages []map[string]any) string {
	rows := messages
	if len(rows) > 0 && rows[len(rows)-1]["role"] == "user" {
		rows = rows[:len(rows)-1]
	}
	var rendered []string
	for _, m := range rows {
		if m == nil {
			continue
		}
		if r := codexSeedRenderRow(m); r != "" {
			rendered = append(rendered, r)
		}
	}
	if len(rendered) == 0 {
		return ""
	}
	body := strings.Join(rendered, "\n\n")
	if pystr.Len(body) > MaxHistorySeedChars {
		body = "[… earlier turns omitted …]\n\n" + pystr.Slice(body, -MaxHistorySeedChars, pystr.Len(body))
	}
	return codexHistorySeedHeader + "\n\n" + body
}
