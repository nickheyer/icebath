package transport

import (
	"cmp"
	"encoding/json"
	"fmt"
	"github.com/nickheyer/icebath/pkg/strs"
	"log/slog"
	"regexp"
	"strings"
)

var trailingComma = regexp.MustCompile(`,\s*([}\]])`)

// Arguments of a tool call fixed when they were cut short, carry a trailing comma or raw control characters; {} when nothing fixes them
func RepairArguments(raw, tool string) string {
	tool = cmp.Or(tool, "?")
	stripped := strings.TrimSpace(raw)
	switch stripped {
	case "":
		slog.Warn("Sanitized empty tool_call arguments for " + tool)
		return "{}"
	case "None":
		slog.Warn("Sanitized Python-None tool_call arguments for " + tool)
		return "{}"
	}
	if escaped := escapeControls(stripped); json.Valid([]byte(escaped)) {
		var decoded any
		var encoded strings.Builder
		encoder := json.NewEncoder(&encoded)
		encoder.SetEscapeHTML(false)
		if json.Unmarshal([]byte(escaped), &decoded) == nil && encoder.Encode(decoded) == nil {
			if escaped != stripped {
				slog.Warn("Repaired unescaped control chars in tool_call arguments for " + tool)
			}
			return strings.TrimSuffix(encoded.String(), "\n")
		}
	}
	fixed := trailingComma.ReplaceAllString(stripped, "$1")
	if balanced, ok := rebalance(fixed); ok && balanced != "" {
		fixed = balanced
	}
	for range 50 {
		excessBrace := strings.HasSuffix(fixed, "}") && strings.Count(fixed, "}") > strings.Count(fixed, "{")
		excessBracket := strings.HasSuffix(fixed, "]") && strings.Count(fixed, "]") > strings.Count(fixed, "[")
		if json.Valid([]byte(fixed)) || !excessBrace && !excessBracket {
			break
		}
		fixed = fixed[:len(fixed)-1]
	}
	if json.Valid([]byte(fixed)) {
		slog.Warn(fmt.Sprintf("Repaired malformed tool_call arguments for %s: %s → %s", tool, strs.Head(stripped, 80), strs.Head(fixed, 80)))
		return fixed
	}
	if escaped := escapeControls(fixed); escaped != fixed && json.Valid([]byte(escaped)) {
		slog.Warn(fmt.Sprintf("Repaired control-char-laced tool_call arguments for %s: %s → %s", tool, strs.Head(stripped, 80), strs.Head(escaped, 80)))
		return escaped
	}
	slog.Warn(fmt.Sprintf("Unrepairable tool_call arguments for %s — replaced with empty object (was: %s)", tool, strs.Head(stripped, 100_000)))
	return "{}"
}

// Raw control characters inside JSON strings written as unicode escapes
func escapeControls(raw string) string {
	var out strings.Builder
	inString := false
	r := []rune(raw)
	for i := 0; i < len(r); i++ {
		ch := r[i]
		if inString && ch == '\\' && i+1 < len(r) {
			out.WriteRune(ch)
			out.WriteRune(r[i+1])
			i++
			continue
		}
		if ch == '"' {
			inString = !inString
		}
		if inString && ch < 0x20 {
			fmt.Fprintf(&out, "\\u%04x", ch)
		} else {
			out.WriteRune(ch)
		}
	}
	return out.String()
}

var closers = map[byte]byte{'{': '}', '[': ']'}

// JSON cut short with its open structures closed in order, false when it ends inside a string
func rebalance(raw string) (string, bool) {
	var out strings.Builder
	var stack []byte
	inString := false
	b := []byte(raw)
	for i := 0; i < len(b); i++ {
		ch := b[i]
		switch {
		case inString && ch == '\\':
			out.WriteByte(ch)
			if i+1 < len(b) {
				out.WriteByte(b[i+1])
			}
			i++
			continue
		case inString && ch == '"':
			inString = false
		case inString:
		case ch == '"':
			inString = true
		case ch == '{' || ch == '[':
			stack = append(stack, ch)
		case ch == '}' || ch == ']':
			open := false
			for _, o := range stack {
				open = open || closers[o] == ch
			}
			if open {
				for closers[stack[len(stack)-1]] != ch {
					out.WriteByte(closers[stack[len(stack)-1]])
					stack = stack[:len(stack)-1]
				}
				stack = stack[:len(stack)-1]
			}
		}
		out.WriteByte(ch)
	}
	if inString {
		return "", false
	}
	for i := len(stack) - 1; i >= 0; i-- {
		out.WriteByte(closers[stack[i]])
	}
	return out.String(), true
}
