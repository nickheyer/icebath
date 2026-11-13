package recap

import (
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	recentTurnWindow      = 20
	promptPreviewChars    = 140
	assistantPreviewChars = 200
	maxFilesListed        = 5
)

// File touching tool names mapped to the argument key holding the path
var fileEditTools = map[string]string{
	"write_file": "path", "patch": "path", "read_file": "path",
	"skill_manage": "file_path", "skill_view": "file_path",
}

// Value under key of a Go map or decoded JSON object, false when v is no mapping
func mappingGet(v any, key string) (any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m[key], true
	case *pyjson.Object:
		if m == nil {
			return nil, false
		}
		value, _ := m.Get(key)
		return value, true
	case nil:
		return nil, false
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Map || rv.Type().Key().Kind() != reflect.String {
		return nil, false
	}
	value := rv.MapIndex(reflect.ValueOf(key).Convert(rv.Type().Key()))
	if !value.IsValid() {
		return nil, true
	}
	return value.Interface(), true
}

// Reports a Go map with string keys or a decoded JSON object
func isMapping(v any) bool {
	_, ok := mappingGet(v, "")
	return ok
}

// Elements of any Go slice except bytes, false when v is no list
func listItems(v any) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, true
	case []byte, nil:
		return nil, false
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice {
		return nil, false
	}
	items := make([]any, rv.Len())
	for i := range items {
		items[i] = rv.Index(i).Interface()
	}
	return items, true
}

// Role string of a mapping message, false for a non mapping or a non string role
func messageRole(msg any) (string, bool) {
	role, ok := mappingGet(msg, "role")
	if !ok {
		return "", false
	}
	text, isText := role.(string)
	return text, isText
}

// Flattens content strings and the text parts of block lists into one newline joined string
func coerceText(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	blocks, ok := listItems(value)
	if !ok {
		return pyrepr.Str(value)
	}
	var parts []string
	for _, block := range blocks {
		var text any
		_, blockIsText := block.(string)
		if blockIsText {
			text = block
		} else if got, mapping := mappingGet(block, "text"); mapping {
			text = got
		}
		if s, isText := text.(string); isText && (s != "" || blockIsText) {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n")
}

// One tool call argument pair with a JSON string decoded and an empty mapping for anything unparsable
type toolCall struct {
	name string
	args any
}

// Name and argument mapping of a tool call, empty name when its function is no mapping
func toolCallNameAndArgs(call any) toolCall {
	fn, _ := mappingGet(call, "function")
	if !isMapping(fn) {
		return toolCall{args: map[string]any{}}
	}
	rawName, _ := mappingGet(fn, "name")
	name := ""
	if pyrepr.Truthy(rawName) {
		name = pyrepr.Str(rawName)
	}
	rawArgs, _ := mappingGet(fn, "arguments")
	if text, ok := rawArgs.(string); ok && text != "" {
		decoded, err := pyjson.Loads(text)
		if err != nil {
			return toolCall{name: name, args: map[string]any{}}
		}
		rawArgs = decoded
	}
	if !isMapping(rawArgs) {
		return toolCall{name: name, args: map[string]any{}}
	}
	return toolCall{name: name, args: rawArgs}
}

// Named tool calls of every assistant message in order
func assistantToolCalls(messages []any) []toolCall {
	var calls []toolCall
	for _, msg := range messages {
		if role, ok := messageRole(msg); !ok || role != "assistant" {
			continue
		}
		raw, _ := mappingGet(msg, "tool_calls")
		items, _ := listItems(raw)
		for _, tc := range items {
			if call := toolCallNameAndArgs(tc); call.name != "" {
				calls = append(calls, call)
			}
		}
	}
	return calls
}

// User, assistant and tool message counts among mapping messages
func countVisibleTurns(messages []any) (users, assistants, toolMessages int) {
	for _, msg := range messages {
		switch role, _ := messageRole(msg); role {
		case "user":
			users++
		case "assistant":
			assistants++
		case "tool":
			toolMessages++
		}
	}
	return users, assistants, toolMessages
}

// Most recent non empty stripped content text of a role, false when none has any
func latestText(messages []any, role string) (string, bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		got, ok := messageRole(messages[i])
		if !ok || got != role {
			continue
		}
		content, _ := mappingGet(messages[i], "content")
		if text := pystr.Strip(coerceText(content)); text != "" {
			return text, true
		}
	}
	return "", false
}

// Tail covering at most window user and assistant turns with tool messages riding along
func recentWindow(messages []any, window int) []any {
	count := 0
	for i := len(messages) - 1; i >= 0; i-- {
		if role, ok := messageRole(messages[i]); ok && (role == "user" || role == "assistant") {
			count++
			if count >= window {
				return slices.Clone(messages[i:])
			}
		}
	}
	return slices.Clone(messages)
}

// Path relative to the working directory when inside it, under a tilde when inside home, else absolute
func shortenedPath(path string) string {
	if path == "" {
		return path
	}
	absPath, err := pypath.Abspath(pypath.Expanduser(path))
	if err != nil {
		return path
	}
	cwd, err := pypath.Getcwd()
	if err != nil {
		return path
	}
	home := pypath.Expanduser("~")
	sep := string(os.PathSeparator)
	switch {
	case absPath == cwd:
		return "."
	case strings.HasPrefix(absPath, cwd+sep):
		return absPath[len(cwd)+1:]
	case strings.HasPrefix(absPath, home+sep):
		return "~/" + absPath[len(home)+1:]
	}
	return absPath
}

// Tool count with its name, the order a recap lists tools in
type toolCount struct {
	name  string
	count int
}

// Tool counts by descending count then name, and distinct touched files newest first
func summariseToolActivity(calls []toolCall) ([]toolCount, []string) {
	counts := map[string]int{}
	seen := map[string]bool{}
	var files []string
	for i := len(calls) - 1; i >= 0; i-- {
		call := calls[i]
		counts[call.name]++
		key, edits := fileEditTools[call.name]
		if !edits {
			continue
		}
		raw, _ := mappingGet(call.args, key)
		if path, ok := raw.(string); ok && path != "" && !seen[path] {
			seen[path] = true
			files = append(files, shortenedPath(path))
		}
	}
	sorted := make([]toolCount, 0, len(counts))
	for name, count := range counts {
		sorted = append(sorted, toolCount{name, count})
	}
	slices.SortFunc(sorted, func(a, b toolCount) int {
		if a.count != b.count {
			return b.count - a.count
		}
		return strings.Compare(a.name, b.name)
	})
	return sorted, files
}

// Comma joined first limit items with the count of the rest appended
func joinCapped(items []string, limit int) string {
	text := strings.Join(items[:min(limit, len(items))], ", ")
	if extra := len(items) - limit; extra > 0 {
		return fmt.Sprintf("%s (+%d more)", text, extra)
	}
	return text
}

// Sanitized one line text cut to limit characters with an ellipsis
func truncate(text string, limit int) string {
	text = strings.Join(pystr.SplitWhitespace(tools.SanitizeDisplayText(text), -1), " ")
	if pystr.Len(text) <= limit {
		return text
	}
	return pystr.Rstrip(pystr.Slice(text, 0, limit-1)) + "…"
}

// Plain multi line recap of recent session activity built from in memory history
func BuildRecap(messages []any, sessionTitle, sessionID string) string {
	lines := []string{"Session recap"}
	if sessionTitle != "" || sessionID != "" {
		label := sessionTitle
		if label == "" {
			label = pystr.Slice(sessionID, 0, 8)
		}
		lines[0] += " — " + label
	}
	if len(messages) == 0 {
		lines = append(lines, "  (nothing to recap — no messages yet)")
		return strings.Join(lines, "\n")
	}
	users, assistants, toolMessages := countVisibleTurns(messages)
	window := recentWindow(messages, recentTurnWindow)
	winUsers, winAssistants, _ := countVisibleTurns(window)
	scope := fmt.Sprintf("%d user turn%s / %d assistant repl%s", winUsers, plural(winUsers, "s", ""), winAssistants, plural(winAssistants, "ies", "y"))
	if users != winUsers || assistants != winAssistants {
		scope += fmt.Sprintf(" (of %d/%d total)", users, assistants)
	}
	lines = append(lines, fmt.Sprintf("  Recent: %s, %d tool result%s", scope, toolMessages, plural(toolMessages, "s", "")))
	counts, files := summariseToolActivity(assistantToolCalls(window))
	if len(counts) > 0 {
		labels := make([]string, len(counts))
		for i, c := range counts {
			labels[i] = fmt.Sprintf("%s×%d", c.name, c.count)
		}
		lines = append(lines, "  Tools used: "+joinCapped(labels, 5))
	}
	if len(files) > 0 {
		lines = append(lines, "  Files touched: "+joinCapped(files, maxFilesListed))
	}
	previews := []struct {
		role, label string
		limit       int
	}{{"user", "Last ask", promptPreviewChars}, {"assistant", "Last reply", assistantPreviewChars}}
	for _, p := range previews {
		if latest, ok := latestText(window, p.role); ok {
			lines = append(lines, fmt.Sprintf("  %s: %s", p.label, truncate(latest, p.limit)))
		}
	}
	if len(lines) == 2 {
		lines = append(lines, "  (no assistant activity yet in this window)")
	}
	return strings.Join(lines, "\n")
}

// Plural suffix for n other than one, singular suffix for exactly one
func plural(n int, many, one string) string {
	if n != 1 {
		return many
	}
	return one
}
