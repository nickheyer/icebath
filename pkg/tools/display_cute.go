package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	errorSuffixMaxLen    = 48
	degradedSuffixMaxLen = 200
	unsafeFailureMarker  = "ROLLBACK FAILED"
)

// Tools whose refused calls render neutrally because the agent replans them itself
var NeutralFailureTools = map[string]bool{"skill_manage": true}

// Python exception a display renderer raises, recovered by the completion line fallback
type displayPyError struct{ class, msg string }

func (e displayPyError) Error() string { return e.class + ": " + e.msg }

func raiseDisplay(class, format string, args ...any) {
	panic(displayPyError{class: class, msg: fmt.Sprintf(format, args...)})
}

func displayT(ctx context.Context, key string, kwargs map[string]any) string {
	return i18n.T(ctx, key, "", kwargs)
}

// Decoded JSON of a string result with ints kept apart from floats, nil for undecodable or non string input
func displayJSONLoads(result any) any {
	s, ok := result.(string)
	if !ok {
		return nil
	}
	v, err := pyjson.DecodeWTF8Numbers([]byte(s))
	if err != nil {
		return nil
	}
	return v
}

// The result itself when it is a dict, else its decoded JSON
func displayData(result any) any {
	if m, ok := result.(map[string]any); ok {
		return m
	}
	return displayJSONLoads(result)
}

// Python equality of a value with the int zero
func pyEqualsZero(v any) bool {
	switch x := v.(type) {
	case bool:
		return !x
	case float64:
		return x == 0
	case float32:
		return x == 0
	case json.Number:
		if n, ok := pyrepr.Int(x); ok {
			return n.Sign() == 0
		}
		f, _ := x.Float64()
		return f == 0
	}
	n, ok := pyrepr.Int(v)
	return ok && n.Sign() == 0
}

// Python greater than zero comparison, raising TypeError for a non number
func pyGreaterThanZero(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x > 0
	case float32:
		return x > 0
	case json.Number:
		if n, ok := pyrepr.Int(x); ok {
			return n.Sign() > 0
		}
		f, _ := x.Float64()
		return f > 0
	}
	if n, ok := pyrepr.Int(v); ok {
		return n.Sign() > 0
	}
	raiseDisplay("TypeError", "'>' not supported between instances of '%s' and 'int'", pyrepr.TypeName(v))
	return false
}

// Python membership of item in a str, list or dict, raising TypeError for anything else
func displayContains(container any, item string) bool {
	switch c := container.(type) {
	case string:
		return strings.Contains(c, item)
	case map[string]any:
		_, ok := c[item]
		return ok
	}
	if list, ok := anyList(container); ok {
		for _, e := range list {
			if e == any(item) {
				return true
			}
		}
		return false
	}
	raiseDisplay("TypeError", "argument of type '%s' is not a container or iterable", pyrepr.TypeName(container))
	return false
}

// Python len of a str, list or dict, raising TypeError for anything else
func displayLen(v any) int {
	switch x := v.(type) {
	case string:
		return pystr.Len(x)
	case map[string]any:
		return len(x)
	}
	if list, ok := anyList(v); ok {
		return len(list)
	}
	raiseDisplay("TypeError", "object of type '%s' has no len()", pyrepr.TypeName(v))
	return 0
}

// Python isinstance int value including bool, ok false for any other type or an int beyond int range
func pyIntIndex(v any) (int, bool) {
	if b, ok := v.(bool); ok {
		if b {
			return 1, true
		}
		return 0, true
	}
	n, ok := pyrepr.Int(v)
	if !ok {
		return 0, false
	}
	if !n.IsInt64() || n.Int64() > math.MaxInt || n.Int64() < math.MinInt {
		return -1, true
	}
	return int(n.Int64()), true
}

// Shrinks an error for inline display, long File not found paths become the filename
func trimError(ctx context.Context, msg string) string {
	msg = pystr.Strip(msg)
	if strings.Contains(msg, "File not found:") {
		_, tail, _ := strings.Cut(msg, "File not found:")
		tail = pystr.Strip(tail)
		if i := strings.LastIndex(tail, "/"); i >= 0 {
			msg = displayT(ctx, "display.failure.file_not_found", map[string]any{"name": tail[i+1:]})
		}
	}
	return tailTrunc(msg, errorSuffixMaxLen)
}

// Bracketed reason and retry hint of a degraded terminal backend result
func degradedSuffix(ctx context.Context, data map[string]any) string {
	reason := data["reason"]
	if !pyrepr.Truthy(reason) {
		reason = data["error"]
	}
	if !pyrepr.Truthy(reason) {
		reason = displayT(ctx, "display.failure.terminal_backend_unavailable", nil)
	}
	text := pystr.Strip(pyrepr.Str(reason))
	if hint := data["retry_hint"]; pyrepr.Truthy(hint) {
		if h := pystr.Strip(pyrepr.Str(hint)); h != "" {
			text += " — " + h
		}
	}
	return " [" + tailTrunc(text, degradedSuffixMaxLen) + "]"
}

// Whether a failed call renders neutrally, a failed rollback always shows
func IsNeutralFailure(toolName string, result any) bool {
	if !NeutralFailureTools[toolName] {
		return false
	}
	errValue := result
	if m, ok := displayData(result).(map[string]any); ok {
		errValue = m["error"]
	}
	text := ""
	if pyrepr.Truthy(errValue) {
		text = pyrepr.Str(errValue)
	}
	return !strings.Contains(text, unsafeFailureMarker)
}

// Whether a tool result failed and the suffix its completion row shows
func DetectToolFailure(ctx context.Context, toolName string, result any) (bool, string) {
	if result == nil || FileMutationResultLanded(toolName, result) {
		return false, ""
	}
	data := displayData(result)
	if IsGuardrailRefusal(data) {
		return false, ""
	}
	m, isDict := data.(map[string]any)
	if isDict && pyrepr.Truthy(m["user_summary"]) {
		return true, " [" + tailTrunc(pyrepr.Str(m["user_summary"]), degradedSuffixMaxLen) + "]"
	}
	if toolName == "terminal" {
		var exitCode any
		if isDict {
			exitCode = m["exit_code"]
		}
		if exitCode == nil || pyEqualsZero(exitCode) {
			return false, ""
		}
		if m["status"] == any("degraded") {
			return true, degradedSuffix(ctx, m)
		}
		if errMsg := m["error"]; pyrepr.Truthy(errMsg) {
			return true, " [" + trimError(ctx, pyrepr.Str(errMsg)) + "]"
		}
		return true, displayT(ctx, "display.failure.exit_code", map[string]any{"code": exitCode})
	}
	if isDict {
		failed := m["success"] == any(false)
		if toolName == "memory" && failed {
			errValue, present := m["error"]
			if !present {
				errValue = ""
			}
			if displayContains(errValue, "exceed the limit") {
				return true, displayT(ctx, "display.failure.memory_full", nil)
			}
		}
		errValue := m["error"]
		if !pyrepr.Truthy(errValue) {
			errValue = m["message"]
		}
		_, hasError := m["error"]
		if pyrepr.Truthy(errValue) && (failed || hasError) {
			return true, " [" + trimError(ctx, pyrepr.Str(errValue)) + "]"
		}
		if shapeErr := ShapeFailureError(shapeFailureData(result, m)); shapeErr != "" {
			return true, " [" + trimError(ctx, shapeErr) + "]"
		}
		return false, ""
	}
	if s, ok := result.(string); ok && HasFailureMarker(s) {
		return true, displayT(ctx, "display.failure.generic", nil)
	}
	return false, ""
}

// The dict result as is, or its decoding in the float model the shape classifier reads
func shapeFailureData(result any, decoded map[string]any) map[string]any {
	if s, ok := result.(string); ok {
		if m, ok := safeJSONLoads(s).(map[string]any); ok {
			return m
		}
	}
	return decoded
}

func cuteDomain(url any) string {
	s, ok := url.(string)
	if !ok {
		raiseDisplay("AttributeError", "'%s' object has no attribute 'replace'", pyrepr.TypeName(url))
	}
	s = strings.ReplaceAll(strings.ReplaceAll(s, "https://", ""), "http://", "")
	host, _, _ := strings.Cut(s, "/")
	return host
}

// Tail truncates the str of a value to the configured preview cap, zero meaning unlimited
func cuteTrunc(s any) string {
	return tailTrunc(pyrepr.Str(s), GetToolPreviewMaxLen())
}

// Head truncates a path to the configured preview cap, keeping the filename end
func cutePath(p any) string {
	text := pyrepr.Str(p)
	limit := GetToolPreviewMaxLen()
	if limit == 0 || pystr.Len(text) <= limit {
		return text
	}
	if limit <= 3 {
		return strings.Repeat(".", limit)
	}
	runes := []rune(text)
	return "..." + string(runes[len(runes)-(limit-3):])
}

// Rendered completion row keeping its emoji, verb and detail for glyph layouts
type CuteRow struct {
	Text, Emoji, Verb, Detail string
}

// Row with the catalog verb padded to nine cells after the emoji
func cuteRow(ctx context.Context, emoji, verb, detail string) CuteRow {
	label := displayT(ctx, "display.cute.verb."+verb, nil)
	return CuteRow{Text: "┊ " + emoji + " " + pystr.Ljust(label, 9) + " " + detail, Emoji: pystr.Strip(emoji), Verb: label, Detail: detail}
}

// Row for a tool without a dedicated renderer, its emoji, its name and its preview
func cuteFallbackRow(ctx context.Context, env *Env, toolName string, args map[string]any) CuteRow {
	detail := cuteTrunc(BuildToolPreview(env, toolName, args, nil))
	emoji := GetToolEmoji(env, toolName, "⚡")
	return CuteRow{Text: "┊ " + emoji + " " + pystr.Ljust(pystr.Slice(toolName, 0, 9), 9) + " " + detail, Emoji: pystr.Strip(emoji), Verb: toolName, Detail: detail}
}

type cuteRenderer func(ctx context.Context, env *Env, a map[string]any, result any) CuteRow

func cuteWebExtract(ctx context.Context, _ *Env, a map[string]any, _ any) CuteRow {
	urls := getOr(a, "urls", []any{})
	list, isList := anyList(urls)
	url := ""
	if pyrepr.Truthy(urls) {
		if isList {
			url = displayURL(list[0])
		} else {
			url = displayURL(urls)
		}
	}
	if url == "" {
		return cuteRow(ctx, "📄", "fetch", displayT(ctx, "display.cute.fetch_pages", nil))
	}
	extra := ""
	if isList && len(list) > 1 {
		extra = fmt.Sprintf(" +%d", len(list)-1)
	}
	return cuteRow(ctx, "📄", "fetch", cuteTrunc(cuteDomain(url))+extra)
}

// Done and total counts from a todo_list result summary, zeros when the summary is unreadable
func planSummaryCounts(result any) (total, done any) {
	total, done = 0, 0
	if !pyrepr.Truthy(result) {
		return total, done
	}
	parsed := displayJSONLoads(result)
	if !pyrepr.Truthy(parsed) {
		return total, done
	}
	m, ok := parsed.(map[string]any)
	if !ok {
		return total, done
	}
	summary, ok := getOr(m, "summary", map[string]any{}).(map[string]any)
	if !ok {
		return total, done
	}
	return getOr(summary, "total", 0), getOr(summary, "completed", 0)
}

func cuteTodoList(ctx context.Context, _ *Env, a map[string]any, result any) CuteRow {
	todosArg := a["todos"]
	total, done := planSummaryCounts(result)
	progress := displayT(ctx, "display.cute.todo_progress", map[string]any{"done": done, "total": total})
	var detail string
	switch {
	case todosArg == nil:
		detail = displayT(ctx, "display.cute.todo_reading", nil)
		if pyGreaterThanZero(total) {
			detail = progress
		}
	case pyrepr.Truthy(getOr(a, "merge", false)):
		if pyGreaterThanZero(total) && pyGreaterThanZero(done) {
			detail = displayT(ctx, "display.cute.todo_update_progress", map[string]any{"done": done, "total": total})
		} else {
			detail = displayT(ctx, "display.cute.todo_update_count", map[string]any{"count": displayLen(todosArg)})
		}
	default:
		if pyGreaterThanZero(total) && pyGreaterThanZero(done) {
			detail = progress
		} else {
			detail = displayT(ctx, "display.cute.todo_count", map[string]any{"count": displayLen(todosArg)})
		}
	}
	return cuteRow(ctx, "📋", "plan", detail)
}

func cuteMemory(ctx context.Context, _ *Env, a map[string]any, _ any) CuteRow {
	op, more := memoryFirstOp(a)
	action, target := getOr(op, "action", "?"), getOr(a, "target", "")
	if action == any("add") {
		content := op["content"]
		if !pyrepr.Truthy(content) {
			content = op["new_text"]
		}
		if !pyrepr.Truthy(content) {
			content = ""
		}
		return cuteRow(ctx, "🧠", "memory", "+"+pyrepr.Str(target)+": \""+cuteTrunc(content)+"\""+more)
	}
	if action == any("replace") || action == any("patch") || action == any("remove") {
		old := op["old_text"]
		if !pyrepr.Truthy(old) {
			old = displayT(ctx, "display.preview.missing_old_text", nil)
		}
		sign := "~"
		if action == any("remove") {
			sign = "-"
		}
		return cuteRow(ctx, "🧠", "memory", sign+pyrepr.Str(target)+": \""+cuteTrunc(old)+"\""+more)
	}
	return cuteRow(ctx, "🧠", "memory", pyrepr.Str(action))
}

func cuteSkillView(ctx context.Context, _ *Env, a map[string]any, _ any) CuteRow {
	label, filePath := getOr(a, "name", ""), a["file_path"]
	if pyrepr.Truthy(filePath) {
		if pyrepr.Truthy(label) {
			label = pyrepr.Str(label) + " → " + pyrepr.Str(filePath)
		} else {
			label = pyrepr.Str(filePath)
		}
	}
	return cuteRow(ctx, "📚", "skill", cuteTrunc(label))
}

// Python subscript zero of a truthy value, raising for a dict or a non sequence
func pyFirstItem(v any) any {
	if s, ok := v.(string); ok {
		return pystr.Slice(s, 0, 1)
	}
	if list, ok := anyList(v); ok {
		return list[0]
	}
	if _, ok := v.(map[string]any); ok {
		raiseDisplay("KeyError", "0")
	}
	raiseDisplay("TypeError", "'%s' object is not subscriptable", pyrepr.TypeName(v))
	return nil
}

func cuteCronjob(ctx context.Context, _ *Env, a map[string]any, _ any) CuteRow {
	action := getOr(a, "action", "?")
	if action == any("create") {
		skills := a["skills"]
		if !pyrepr.Truthy(skills) {
			skills = []any{}
			if pyrepr.Truthy(a["skill"]) {
				skills = []any{a["skill"]}
			}
		}
		label := a["name"]
		if !pyrepr.Truthy(label) {
			label = nil
			if pyrepr.Truthy(skills) {
				label = pyFirstItem(skills)
			}
		}
		if !pyrepr.Truthy(label) {
			label = getOr(a, "prompt", displayT(ctx, "display.cute.cron_task_fallback", nil))
		}
		return cuteRow(ctx, "⏰", "cron", displayT(ctx, "display.cute.cron_create", map[string]any{"label": cuteTrunc(label)}))
	}
	if action == any("list") {
		return cuteRow(ctx, "⏰", "cron", displayT(ctx, "display.cute.cron_listing", nil))
	}
	return cuteRow(ctx, "⏰", "cron", pyrepr.Str(action)+" "+pyrepr.Str(getOr(a, "job_id", "")))
}

func cuteExecuteCode(ctx context.Context, _ *Env, a map[string]any, _ any) CuteRow {
	raw := getOr(a, "code", "")
	code, ok := raw.(string)
	if !ok {
		raiseDisplay("AttributeError", "'%s' object has no attribute 'strip'", pyrepr.TypeName(raw))
	}
	code = pystr.Strip(code)
	first, _, _ := strings.Cut(code, "\n")
	return cuteRow(ctx, "🐍", "exec", cuteTrunc(first))
}

func cuteBrowserExec(ctx context.Context, _ *Env, a map[string]any, _ any) CuteRow {
	if label, ok := browserExecStepLabel(a, 80); ok {
		return cuteRow(ctx, "🌐", "browser", label)
	}
	code := getOr(a, "code", "")
	text := ""
	if pyrepr.Truthy(code) {
		text = pyrepr.Str(code)
	}
	return cuteRow(ctx, "🌐", "browser", cuteTrunc(oneline(text)))
}

func cuteDelegate(ctx context.Context, _ *Env, a map[string]any, _ any) CuteRow {
	actionPreview, hasAction := delegateActionPreview(a)
	tasks := a["tasks"]
	if hasAction {
		return cuteRow(ctx, "🔀", "delegate", cuteTrunc(actionPreview))
	}
	if list, ok := tasks.([]any); ok && len(list) > 0 {
		goals := delegateTaskGoals(tasks, 30)
		joined := cuteTrunc(displayT(ctx, "display.cute.delegate_parallel", nil))
		if len(goals) > 0 {
			joined = cuteTrunc(strings.Join(goals, " | "))
		}
		count := len(goals)
		if count == 0 {
			count = len(list)
		}
		return cuteRow(ctx, "🔀", "delegate", fmt.Sprintf("%dx: %s", count, joined))
	}
	return cuteRow(ctx, "🔀", "delegate", cuteTrunc(getOr(a, "goal", "")))
}

// Python slice of the first twelve items of a str or list, raising for anything unsubscriptable
func pyHead12(v any) string {
	if s, ok := v.(string); ok {
		return pystr.Slice(s, 0, 12)
	}
	if list, ok := v.([]any); ok {
		return pyrepr.Str(list[:min(12, len(list))])
	}
	raiseDisplay("TypeError", "'%s' object is not subscriptable", pyrepr.TypeName(v))
	return ""
}

func cuteProcessManage(ctx context.Context, _ *Env, a map[string]any, _ any) CuteRow {
	action, sid := getOr(a, "action", "?"), pyHead12(getOr(a, "session_id", ""))
	if action == any("list") {
		return cuteRow(ctx, "⚙️ ", "proc", displayT(ctx, "display.cute.process_list", nil))
	}
	return cuteRow(ctx, "⚙️ ", "proc", pyrepr.Str(action)+" "+sid)
}

var scrollArrows = map[string]string{"down": "↓", "up": "↑", "right": "→", "left": "←"}

func cuteScroll(ctx context.Context, _ *Env, a map[string]any, _ any) CuteRow {
	direction := getOr(a, "direction", "down")
	switch direction.(type) {
	case []any, map[string]any:
		raiseDisplay("TypeError", "unhashable type: '%s'", pyrepr.TypeName(direction))
	}
	name, isStr := direction.(string)
	arrow, known := scrollArrows[name]
	if !isStr || !known {
		return cuteRow(ctx, "↓ ", "scroll", pyrepr.Str(direction))
	}
	return cuteRow(ctx, arrow+" ", "scroll", displayT(ctx, "display.cute.scroll."+name, nil))
}

// Primary argument row, the tool preview when it has one
func cutePreviewRow(emoji, verb, tool, key string) cuteRenderer {
	return func(ctx context.Context, env *Env, a map[string]any, _ any) CuteRow {
		var detail any = BuildToolPreview(env, tool, a, nil)
		if detail == "" {
			detail = getOr(a, key, "")
		}
		return cuteRow(ctx, emoji, verb, cuteTrunc(detail))
	}
}

// Row whose detail is the truncated argument under key
func cuteArgRow(emoji, verb, key string, def any) cuteRenderer {
	return func(ctx context.Context, _ *Env, a map[string]any, _ any) CuteRow {
		return cuteRow(ctx, emoji, verb, cuteTrunc(getOr(a, key, def)))
	}
}

// Row whose detail is the str of the argument under key
func cuteStrRow(emoji, verb, key string, def any) cuteRenderer {
	return func(ctx context.Context, _ *Env, a map[string]any, _ any) CuteRow {
		return cuteRow(ctx, emoji, verb, pyrepr.Str(getOr(a, key, def)))
	}
}

// Row whose detail is a fixed catalog string
func cuteCatalogRow(emoji, verb, key string) cuteRenderer {
	return func(ctx context.Context, _ *Env, _ map[string]any, _ any) CuteRow {
		return cuteRow(ctx, emoji, verb, displayT(ctx, key, nil))
	}
}

var cuteLines map[string]cuteRenderer

func init() {
	cuteLines = map[string]cuteRenderer{
		"web_search":     cuteArgRow("🔍", "search", "query", ""),
		"web_extract":    cuteWebExtract,
		"terminal":       cutePreviewRow("💻", "terminal", "terminal", "command"),
		"process_manage": cuteProcessManage,
		"read_file":      cutePreviewRow("📖", "read", "read_file", "path"),
		"write_file": func(ctx context.Context, _ *Env, a map[string]any, _ any) CuteRow {
			return cuteRow(ctx, "✍️ ", "write", cutePath(getOr(a, "path", "")))
		},
		"patch": func(ctx context.Context, _ *Env, a map[string]any, _ any) CuteRow {
			return cuteRow(ctx, "🔧", "patch", cutePath(getOr(a, "path", "")))
		},
		"search_files": func(ctx context.Context, _ *Env, a map[string]any, _ any) CuteRow {
			verb := "grep"
			if getOr(a, "target", "content") == any("files") {
				verb = "find"
			}
			return cuteRow(ctx, "🔎", verb, cuteTrunc(getOr(a, "pattern", "")))
		},
		"browser_navigate": func(ctx context.Context, _ *Env, a map[string]any, _ any) CuteRow {
			return cuteRow(ctx, "🌐", "navigate", cuteTrunc(cuteDomain(getOr(a, "url", ""))))
		},
		"browser_snapshot": func(ctx context.Context, _ *Env, a map[string]any, _ any) CuteRow {
			key := "display.cute.snapshot_compact"
			if pyrepr.Truthy(a["full"]) {
				key = "display.cute.snapshot_full"
			}
			return cuteRow(ctx, "📸", "snapshot", displayT(ctx, key, nil))
		},
		"browser_click": cuteStrRow("👆", "click", "ref", "?"),
		"browser_type": func(ctx context.Context, _ *Env, a map[string]any, _ any) CuteRow {
			return cuteRow(ctx, "⌨️ ", "type", "\""+cuteTrunc(getOr(a, "text", ""))+"\"")
		},
		"browser_scroll": cuteScroll,
		"browser_back": func(ctx context.Context, _ *Env, _ map[string]any, _ any) CuteRow {
			return cuteRow(ctx, "◀️ ", "back", "")
		},
		"browser_press":      cuteStrRow("⌨️ ", "press", "key", "?"),
		"browser_get_images": cuteCatalogRow("🖼️ ", "images", "display.cute.images_extracting"),
		"browser_vision":     cuteCatalogRow("👁️ ", "vision", "display.cute.vision_analyzing_page"),
		"todo_list":          cuteTodoList,
		"session_search": func(ctx context.Context, _ *Env, a map[string]any, _ any) CuteRow {
			return cuteRow(ctx, "🔍", "recall", "\""+cuteTrunc(getOr(a, "query", ""))+"\"")
		},
		"memory": cuteMemory,
		"skills_list": func(ctx context.Context, _ *Env, a map[string]any, _ any) CuteRow {
			category := a["category"]
			if !pyrepr.Truthy(category) {
				category = displayT(ctx, "display.cute.skills_all", nil)
			}
			return cuteRow(ctx, "📚", "skills", displayT(ctx, "display.cute.skills_list", map[string]any{"category": category}))
		},
		"skill_view":     cuteSkillView,
		"image_generate": cuteArgRow("🎨", "create", "prompt", ""),
		"text_to_speech": func(ctx context.Context, _ *Env, a map[string]any, _ any) CuteRow {
			if voices, ok := a["find_voices"].(map[string]any); ok {
				query := voices["query"]
				if !pyrepr.Truthy(query) {
					query = "browse"
				}
				return cuteRow(ctx, "🗣️ ", "voices", cuteTrunc(query))
			}
			return cuteRow(ctx, "🔊", "speak", cuteTrunc(getOr(a, "text", "")))
		},
		"vision_analyze": cuteArgRow("👁️ ", "vision", "question", ""),
		"send_message": func(ctx context.Context, _ *Env, a map[string]any, _ any) CuteRow {
			return cuteRow(ctx, "📨", "send", pyrepr.Str(getOr(a, "target", "?"))+": \""+cuteTrunc(getOr(a, "message", ""))+"\"")
		},
		"cronjob_manage": cuteCronjob,
		"execute_code":   cuteExecuteCode,
		"browser_exec":   cuteBrowserExec,
		"delegate_task":  cuteDelegate,
	}
}

// One row per bridge label, ok false when the call has no bridge labels
func cuteBridgeRows(env *Env, toolName string, args map[string]any) ([]string, bool) {
	labels := BridgeToolLabels(env, toolName, args)
	if len(labels) == 0 {
		return nil, false
	}
	rows := make([]string, len(labels))
	for i, label := range labels {
		rows[i] = "┊ " + label.Emoji + " " + cuteTrunc(label.Text())
		if label.Preview != "" {
			rows[i] += "  " + cuteTrunc(label.Preview)
		}
	}
	return rows, true
}

var bridgeCallIndexRe = regexp.MustCompile(`calls\[(\p{Nd}+)\]`)

// Failure suffix of one bridge batch entry, empty when the entry succeeded
func entryFailureSuffix(ctx context.Context, entry any) string {
	m, ok := entry.(map[string]any)
	if !ok {
		return ""
	}
	errValue := m["error"]
	if em, ok := errValue.(map[string]any); ok {
		msg := em["message"]
		if !pyrepr.Truthy(msg) {
			msg = em["code"]
		}
		if !pyrepr.Truthy(msg) {
			msg = displayT(ctx, "display.failure.generic_word", nil)
		}
		return " [" + trimError(ctx, pyrepr.Str(msg)) + "]"
	}
	if !pyrepr.Truthy(errValue) && m["success"] != any(false) {
		return ""
	}
	name := ""
	if pyrepr.Truthy(m["name"]) {
		name = pyrepr.Str(m["name"])
	}
	_, suffix := DetectToolFailure(ctx, name, m)
	return suffix
}

// Per row failure suffixes of a bridge call, the call suffix landing on the entry it names
func bridgeRowSuffixes(ctx context.Context, rows int, result any, callSuffix string) []string {
	suffixes := make([]string, rows)
	if data, ok := displayData(result).(map[string]any); ok {
		if entries, ok := data["results"].([]any); ok {
			for position, entry := range entries[:min(rows, len(entries))] {
				suffix := entryFailureSuffix(ctx, entry)
				if suffix == "" {
					continue
				}
				target := position
				if index, ok := pyIntIndex(entry.(map[string]any)["index"]); ok && index >= 0 && index < rows {
					target = index
				}
				suffixes[target] = suffix
			}
		}
	}
	if callSuffix != "" && strings.Join(suffixes, "") == "" {
		named := rows - 1
		if match := bridgeCallIndexRe.FindStringSubmatch(callSuffix); match != nil {
			named = -1
			if n, ok := pystr.IntText(match[1]); ok && n.IsInt64() && n.Int64() < int64(rows) {
				named = int(n.Int64())
			}
		}
		if named < 0 || named >= rows {
			named = rows - 1
		}
		suffixes[named] = callSuffix
	}
	return suffixes
}

// The active skin's tool output prefix, the dotted bar without a skin
func GetSkinToolPrefix(ctx context.Context) any {
	if skin := config.GetActiveSkin(ctx); skin != nil {
		return skin.ToolPrefix
	}
	return "┊"
}

// Swapped by tests to make the renderer raise
var renderCuteToolMessage = cuteToolMessage

// Completion rows with the duration on the last row and the skin prefix in place of the bar
func cuteToolMessage(ctx context.Context, env *Env, toolName string, args map[string]any, duration float64, result any) string {
	if redacted := RedactToolArgsForDisplay(env, toolName, args); len(redacted) > 0 {
		args = redacted
	}
	isFailure, failureSuffix := DetectToolFailure(ctx, toolName, result)
	callSuffix := ""
	if isFailure {
		callSuffix = failureSuffix
	}
	render := cuteLines[toolName]
	rows, bridged := cuteBridgeRows(env, toolName, args)
	var suffixes []string
	if !bridged {
		body := cuteFallbackRow
		if render != nil {
			body = func(ctx context.Context, env *Env, _ string, a map[string]any) CuteRow {
				return render(ctx, env, a, result)
			}
		}
		rows, suffixes = []string{body(ctx, env, toolName, args).Text}, []string{callSuffix}
	} else {
		suffixes = bridgeRowSuffixes(ctx, len(rows), result, callSuffix)
	}
	elapsed, err := pyrepr.Format(duration, ".1f")
	if err != nil {
		panic(err)
	}
	rows[len(rows)-1] += "  " + elapsed + "s"
	prefix, ok := GetSkinToolPrefix(ctx).(string)
	if !ok {
		raiseDisplay("TypeError", "replace() argument 2 must be str, not %s", pyrepr.TypeName(GetSkinToolPrefix(ctx)))
	}
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = strings.Replace(row+suffixes[i], "┊", prefix, 1)
	}
	return strings.Join(out, "\n  ")
}

// Completion line of one tool call, a plain completed row when rendering raises
func GetCuteToolMessage(ctx context.Context, env *Env, toolName string, args map[string]any, duration float64, result any) (message string) {
	defer func() {
		rec := recover()
		if rec == nil {
			return
		}
		slog.Debug(fmt.Sprintf("Tool completion label failed for %s: %v", toolName, rec))
		safeName := displayT(ctx, "display.cute.fallback_tool_name", nil)
		if toolName != "" {
			safeName = pystr.Slice(toolName, 0, 9)
		}
		safeDuration, err := pyrepr.Format(duration, ".1f")
		if err != nil {
			safeDuration = displayT(ctx, "display.cute.fallback_done", nil)
		} else {
			safeDuration += "s"
		}
		message = displayT(ctx, "display.cute.completed", map[string]any{"tool": pystr.Ljust(safeName, 9), "duration": safeDuration})
	}()
	return renderCuteToolMessage(ctx, env, toolName, args, duration, result)
}

// One completed call row without padding, Failure nil when the row succeeded
type ToolRowPart struct {
	Emoji, Verb, Detail string
	Failure             *string
}

// The emoji, verb, detail and failure of each row one completed call shows
func GetToolRowParts(ctx context.Context, env *Env, toolName string, args map[string]any, result any, isError bool) (parts []ToolRowPart) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Debug(fmt.Sprintf("Tool row parts failed for %s", toolName), "error", rec)
			parts = []ToolRowPart{{Emoji: "⚡", Verb: toolName}}
		}
	}()
	if redacted := RedactToolArgsForDisplay(env, toolName, args); len(redacted) > 0 {
		args = redacted
	} else if len(args) == 0 {
		args = map[string]any{}
	}
	isFailure, suffix := DetectToolFailure(ctx, toolName, result)
	var callFailure *string
	if isFailure {
		callFailure = &suffix
	} else if isError {
		empty := ""
		callFailure = &empty
	}
	if labels := BridgeToolLabels(env, toolName, args); len(labels) > 0 {
		callSuffix := ""
		if isFailure {
			callSuffix = suffix
		}
		suffixes := bridgeRowSuffixes(ctx, len(labels), result, callSuffix)
		marks := make([]*string, len(suffixes))
		for i := range suffixes {
			if suffixes[i] != "" {
				marks[i] = &suffixes[i]
			}
		}
		if callFailure != nil && strings.Join(suffixes, "") == "" {
			marks[len(marks)-1] = callFailure
		}
		parts = make([]ToolRowPart, len(labels))
		for i, label := range labels {
			detail := ""
			if label.Preview != "" {
				detail = cuteTrunc(label.Preview)
			}
			parts[i] = ToolRowPart{Emoji: label.Emoji, Verb: cuteTrunc(label.Text()), Detail: detail, Failure: marks[i]}
		}
		return parts
	}
	var row CuteRow
	if render := cuteLines[toolName]; render != nil {
		row = render(ctx, env, args, result)
	} else {
		row = cuteFallbackRow(ctx, env, toolName, args)
	}
	return []ToolRowPart{{Emoji: row.Emoji, Verb: row.Verb, Detail: row.Detail, Failure: callFailure}}
}
