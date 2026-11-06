package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	absPathPrefixPattern   = `(?:[A-Za-z]:[/\\]|/|\\\\)`
	captureBasenamePattern = `computer_use_[0-9a-f]{32}\.(?:png|jpe?g)`
)

var (
	absPathPrefixRE   = newPyRE(`^` + absPathPrefixPattern)
	captureBasenameRE = mustFullmatchPy(`(?i)^` + captureBasenamePattern + `$`)
	captureSummaryRE  = newPyRE(`(?i)\(shareable screenshot saved to (?P<path>` + absPathPrefixPattern + `[^\r\n]*?` + captureBasenamePattern + `)\)`)
)

func mustFullmatchPy(pattern string) *regexp2.Regexp {
	re, err := pyre.CompileFullmatch(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

// Error of a message value that is not the dict Python indexes with get
type notDictError struct{ value any }

func (e notDictError) Error() string {
	return fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(e.value))
}

// Value under key of a decoded dict, an error for anything else
func dictGet(d any, key string) (any, error) {
	switch t := d.(type) {
	case map[string]any:
		return t[key], nil
	case *pyjson.Object:
		v, _ := t.Get(key)
		return v, nil
	}
	return nil, notDictError{d}
}

// Whether the value is a decoded dict
func isDict(v any) bool {
	switch v.(type) {
	case map[string]any, *pyjson.Object:
		return true
	}
	return false
}

// First truthy value of the keys, nil when none is truthy
func firstTruthy(d any, keys ...string) (any, error) {
	for _, k := range keys {
		v, err := dictGet(d, k)
		if err != nil {
			return nil, err
		}
		if pyrepr.Truthy(v) {
			return v, nil
		}
	}
	return nil, nil
}

// Elements of a falsy or list value, an error for any other iterable
func listOrEmpty(v any) ([]any, error) {
	if !pyrepr.Truthy(v) {
		return nil, nil
	}
	if l, ok := v.([]any); ok {
		return l, nil
	}
	return nil, notDictError{v}
}

// Assistant tool call ids mapped to their tool names
func ToolNameByCallID(messages []any) (map[string]string, error) {
	mapping := map[string]string{}
	for _, msg := range messages {
		role, err := dictGet(msg, "role")
		if err != nil {
			return nil, err
		}
		if role != "assistant" {
			continue
		}
		raw, _ := dictGet(msg, "tool_calls")
		calls, err := listOrEmpty(raw)
		if err != nil {
			return nil, err
		}
		for _, call := range calls {
			callID, err := firstTruthy(call, "id", "call_id")
			if err != nil {
				return nil, err
			}
			fn, _ := dictGet(call, "function")
			var name any
			if pyrepr.Truthy(fn) {
				if name, err = dictGet(fn, "name"); err != nil {
					return nil, err
				}
			}
			if !pyrepr.Truthy(name) {
				name, _ = dictGet(call, "name")
			}
			nameText := ""
			if pyrepr.Truthy(name) {
				nameText = pyrepr.Str(name)
			}
			if pyrepr.Truthy(callID) && nameText != "" {
				mapping[pyrepr.Str(callID)] = nameText
			}
		}
	}
	return mapping, nil
}

// Lowercased capture basename of a path in either separator style, empty when it is no capture
func computerUseCaptureBasename(path any) string {
	text := ""
	if pyrepr.Truthy(path) {
		text = pyrepr.Str(path)
	}
	text = strings.Trim(pystr.Strip(text), "`\"'")
	basename := text[strings.LastIndexAny(text, `/\`)+1:]
	if ok, _ := captureBasenameRE.MatchString(basename); ok {
		return pystr.Lower(basename)
	}
	return ""
}

// Persisted screenshot paths of computer use result content in JSON, multimodal or summary text form
func iterComputerUseCapturePaths(content any) ([]string, error) {
	switch t := content.(type) {
	case string:
		stripped := pystr.Strip(t)
		if !strings.HasPrefix(stripped, "{") && !strings.HasPrefix(stripped, "[") {
			var out []string
			for _, m := range captureSummaryRE.finditer([]rune(t)) {
				out = append(out, pystr.Strip(captureSummaryRE.group(m, "path")))
			}
			return out, nil
		}
		payload, err := pyjson.Loads(stripped)
		if err != nil {
			return nil, nil
		}
		if _, ok := payload.([]any); ok || isDict(payload) {
			return iterComputerUseCapturePaths(payload)
		}
		return nil, nil
	case []any:
		var out []string
		for _, part := range t {
			paths, err := iterComputerUseCapturePaths(part)
			if err != nil {
				return nil, err
			}
			out = append(out, paths...)
		}
		return out, nil
	}
	if !isDict(content) {
		return nil, nil
	}
	var out []string
	meta, _ := dictGet(content, "meta")
	holders := []any{content}
	if isDict(meta) {
		holders = append(holders, meta)
	}
	for _, holder := range holders {
		if p, _ := dictGet(holder, "screenshot_path"); p != nil {
			if s, ok := p.(string); ok {
				out = append(out, s)
			}
		}
	}
	for _, field := range []string{"content", "text", "text_summary", "summary"} {
		nested, _ := dictGet(content, field)
		_, isStr := nested.(string)
		_, isList := nested.([]any)
		if isStr || isList || isDict(nested) {
			paths, err := iterComputerUseCapturePaths(nested)
			if err != nil {
				return nil, err
			}
			out = append(out, paths...)
		}
	}
	return out, nil
}

// Messages of the current turn, recovered from the last user message when compression moved the boundary
func currentTurnMessages(messages []any, historyOffset int) ([]any, error) {
	if historyOffset == 0 || len(messages) >= historyOffset {
		start := historyOffset
		if start < 0 {
			start = max(0, len(messages)+start)
		}
		return messages[start:], nil
	}
	for i := len(messages) - 1; i >= 0; i-- {
		role, err := dictGet(messages[i], "role")
		if err != nil {
			return nil, err
		}
		if role == "user" {
			return messages[i:], nil
		}
	}
	return nil, nil
}

// Lowercase capture basename mapped to the absolute canonical path from this turn's computer use results
func canonicalCapturePaths(turn []any) (map[string]string, error) {
	callIDNames, err := ToolNameByCallID(turn)
	if err != nil {
		return nil, err
	}
	canonical := map[string]string{}
	for _, msg := range turn {
		role, err := dictGet(msg, "role")
		if err != nil {
			return nil, err
		}
		switch role.(type) {
		case []any, map[string]any, *pyjson.Object:
			return nil, fmt.Errorf("unhashable type: '%s'", pyrepr.TypeName(role))
		}
		if role != "tool" && role != "function" {
			continue
		}
		id, err := firstTruthy(msg, "tool_call_id", "call_id")
		if err != nil {
			return nil, err
		}
		callID := ""
		if id != nil {
			callID = pyrepr.Str(id)
		}
		name, err := firstTruthy(msg, "name", "tool_name")
		if err != nil {
			return nil, err
		}
		nameText := callIDNames[callID]
		if name != nil {
			nameText = pyrepr.Str(name)
		}
		if nameText != "computer_use" {
			continue
		}
		content, _ := dictGet(msg, "content")
		paths, err := iterComputerUseCapturePaths(content)
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			basename := computerUseCaptureBasename(path)
			if m, _ := absPathPrefixRE.re.FindStringMatch(path); basename != "" && m != nil {
				canonical[basename] = path
			}
		}
	}
	return canonical, nil
}

// Response with model mangled capture paths in explicit media tags restored to this turn's canonical paths
func RepairExplicitComputerUseMediaPaths(response string, messages []any) string {
	return RepairExplicitComputerUseMediaPathsAt(context.Background(), response, messages, 0)
}

// Repair over the messages after the history offset, the response unchanged on any failure
func RepairExplicitComputerUseMediaPathsAt(ctx context.Context, response string, messages []any, historyOffset int) string {
	out, err := repairExplicitComputerUseMediaPaths(ctx, response, messages, historyOffset)
	if err != nil {
		slog.Debug("computer_use media path repair failed", "error", err)
		return response
	}
	return out
}

func repairExplicitComputerUseMediaPaths(ctx context.Context, response string, messages []any, historyOffset int) (string, error) {
	if !strings.Contains(response, "MEDIA:") {
		return response, nil
	}
	turn, err := currentTurnMessages(messages, historyOffset)
	if err != nil {
		return "", err
	}
	byBasename, err := canonicalCapturePaths(turn)
	if err != nil || len(byBasename) == 0 {
		return response, err
	}
	media, _, err := ExtractMedia(ctx, response)
	if err != nil {
		return "", err
	}
	repaired := response
	for _, ref := range media {
		if canonical := byBasename[computerUseCaptureBasename(ref.Path)]; canonical != "" && ref.Path != canonical {
			repaired = strings.ReplaceAll(repaired, ref.Path, canonical)
		}
	}
	return repaired, nil
}
