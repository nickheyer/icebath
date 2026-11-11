package acp

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/tools"
)

var toolsLog = logging.GetLogger("acp_adapter.tools")

// Hermes tool name to ACP tool kind, unlisted tools are other
var ToolKindMap = func() map[string]string {
	m := map[string]string{}
	for kind, names := range map[string][]string{
		"read":   {"read_file", "skill_view", "skills_list", "browser_snapshot", "browser_vision", "browser_get_images", "vision_analyze"},
		"edit":   {"write_file", "patch", "skill_manage"},
		"search": {"search_files"},
		"execute": {"terminal", "process", "execute_code", "browser_click", "browser_type", "browser_scroll",
			"browser_press", "browser_back", "delegate_task", "image_generate", "text_to_speech"},
		"fetch": {"web_search", "web_extract", "browser_navigate"},
		"other": {"todo"},
		"think": {"_thinking"},
	} {
		for _, n := range names {
			m[n] = kind
		}
	}
	return m
}()

var polishedTools = map[string]bool{}

func init() {
	for _, n := range strings.Fields(`todo memory session_search delegate_task read_file write_file patch search_files terminal process
		execute_code skill_view skills_list skill_manage web_search web_extract browser_navigate browser_click browser_type
		browser_press browser_scroll browser_back browser_snapshot browser_console browser_get_images browser_vision
		vision_analyze image_generate text_to_speech cronjob send_message clarify discord discord_admin feishu_doc_read
		feishu_drive_list_comments feishu_drive_list_comment_replies feishu_drive_reply_comment feishu_drive_add_comment
		kanban_create kanban_show kanban_comment kanban_complete kanban_block kanban_request_review kanban_request_changes
		kanban_link kanban_heartbeat yb_query_group_info yb_query_group_members yb_search_sticker yb_send_dm yb_send_sticker`) {
		polishedTools[n] = true
	}
}

// ACP tool kind of a hermes tool, other when unlisted
func GetToolKind(toolName string) string {
	if k, ok := ToolKindMap[toolName]; ok {
		return k
	}
	return "other"
}

// Fresh tool call id of tc- and twelve hex characters
func MakeToolCallID() string {
	return "tc-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
}

func textContent(content string) pyacp.ToolCallContent {
	return pyacp.ToolContent(pyacp.TextBlock(content))
}

func dget(d *pyjson.Object, key string) any {
	if d == nil {
		return nil
	}
	v, _ := d.Get(key)
	return v
}

func dhas(d *pyjson.Object, key string) bool {
	if d == nil {
		return false
	}
	_, ok := d.Get(key)
	return ok
}

// Value of key or def when the key is absent, the dict.get default form
func dgetOr(d *pyjson.Object, key string, def any) any {
	if dhas(d, key) {
		return dget(d, key)
	}
	return def
}

// First truthy value among keys, else def
func dfirst(d *pyjson.Object, def any, keys ...string) any {
	for _, k := range keys {
		if v := dget(d, k); truthy(v) {
			return v
		}
	}
	return def
}

// First truthy argument as a stripped string, else def
func argText(args *pyjson.Object, def string, keys ...string) string {
	if s := strings.TrimSpace(pyStr(dfirst(args, def, keys...))); s != "" {
		return s
	}
	return def
}

func strip(s string) string { return pystr.Strip(s) }

// Template with its braces replaced by str(value) when value is truthy, else fallback
func fmtValue(value any, template, fallback string) string {
	if !truthy(value) {
		return fallback
	}
	return strings.Replace(template, "{}", pyStr(value), 1)
}

func nonempty(result *string) (string, bool) {
	if result != nil && strip(*result) != "" {
		return *result, true
	}
	return "", false
}

func plural(count any, word, suffix string) string {
	s := ""
	if !pyrepr.Equal(count, int64(1)) {
		s = suffix
	}
	return pyStr(count) + " " + word + s
}

func failure(data *pyjson.Object, prefix string) (string, bool) {
	if dget(data, "success") == false || truthy(dget(data, "error")) {
		return prefix + ": " + pyStr(dgetOr(data, "error", "unknown error")), true
	}
	return "", false
}

func isList(v any) ([]any, bool) {
	l, ok := v.([]any)
	return l, ok
}

func isDict(v any) (*pyjson.Object, bool) {
	o, ok := v.(*pyjson.Object)
	return o, ok
}

// Reports whether v is a Python int, bools included
func isPyInt(v any) bool {
	switch v.(type) {
	case int64, int, *big.Int, bool:
		return true
	}
	return false
}

func pyIntValue(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case *big.Int:
		return x.Int64()
	case bool:
		if x {
			return 1
		}
	}
	return 0
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

func runePrefix(s string, n int) string { return pystr.Slice(s, 0, n) }

type formatter func(toolName string, result *string, args *pyjson.Object) (string, bool)

// Completion formatter over the parsed dict result, text fallback returning the raw text for non dict payloads
func structured(textFallback bool, fn func(toolName string, data, args *pyjson.Object) (string, bool)) formatter {
	return func(toolName string, result *string, args *pyjson.Object) (string, bool) {
		if data, ok := isDict(jsonLoadsMaybe(result)); ok {
			if args == nil {
				args = pyjson.NewObject()
			}
			return fn(toolName, data, args)
		}
		if textFallback {
			return nonempty(result)
		}
		return "", false
	}
}

// Tool call arguments as a dict, JSON text decoded or wrapped under raw, other values empty
func CoerceToolArgs(raw any) *pyjson.Object {
	if s, ok := raw.(string); ok {
		v, err := pyjson.Loads(s)
		if err != nil {
			o := pyjson.NewObject()
			o.Set("raw", s)
			return o
		}
		raw = v
	}
	switch x := raw.(type) {
	case *pyjson.Object:
		return x
	case map[string]any:
		o := pyjson.NewObject()
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			o.Set(k, x[k])
		}
		return o
	}
	return pyjson.NewObject()
}

func strDefault(v any) (any, error) { return pyStr(v), nil }

func argsJSON(arguments any) string {
	s, err := pyjson.Dumps(arguments, pyjson.Indent(2), pyjson.Default(strDefault))
	if err != nil {
		return pyStr(arguments)
	}
	return s
}

// Decoded JSON of a result string, its first JSON value when a hint follows, nil when undecodable
func jsonLoadsMaybe(value *string) any {
	if value == nil {
		return nil
	}
	if v, err := pyjson.Loads(*value); err == nil {
		return v
	}
	dec := json.NewDecoder(strings.NewReader(pystr.Lstrip(*value)))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil
	}
	v, err := pyjson.Loads(string(raw))
	if err != nil {
		return nil
	}
	return v
}

func truncateText(text string, limit int) string {
	n := runeLen(text)
	if n <= limit {
		return text
	}
	return runePrefix(text, max(0, limit-100)) + fmt.Sprintf("\n... (%d chars total, truncated)", n)
}

// Markdown fence longer than any backtick separated run inside text
func fencedText(text, language string) string {
	longest := 0
	parts := strings.Split(text, "`")
	for i := 1; i < len(parts); i += 2 {
		longest = max(longest, runeLen(parts[i]))
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + language + "\n" + text + "\n" + fence
}

// Reports whether a structured tool result clearly failed
func toolResultFailed(result *string, toolName string) bool {
	if result != nil && strings.HasPrefix(*result, "Error executing tool '") {
		return true
	}
	data, ok := isDict(jsonLoadsMaybe(result))
	if !ok {
		return false
	}
	exitCode := dgetOr(data, "exit_code", dget(data, "returncode"))
	if dget(data, "success") == false || dget(data, "ok") == false || (isPyInt(exitCode) && pyIntValue(exitCode) != 0) {
		return true
	}
	return polishedTools[toolName] && truthy(dget(data, "error")) && !truthy(dget(data, "content"))
}

// Tool name and its shared preview, the bare name when the arguments give none
func BuildToolTitle(toolName string, args *pyjson.Object) string {
	plain, _ := pyjson.Plain(args).(map[string]any)
	limit := 80
	preview := tools.BuildToolPreview(nil, toolName, plain, &limit)
	if preview != "" {
		return toolName + ": " + preview
	}
	return toolName
}

var todoIcons = map[string]string{"completed": "✅", "in_progress": "\U0001f504", "pending": "⏳", "cancelled": "✗"}

var formatTodoResult = structured(false, func(toolName string, data, args *pyjson.Object) (string, bool) {
	items, ok := isList(dget(data, "todos"))
	if !ok {
		return "", false
	}
	var todos []*pyjson.Object
	for _, t := range items {
		if o, ok := isDict(t); ok {
			todos = append(todos, o)
		}
	}
	byID := map[string]*pyjson.Object{}
	for _, t := range todos {
		byID[pyStr(pyrepr.Or(dget(t, "id"), ""))] = t
	}
	depth := func(item *pyjson.Object) int {
		d, seen := 0, map[string]bool{}
		for {
			parent := pyStr(pyrepr.Or(dget(item, "parent"), ""))
			if parent == "" || byID[parent] == nil || seen[parent] {
				break
			}
			seen[parent] = true
			d++
			item = byID[parent]
		}
		return min(d, 4)
	}
	lines := []string{"**Todo list**", ""}
	for _, item := range todos {
		content := strip(pyStr(pyrepr.Or(pyrepr.Or(dget(item, "content"), dget(item, "id")), "")))
		if content == "" {
			continue
		}
		icon, ok := todoIcons[pyStr(pyrepr.Or(dget(item, "status"), "pending"))]
		if !ok {
			icon = "•"
		}
		lines = append(lines, strings.Repeat("  ", depth(item))+"- "+icon+" "+content)
	}
	if summary, ok := isDict(dget(data, "summary")); ok && summary.Len() > 0 {
		cancelled := dgetOr(summary, "cancelled", int64(0))
		progress := fmt.Sprintf("%s completed, %s in progress, ", pyStr(dgetOr(summary, "completed", int64(0))), pyStr(dgetOr(summary, "in_progress", int64(0))))
		progress += pyStr(dgetOr(summary, "pending", int64(0))) + " pending"
		if truthy(cancelled) {
			progress += ", " + pyStr(cancelled) + " cancelled"
		}
		lines = append(lines, "", "**Progress:** "+progress)
	}
	return strings.Join(lines, "\n"), true
})

var formatReadFileResult = structured(false, func(toolName string, data, a *pyjson.Object) (string, bool) {
	if truthy(dget(data, "error")) && !truthy(dget(data, "content")) {
		return "Read failed: " + pyStr(dget(data, "error")), true
	}
	content, ok := dget(data, "content").(string)
	if !ok {
		return "", false
	}
	var bits []string
	for _, kl := range [][2]string{{"offset", "from line"}, {"limit", "limit"}} {
		if truthy(dget(a, kl[0])) {
			bits = append(bits, kl[1]+" "+pyStr(dget(a, kl[0])))
		}
	}
	header := "Read " + strip(pyStr(pyrepr.Or(pyrepr.Or(dget(a, "path"), dget(data, "path")), "file")))
	if len(bits) > 0 {
		header += " (" + strings.Join(bits, ", ") + ")"
	}
	if dget(data, "total_lines") != nil {
		header += " — " + pyStr(dget(data, "total_lines")) + " total lines"
	}
	return truncateText(header+"\n\n"+fencedText(content, ""), 5000), true
})

var formatSearchFilesResult = structured(false, func(toolName string, data, args *pyjson.Object) (string, bool) {
	if files, ok := isList(dget(data, "files")); ok {
		shown := min(len(files), 20)
		lines := []string{"File search results", fmt.Sprintf("Found %s; showing %d.", plural(dgetOr(data, "total_count", int64(len(files))), "file", "s"), shown), ""}
		for _, p := range files[:shown] {
			lines = append(lines, "- "+pyStr(p))
		}
		if truthy(dget(data, "truncated")) || len(files) > shown {
			lines = append(lines, "", "Results truncated. Narrow the search, add path/file_glob, or use offset to page.")
		}
		return truncateText(strings.Join(lines, "\n"), 7000), true
	}
	matches, ok := isList(dget(data, "matches"))
	if !ok {
		return "", false
	}
	shown := min(len(matches), 12)
	lines := []string{"Search results", fmt.Sprintf("Found %s; showing %d.", plural(dgetOr(data, "total_count", int64(len(matches))), "match", "es"), shown), ""}
	for _, m := range matches[:shown] {
		match, ok := isDict(m)
		if !ok {
			lines = append(lines, "- "+pyStr(m))
			continue
		}
		path := pyStr(dfirst(match, "?", "path", "file", "filename"))
		line := pyrepr.Or(dget(match, "line"), dget(match, "line_number"))
		if truthy(line) {
			lines = append(lines, "- "+path+":"+pyStr(line))
		} else {
			lines = append(lines, "- "+path)
		}
		if content := strip(pyStr(pyrepr.Or(pyrepr.Or(dget(match, "content"), dget(match, "text")), ""))); content != "" {
			lines = append(lines, "  "+truncateText(strings.Join(pystr.SplitWhitespace(content, -1), " "), 300))
		}
	}
	if truthy(dget(data, "truncated")) || len(matches) > shown {
		lines = append(lines, "", "Results truncated. Narrow the search, add file_glob, or use offset to page.")
	}
	return truncateText(strings.Join(lines, "\n"), 7000), true
})

var formatExecuteCodeResult = structured(true, func(toolName string, data, args *pyjson.Object) (string, bool) {
	parts := []string{"Execution complete"}
	if ec := dget(data, "exit_code"); ec != nil {
		parts[0] = "Exit code: " + pyStr(ec)
	}
	if truthy(dget(data, "stdout_truncated")) {
		total, captured, omitted := dget(data, "stdout_bytes_total"), dget(data, "stdout_bytes_captured"), dget(data, "stdout_bytes_omitted")
		if isPyInt(captured) && isPyInt(total) && isPyInt(omitted) {
			parts = append(parts, "", fmt.Sprintf("Output truncated: captured %s of %s bytes (%s omitted).",
				pystr.GroupThousands(pyIntValue(captured)), pystr.GroupThousands(pyIntValue(total)), pystr.GroupThousands(pyIntValue(omitted))))
		} else {
			parts = append(parts, "", "Output truncated.")
		}
	}
	for _, e := range []struct{ label, value string }{
		{"Warning:", strip(pyStr(pyrepr.Or(dget(data, "warning"), "")))},
		{"Output:", pyStr(pyrepr.Or(dget(data, "output"), ""))},
		{"Error:", pyStr(pyrepr.Or(dget(data, "error"), ""))},
	} {
		if e.value != "" {
			parts = append(parts, "", e.label, e.value)
		}
	}
	return truncateText(strings.Join(parts, "\n"), 5000), true
})

var formatSkillViewResult = structured(false, func(toolName string, data, args *pyjson.Object) (string, bool) {
	if dget(data, "success") == false {
		return "Skill view failed: " + pyStr(dgetOr(data, "error", "unknown error")), true
	}
	content := pyStr(pyrepr.Or(dget(data, "content"), ""))
	lines := []string{"**Skill loaded**", "", "- **Name:** `" + pyStr(pyrepr.Or(dget(data, "name"), "skill")) + "`",
		"- **File:** `" + pyStr(pyrepr.Or(pyrepr.Or(dget(data, "file"), dget(data, "path")), "SKILL.md")) + "`"}
	if d := strip(pyStr(pyrepr.Or(dget(data, "description"), ""))); d != "" {
		lines = append(lines, "- **Description:** "+d)
	}
	if content != "" {
		lines = append(lines, "- **Content:** "+pystr.GroupThousands(int64(runeLen(content)))+" chars loaded into agent context")
	}
	if linked, ok := isDict(dget(data, "linked_files")); ok && linked.Len() > 0 {
		n := 0
		for _, k := range linked.Keys {
			if l, ok := isList(linked.Vals[k]); ok {
				n += len(l)
			}
		}
		lines = append(lines, fmt.Sprintf("- **Linked files:** %d", n))
	}
	var headings []string
	for _, line := range pystr.Splitlines(content, false) {
		s := strip(line)
		if !strings.HasPrefix(s, "#") {
			continue
		}
		if h := strip(pystr.LstripChars(s, "#")); h != "" {
			headings = append(headings, h)
		}
	}
	if len(headings) > 8 {
		headings = headings[:8]
	}
	if len(headings) > 0 {
		lines = append(lines, "", "**Sections**")
		for _, h := range headings {
			lines = append(lines, "- "+h)
		}
	}
	lines = append(lines, "", "_Full skill content is available to the agent but hidden here to keep ACP readable._")
	return strings.Join(lines, "\n"), true
})

var formatSkillManageResult = structured(false, func(toolName string, data, a *pyjson.Object) (string, bool) {
	action := argText(a, "manage", "action")
	name := strip(pyStr(pyrepr.Or(pyrepr.Or(dget(a, "name"), dget(data, "name")), "skill")))
	if name == "" {
		name = "skill"
	}
	filePath := strip(pyStr(pyrepr.Or(pyrepr.Or(dget(a, "file_path"), dget(data, "file_path")), "SKILL.md")))
	if filePath == "" {
		filePath = "SKILL.md"
	}
	status := "✅ Skill updated"
	if dget(data, "success") == false {
		status = "✗ Skill update failed"
	}
	lines := []string{"**" + status + "**", "", "- **Action:** `" + action + "`", "- **Skill:** `" + name + "`"}
	if action != "delete" {
		lines = append(lines, "- **File:** `"+filePath+"`")
	}
	if msg := strip(pyStr(pyrepr.Or(pyrepr.Or(dget(data, "message"), dget(data, "error")), ""))); msg != "" {
		lines = append(lines, "- **Result:** "+msg)
	}
	if r := pyrepr.Or(dget(data, "replacements"), dget(data, "replacement_count")); r != nil {
		lines = append(lines, "- **Replacements:** "+pyStr(r))
	}
	if p := strip(pyStr(pyrepr.Or(dget(data, "path"), ""))); p != "" {
		lines = append(lines, "- **Path:** `"+p+"`")
	}
	return strings.Join(lines, "\n"), true
})

var formatWebSearchResult = structured(false, func(toolName string, data, args *pyjson.Object) (string, bool) {
	var web any
	if inner, ok := isDict(dget(data, "data")); ok {
		web = dget(inner, "web")
	} else {
		web = dget(data, "web")
	}
	items, ok := isList(web)
	if !ok {
		return "", false
	}
	lines := []string{fmt.Sprintf("Web results: %d", len(items))}
	for _, it := range items[:min(len(items), 10)] {
		item, ok := isDict(it)
		if !ok {
			continue
		}
		url := strip(pyStr(pyrepr.Or(dget(item, "url"), "")))
		line := "• " + strip(pyStr(pyrepr.Or(pyrepr.Or(dget(item, "title"), dget(item, "url")), "result")))
		if url != "" {
			line += " — " + url
		}
		lines = append(lines, line)
		if d := strip(pyStr(pyrepr.Or(dget(item, "description"), ""))); d != "" {
			lines = append(lines, "  "+d)
		}
	}
	return truncateText(strings.Join(lines, "\n"), 5000), true
})

var formatWebExtractResult = structured(false, func(toolName string, data, args *pyjson.Object) (string, bool) {
	if dget(data, "success") == false && truthy(dget(data, "error")) {
		return "Web extract failed: " + pyStr(dget(data, "error")), true
	}
	results, ok := isList(dget(data, "results"))
	if !ok {
		return "", false
	}
	var failures []string
	for _, it := range results[:min(len(results), 10)] {
		item, isD := isDict(it)
		errText := ""
		if isD {
			errText = strip(pyStr(pyrepr.Or(dget(item, "error"), "")))
		}
		if errText == "" || errText == "None" || errText == "null" {
			continue
		}
		url := strip(pyStr(pyrepr.Or(dget(item, "url"), "")))
		title := strip(pyStr(pyrepr.Or(pyrepr.Or(dget(item, "title"), url), "Untitled")))
		where := ""
		if url != "" && url != title {
			where = " — " + url
		}
		failures = append(failures, "- "+title+where+"\n  Error: "+truncateText(errText, 500))
	}
	if len(failures) == 0 {
		return "", false
	}
	return strings.Join(append([]string{"Web extract failed for " + plural(int64(len(failures)), "URL", "s")}, failures...), "\n"), true
})

func more(n, shown int, unit string) []string {
	if n > shown {
		return []string{fmt.Sprintf("... %d more%s", n-shown, unit)}
	}
	return nil
}

var formatProcessResult = structured(true, func(toolName string, data, args *pyjson.Object) (string, bool) {
	if dget(data, "success") == false && truthy(dget(data, "error")) {
		return "Process error: " + pyStr(dget(data, "error")), true
	}
	action := argText(args, "process", "action")
	if procs, ok := isList(dget(data, "processes")); ok {
		lines := []string{fmt.Sprintf("Processes: %d", len(procs))}
		for _, p := range procs[:min(len(procs), 20)] {
			proc, ok := isDict(p)
			if !ok {
				lines = append(lines, "- "+pyStr(p))
				continue
			}
			st := dget(proc, "status")
			if !truthy(st) {
				st = "running"
				if truthy(dget(proc, "exited")) {
					st = "exited"
				}
			}
			bits := []string{pyStr(st)}
			for _, kl := range [][2]string{{"pid", "pid"}, {"exit_code", "exit"}} {
				if dget(proc, kl[0]) != nil {
					bits = append(bits, kl[1]+" "+pyStr(dget(proc, kl[0])))
				}
			}
			cmd := strip(pyStr(pyrepr.Or(dget(proc, "command"), "")))
			line := "- `" + pyStr(dfirst(proc, "?", "session_id", "id")) + "` — " + strings.Join(bits, ", ")
			if cmd != "" {
				line += " — " + runePrefix(cmd, 120)
			}
			lines = append(lines, line)
		}
		return strings.Join(append(lines, more(len(procs), 20, " process(es)")...), "\n"), true
	}
	status := strip(pyStr(pyrepr.Or(pyrepr.Or(dget(data, "status"), dget(data, "state")), action)))
	sid := strip(pyStr(pyrepr.Or(pyrepr.Or(dget(data, "session_id"), dget(args, "session_id")), "")))
	head := "Process " + action + ": " + status
	if sid != "" {
		head += " (`" + sid + "`)"
	}
	lines := []string{head}
	for _, kl := range [][2]string{{"command", "Command"}, {"pid", "PID"}, {"exit_code", "Exit code"}, {"returncode", "Exit code"}, {"lines", "Lines"}} {
		if dget(data, kl[0]) != nil {
			lines = append(lines, "- **"+kl[1]+":** "+pyStr(dget(data, kl[0])))
		}
	}
	output := dfirst(data, nil, "output", "new_output", "log", "stdout")
	errV := dfirst(data, nil, "error", "stderr")
	for _, e := range []struct {
		label string
		value any
		limit int
	}{{"Output:", output, 5000}, {"Error:", errV, 2000}} {
		if truthy(e.value) {
			lines = append(lines, "", e.label, truncateText(pyStr(e.value), e.limit))
		}
	}
	if truthy(dget(data, "message")) && !truthy(output) && !truthy(errV) {
		lines = append(lines, pyStr(dget(data, "message")))
	}
	return truncateText(strings.Join(lines, "\n"), 7000), true
})

var delegateIcons = map[string]string{"completed": "✅", "failed": "✗", "error": "✗", "timeout": "⏱", "interrupted": "⚠"}

var formatDelegateResult = structured(false, func(toolName string, data, args *pyjson.Object) (string, bool) {
	results, ok := isList(dget(data, "results"))
	if !ok {
		if truthy(dget(data, "error")) {
			return "Delegation failed: " + pyStr(dget(data, "error")), true
		}
		return "", false
	}
	head := "Delegation results: " + plural(int64(len(results)), "task", "s")
	if total := dget(data, "total_duration_seconds"); total != nil {
		head += " in " + pyStr(total) + "s"
	}
	lines := []string{head}
	for _, it := range results {
		item, ok := isDict(it)
		if !ok {
			lines = append(lines, "- "+pyStr(it))
			continue
		}
		idx, status := dget(item, "task_index"), pyStr(pyrepr.Or(dget(item, "status"), "unknown"))
		icon, ok := delegateIcons[status]
		if !ok {
			icon = "•"
		}
		num := "?"
		if isPyInt(idx) {
			num = fmt.Sprint(pyIntValue(idx) + 1)
		}
		header := icon + " Task " + num + ": " + status
		var bits []string
		if truthy(dget(item, "model")) {
			bits = append(bits, pyStr(dget(item, "model")))
		}
		if truthy(dget(item, "_child_role")) {
			bits = append(bits, "role="+pyStr(dget(item, "_child_role")))
		}
		if dget(item, "duration_seconds") != nil {
			bits = append(bits, pyStr(dget(item, "duration_seconds"))+"s")
		}
		if len(bits) > 0 {
			header += " (" + strings.Join(bits, ", ") + ")"
		}
		lines = append(lines, "", header)
		for _, e := range []struct {
			key, prefix string
			limit       int
		}{{"summary", "", 1200}, {"error", "Error: ", 800}} {
			if v := strip(pyStr(pyrepr.Or(dget(item, e.key), ""))); v != "" {
				lines = append(lines, e.prefix+truncateText(v, e.limit))
			}
		}
		trace, _ := isList(dget(item, "tool_trace"))
		var names []string
		for _, t := range trace {
			if to, ok := isDict(t); ok {
				names = append(names, pyStr(pyrepr.Or(dget(to, "tool"), "?")))
			}
		}
		if len(names) > 0 {
			line := "Tools: " + strings.Join(names[:min(len(names), 12)], ", ")
			if len(names) > 12 {
				line += fmt.Sprintf(" (+%d)", len(names)-12)
			}
			lines = append(lines, line)
		}
	}
	return truncateText(strings.Join(lines, "\n"), 8000), true
})

var formatSessionSearchResult = structured(false, func(toolName string, data, args *pyjson.Object) (string, bool) {
	if dget(data, "success") == false {
		return "Session search failed: " + pyStr(dgetOr(data, "error", "unknown error")), true
	}
	results, ok := isList(dget(data, "results"))
	if !ok {
		return "", false
	}
	recent := pyrepr.Equal(pyrepr.Or(dget(data, "mode"), "search"), "recent")
	head := "Session search results" + fmtValue(dget(data, "query"), " for `{}`", "")
	if recent {
		head = "Recent sessions"
	}
	lines := []string{head}
	if len(results) == 0 {
		lines = append(lines, pyStr(pyrepr.Or(dget(data, "message"), "No matching sessions found.")))
		return strings.Join(lines, "\n"), true
	}
	for _, it := range results {
		item, ok := isDict(it)
		if !ok {
			continue
		}
		title := strip(pyStr(dfirst(item, "Untitled session", "title", "when")))
		when := strip(pyStr(dfirst(item, "", "last_active", "started_at", "when")))
		count := dget(item, "message_count")
		countText := ""
		if count != nil {
			countText = pyStr(count) + " msgs"
		}
		var metaBits []string
		for _, x := range []string{when, strip(pyStr(pyrepr.Or(dget(item, "source"), ""))), countText} {
			if x != "" {
				metaBits = append(metaBits, x)
			}
		}
		line := "- **" + title + "** (`" + pyStr(pyrepr.Or(dget(item, "session_id"), "?")) + "`)"
		if len(metaBits) > 0 {
			line += " — " + strings.Join(metaBits, ", ")
		}
		lines = append(lines, line)
		if summary := strip(pyStr(pyrepr.Or(pyrepr.Or(dget(item, "summary"), dget(item, "preview")), ""))); summary != "" {
			lines = append(lines, "  "+truncateText(strings.Join(pystr.SplitWhitespace(summary, -1), " "), 500))
		}
	}
	return truncateText(strings.Join(lines, "\n"), 7000), true
})

var formatMemoryResult = structured(false, func(toolName string, data, args *pyjson.Object) (string, bool) {
	action := argText(args, "memory", "action")
	target := pyStr(pyrepr.Or(pyrepr.Or(dget(data, "target"), dget(args, "target")), "memory"))
	if dget(data, "success") == false {
		lines := []string{"✗ Memory " + action + " failed (" + target + ")", pyStr(pyrepr.Or(dget(data, "error"), "unknown error"))}
		if matches, ok := isList(dget(data, "matches")); ok && len(matches) > 0 {
			lines = append(lines, "Matches:")
			for _, m := range matches[:min(len(matches), 5)] {
				lines = append(lines, "- "+truncateText(pyStr(m), 160))
			}
		}
		return strings.Join(lines, "\n"), true
	}
	lines := []string{"✅ Memory " + action + " saved (" + target + ")"}
	if truthy(dget(data, "message")) {
		lines = append(lines, pyStr(dget(data, "message")))
	}
	if dget(data, "entry_count") != nil {
		lines = append(lines, "Entries: "+pyStr(dget(data, "entry_count")))
	}
	if truthy(dget(data, "usage")) {
		lines = append(lines, "Usage: "+pyStr(dget(data, "usage")))
	}
	if preview := argText(args, "", "content", "old_text"); preview != "" {
		lines = append(lines, "Preview: "+truncateText(preview, 300))
	}
	return strings.Join(lines, "\n"), true
})

func formatEditResult(toolName string, result *string, args *pyjson.Object) (string, bool) {
	path := strip(pyStr(pyrepr.Or(dget(args, "path"), "file")))
	done := "✅ " + toolName + " completed"
	if path != "" {
		done += " for `" + path + "`"
	}
	data, ok := isDict(jsonLoadsMaybe(result))
	if !ok {
		if text, ok := nonempty(result); ok {
			return truncateText(text, 3000), true
		}
		return done, true
	}
	if failed, ok := failure(data, toolName+" failed for "+path); ok {
		return failed, true
	}
	lines := []string{done}
	if msg := strip(pyStr(pyrepr.Or(dget(data, "message"), ""))); msg != "" {
		lines = append(lines, msg)
	}
	if r := pyrepr.Or(dget(data, "replacements"), dget(data, "replacement_count")); r != nil {
		lines = append(lines, "Replacements: "+pyStr(r))
	}
	if files, ok := isList(dget(data, "files_modified")); ok && len(files) > 0 {
		var names []string
		for _, f := range files[:min(len(files), 8)] {
			names = append(names, "`"+pyStr(f)+"`")
		}
		lines = append(lines, "Files: "+strings.Join(names, ", "))
	}
	return strings.Join(lines, "\n"), true
}

var formatBrowserResult = structured(true, func(toolName string, data, args *pyjson.Object) (string, bool) {
	if failed, ok := failure(data, toolName+" failed"); ok {
		return failed, true
	}
	if toolName == "browser_get_images" {
		if images, ok := isList(pyrepr.Or(dget(data, "images"), dget(data, "data"))); ok {
			lines := []string{fmt.Sprintf("Images found: %d", len(images))}
			for _, it := range images[:min(len(images), 12)] {
				img, ok := isDict(it)
				if !ok {
					continue
				}
				url := strip(pyStr(pyrepr.Or(pyrepr.Or(dget(img, "url"), dget(img, "src")), "")))
				alt := strip(pyStr(pyrepr.Or(dget(img, "alt"), "")))
				if alt == "" {
					alt = "image"
				}
				line := "- " + alt
				if url != "" {
					line += " — " + url
				}
				lines = append(lines, line)
			}
			return truncateText(strings.Join(lines, "\n"), 5000), true
		}
	}
	title := pyStr(dfirst(data, toolName, "title", "url", "status"))
	lines := []string{title}
	if u := dget(data, "url"); truthy(u) && !pyrepr.Equal(u, title) {
		lines = append(lines, pyStr(u))
	}
	if text := strip(pyStr(dfirst(data, "", "text", "content", "snapshot", "analysis", "message"))); text != "" {
		lines = append(lines, "", truncateText(text, 5000))
	}
	return truncateText(strings.Join(lines, "\n"), 7000), true
})

var formatMediaOrCronResult = structured(true, func(toolName string, data, args *pyjson.Object) (string, bool) {
	if failed, ok := failure(data, toolName+" failed"); ok {
		return failed, true
	}
	lines := []string{"✅ " + toolName + " completed"}
	for _, k := range []string{"file_path", "path", "url", "image_url", "job_id", "id", "status", "message", "next_run"} {
		if truthy(dget(data, k)) {
			lines = append(lines, "- **"+k+":** "+pyStr(dget(data, k)))
		}
	}
	return strings.Join(lines, "\n"), true
})

// Reports whether v is None, an empty string, an empty list or an empty dict
func emptyish(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case *pyjson.Object:
		return x.Len() == 0
	}
	return false
}

func dumpsLoose(v any) string {
	s, err := pyjson.Dumps(v, pyjson.EnsureASCII(false), pyjson.Default(strDefault))
	if err != nil {
		return pyStr(v)
	}
	return s
}

// Nested JSON values as compact Markdown bullets bounded by depth and item caps
func formatStructuredValue(key string, value any, indent, maxDepth, maxItems int) []string {
	pad := strings.Repeat("  ", indent)
	bullet := pad + "- "
	label := ""
	if key != "" {
		label = "**" + key + ":**"
	}
	line := func(text string) string {
		if label != "" {
			return bullet + label + " " + text
		}
		return bullet + text
	}
	child := func(ck string, cv any, extra int) []string {
		return formatStructuredValue(ck, cv, indent+extra, maxDepth-1, maxItems)
	}
	if emptyish(value) {
		return nil
	}
	if maxDepth <= 0 {
		preview := pyStr(value)
		switch value.(type) {
		case *pyjson.Object, []any:
			preview = dumpsLoose(value)
		}
		return []string{line(truncateText(preview, 240))}
	}
	if obj, ok := isDict(value); ok {
		head := bullet + label
		if label == "" {
			head = fmt.Sprintf("%s%d fields", bullet, obj.Len())
		}
		lines := []string{head}
		shown := 0
		for _, ck := range obj.Keys {
			cv := obj.Vals[ck]
			if emptyish(cv) {
				continue
			}
			lines = append(lines, child(ck, cv, 1)...)
			shown++
			if shown >= maxItems {
				if obj.Len() > shown {
					lines = append(lines, fmt.Sprintf("%s  - ... %d more fields", pad, obj.Len()-shown))
				}
				break
			}
		}
		return lines
	}
	if list, ok := isList(value); ok {
		lines := []string{line(plural(int64(len(list)), "item", "s"))}
		for i, it := range list[:min(len(list), maxItems)] {
			idx := i + 1
			switch item := it.(type) {
			case *pyjson.Object:
				headline := strip(pyStr(dfirst(item, "", "content", "message", "title", "name", "id")))
				if headline != "" {
					lines = append(lines, fmt.Sprintf("%s  %d. %s", pad, idx, truncateText(headline, 220)))
					for _, ck := range []string{"id", "status", "type", "scope", "quality_score", "score", "path", "url"} {
						if !emptyish(dget(item, ck)) {
							lines = append(lines, fmt.Sprintf("%s    - **%s:** %s", pad, ck, truncateText(pyStr(dget(item, ck)), 180)))
						}
					}
				} else {
					lines = append(lines, fmt.Sprintf("%s  %d.", pad, idx))
					for _, ck := range item.Keys[:min(len(item.Keys), maxItems)] {
						lines = append(lines, child(ck, item.Vals[ck], 2)...)
					}
				}
			case []any:
				lines = append(lines, fmt.Sprintf("%s  %d. %d items", pad, idx, len(item)))
				for _, nested := range item[:min(len(item), maxItems)] {
					lines = append(lines, child("", nested, 2)...)
				}
			default:
				lines = append(lines, fmt.Sprintf("%s  %d. %s", pad, idx, truncateText(pyStr(it), 240)))
			}
		}
		if len(list) > maxItems {
			lines = append(lines, fmt.Sprintf("%s  ... %d more items", pad, len(list)-maxItems))
		}
		return lines
	}
	return []string{line(truncateText(pyStr(value), 500))}
}

var priorityKeys = []string{"message", "status", "id", "task_id", "issue_id", "title", "name", "entity_id",
	"state", "service", "url", "path", "file_path", "count", "total", "next_run"}

// Generic rendering of a dict or list result, raw text for non structured payloads when allowed
func formatGenericStructuredResult(toolName string, result *string, fallbackToText bool) (string, bool) {
	raw := jsonLoadsMaybe(result)
	if list, ok := isList(raw); ok {
		lines := []string{toolName + ": " + plural(int64(len(list)), "item", "s")}
		for _, it := range list[:min(len(list), 12)] {
			switch it.(type) {
			case *pyjson.Object, []any:
				lines = append(lines, formatStructuredValue("", it, 0, 2, 6)...)
			default:
				lines = append(lines, "- "+truncateText(pyStr(it), 240))
			}
		}
		return truncateText(strings.Join(append(lines, more(len(list), 12, " items")...), "\n"), 5000), true
	}
	data, ok := isDict(raw)
	if !ok {
		if fallbackToText {
			return nonempty(result)
		}
		return "", false
	}
	if failed, ok := failure(data, toolName+" failed"); ok {
		return failed, true
	}
	lines := []string{toolName + " result"}
	if dget(data, "success") == true {
		lines[0] = "✅ " + toolName + " completed"
	}
	seen := map[string]bool{}
	for _, k := range priorityKeys {
		if !emptyish(dget(data, k)) {
			seen[k] = true
			lines = append(lines, "- **"+k+":** "+truncateText(pyStr(dget(data, k)), 500))
		}
	}
	for _, k := range data.Keys {
		v := data.Vals[k]
		if seen[k] || k == "success" || k == "raw" || k == "content" || k == "entries" || emptyish(v) {
			continue
		}
		lines = append(lines, formatStructuredValue(k, v, 0, 3, 8)...)
		if len(lines) >= 40 {
			lines = append(lines, "- ... more fields truncated")
			break
		}
	}
	if c, ok := dget(data, "content").(string); ok && strip(c) != "" {
		lines = append(lines, "", truncateText(strip(c), 1500))
	}
	return truncateText(strings.Join(lines, "\n"), 7000), true
}

var completionFormatters = map[string]formatter{
	"todo": formatTodoResult, "read_file": formatReadFileResult, "write_file": formatEditResult, "patch": formatEditResult,
	"search_files": formatSearchFilesResult, "execute_code": formatExecuteCodeResult, "process": formatProcessResult,
	"delegate_task": formatDelegateResult, "session_search": formatSessionSearchResult, "memory": formatMemoryResult,
	"skill_view": formatSkillViewResult, "skill_manage": formatSkillManageResult, "web_search": formatWebSearchResult,
	"browser_navigate": formatBrowserResult, "browser_snapshot": formatBrowserResult, "browser_vision": formatBrowserResult,
	"browser_get_images": formatBrowserResult, "vision_analyze": formatMediaOrCronResult,
	"image_generate": formatMediaOrCronResult, "cronjob": formatMediaOrCronResult,
}

// ACP diff blocks of a unified diff, one per old and new file header pair
func parseUnifiedDiffContent(diffText string) []pyacp.ToolCallContent {
	var content []pyacp.ToolCallContent
	var oldPath, newPath *string
	var oldLines, newLines []string
	flush := func() {
		var path *string
		if newPath != nil && *newPath != "" && *newPath != "/dev/null" {
			path = newPath
		} else {
			path = oldPath
		}
		if path != nil && *path != "" && *path != "/dev/null" {
			p := strip(*path)
			if strings.HasPrefix(p, "a/") || strings.HasPrefix(p, "b/") {
				p = p[2:]
			}
			var oldText *string
			if len(oldLines) > 0 {
				oldText = pyacp.Ptr(strings.Join(oldLines, "\n"))
			}
			content = append(content, pyacp.ToolDiffContent(p, strings.Join(newLines, "\n"), oldText))
		}
		oldPath, newPath, oldLines, newLines = nil, nil, nil, nil
	}
	for _, line := range pystr.Splitlines(diffText, false) {
		switch {
		case strings.HasPrefix(line, "--- "):
			flush()
			oldPath = pyacp.Ptr(strip(line[4:]))
		case strings.HasPrefix(line, "+++ "):
			newPath = pyacp.Ptr(strip(line[4:]))
		case strings.HasPrefix(line, "@@") || (oldPath == nil && newPath == nil):
		case strings.HasPrefix(line, "+"):
			newLines = append(newLines, line[1:])
		case strings.HasPrefix(line, "-"):
			oldLines = append(oldLines, line[1:])
		case strings.HasPrefix(line, " "):
			oldLines = append(oldLines, line[1:])
			newLines = append(newLines, line[1:])
		}
	}
	flush()
	return content
}

// Structured completion content, skill_manage diffs first, falling back to plain text
func buildToolCompleteContent(toolName string, result *string, functionArgs *pyjson.Object, snapshot *tools.LocalEditSnapshot) []pyacp.ToolCallContent {
	if toolName == "skill_manage" {
		plain, _ := pyjson.Plain(functionArgs).(map[string]any)
		res := ""
		if result != nil {
			res = *result
		}
		if diff := safeExtractEditDiff(toolName, res, plain, snapshot); strip(diff) != "" {
			if dc := parseUnifiedDiffContent(diff); len(dc) > 0 {
				return dc
			}
		}
	}
	var text string
	var ok bool
	if f, found := completionFormatters[toolName]; found {
		text, ok = f(toolName, result, functionArgs)
	} else {
		text, ok = formatGenericStructuredResult(toolName, result, polishedTools[toolName])
	}
	if ok && text != "" {
		return []pyacp.ToolCallContent{textContent(text)}
	}
	r := ""
	if result != nil {
		r = *result
	}
	return []pyacp.ToolCallContent{textContent(truncateText(r, 5000))}
}

// Edit diff of a tool result, empty when the extraction panics
func safeExtractEditDiff(toolName, result string, args map[string]any, snapshot *tools.LocalEditSnapshot) (diff string) {
	defer func() {
		if recover() != nil {
			diff = ""
		}
	}()
	return tools.ExtractEditDiff(toolName, result, args, snapshot)
}

func startTodo(args *pyjson.Object) any {
	items, ok := isList(dget(args, "todos"))
	if !ok {
		return "Reading todo list"
	}
	lines := []string{"Updating todo list", ""}
	for _, it := range items[:min(len(items), 8)] {
		if i, ok := isDict(it); ok {
			lines = append(lines, "- "+pyStr(dgetOr(i, "status", "pending"))+": "+pyStr(dgetOr(i, "content", dgetOr(i, "id", ""))))
		}
	}
	return strings.Join(append(lines, more(len(items), 8, "")...), "\n")
}

func startSkillManage(args *pyjson.Object) any {
	action := argText(args, "manage", "action")
	name := argText(args, "?", "name")
	filePath := argText(args, "SKILL.md", "file_path")
	path := "skills/" + name + "/" + filePath
	switch action {
	case "patch":
		old := pyStr(pyrepr.Or(dget(args, "old_string"), ""))
		var oldText *string
		if old != "" {
			oldText = &old
		}
		return pyacp.ToolDiffContent(path, pyStr(pyrepr.Or(dget(args, "new_string"), "")), oldText)
	case "edit", "create":
		return pyacp.ToolDiffContent(path, pyStr(pyrepr.Or(dget(args, "content"), "")), nil)
	case "write_file":
		target := pyStr(pyrepr.Or(dget(args, "file_path"), "file"))
		return pyacp.ToolDiffContent("skills/"+name+"/"+target, pyStr(pyrepr.Or(dget(args, "file_content"), "")), nil)
	case "delete", "remove_file":
		return "Removing " + pyStr(pyrepr.Or(dget(args, "file_path"), filePath)) + " from skill '" + name + "'"
	}
	return "Running skill_manage action '" + action + "' on skill '" + name + "' (" + filePath + ")"
}

func startExecuteCode(args *pyjson.Object) any {
	code := argText(args, "", "code")
	preview := runePrefix(code, 1200)
	if n := runeLen(code); n > 1200 {
		preview += fmt.Sprintf("\n... (%d chars total, truncated)", n)
	}
	return fmtValue(preview, "Running Python helper script:\n\n```python\n{}\n```", "Running Python helper script")
}

func startDelegate(args *pyjson.Object) any {
	tasks, ok := isList(dget(args, "tasks"))
	if !ok || len(tasks) == 0 {
		return "Delegating task" + fmtValue(truncateText(argText(args, "", "goal"), 800), ":\n{}", "")
	}
	lines := []string{fmt.Sprintf("Delegating %d tasks", len(tasks)), ""}
	for i, t := range tasks[:min(len(tasks), 8)] {
		if task, ok := isDict(t); ok {
			lines = append(lines, fmt.Sprintf("%d. ", i+1)+truncateText(argText(task, "", "goal"), 160)+fmtValue(argText(task, "", "role"), " ({})", ""))
		}
	}
	return strings.Join(append(lines, more(len(tasks), 8, "")...), "\n")
}

func previewLine(label, value string, limit int) string {
	if value == "" {
		return ""
	}
	return "\n" + label + ": " + truncateText(value, limit)
}

var startContentBuilders = map[string]func(*pyjson.Object) any{
	"patch": func(a *pyjson.Object) any {
		return "Preparing " + pyStr(dgetOr(a, "mode", "replace")) + " edit for " + pyStr(pyrepr.Or(dget(a, "path"), "patch input")) + ". Approval prompt shows the diff."
	},
	"write_file": func(a *pyjson.Object) any {
		return fmtValue(dgetOr(a, "path", ""), "Preparing write to {}. Approval prompt shows the diff.", "Preparing file write. Approval prompt shows the diff.")
	},
	"terminal":  func(a *pyjson.Object) any { return "$ " + pyStr(dgetOr(a, "command", "")) },
	"read_file": nil,
	"search_files": func(a *pyjson.Object) any {
		return "Searching for '" + pyStr(dgetOr(a, "pattern", "")) + "' (" + pyStr(dgetOr(a, "target", "content")) + ")" + fmtValue(dget(a, "path"), " in {}", "")
	},
	"todo": startTodo,
	"skill_view": func(a *pyjson.Object) any {
		return "Loading skill '" + argText(a, "?", "name") + "' (" + argText(a, "SKILL.md", "file_path") + ")"
	},
	"skill_manage": startSkillManage,
	"execute_code": startExecuteCode,
	"web_search": func(a *pyjson.Object) any {
		return fmtValue(argText(a, "", "query"), "Searching the web for: {}", "Searching the web")
	},
	"web_extract": nil,
	"process": func(a *pyjson.Object) any {
		return "Process action: " + argText(a, "manage", "action") + fmtValue(argText(a, "", "session_id"), "\nSession: {}", "") +
			previewLine("Input", argText(a, "", "data"), 500)
	},
	"delegate_task": startDelegate,
	"session_search": func(a *pyjson.Object) any {
		return fmtValue(argText(a, "", "query"), "Searching past sessions for: {}", "Loading recent sessions")
	},
	"memory": func(a *pyjson.Object) any {
		return "Memory " + argText(a, "manage", "action") + " (" + argText(a, "memory", "target") + ")" +
			previewLine("Preview", argText(a, "", "content", "old_text"), 500)
	},
}

// Tool call start for a hermes tool, a minimal start when rendering the arguments fails
func BuildToolStart(toolCallID, toolName string, arguments *pyjson.Object, editDiff *tools.EditProposal) *pyacp.ToolCallStart {
	start, err := buildToolStartGuarded(toolCallID, toolName, arguments, editDiff)
	if err == nil {
		return start
	}
	toolsLog.Debug(fmt.Sprintf("ACP tool-start render failed for %s: %s", pyrepr.Repr(toolName), err))
	safe := toolName
	if safe == "" {
		safe = "tool"
	}
	return pyacp.StartToolCall(toolCallID, safe, pyacp.ToolCallFields{Kind: pyacp.Ptr(GetToolKind(safe)), Locations: []pyacp.ToolCallLocation{}})
}

func buildToolStartGuarded(toolCallID, toolName string, arguments *pyjson.Object, editDiff *tools.EditProposal) (start *pyacp.ToolCallStart, err error) {
	defer func() {
		if r := recover(); r != nil {
			start, err = nil, fmt.Errorf("%v", r)
		}
	}()
	if arguments == nil {
		arguments = pyjson.NewObject()
	}
	var content []pyacp.ToolCallContent
	var rawInput any
	builder, hasBuilder := startContentBuilders[toolName]
	switch {
	case (toolName == "patch" || toolName == "write_file") && editDiff != nil:
		content = []pyacp.ToolCallContent{pyacp.ToolDiffContent(editDiff.Path, editDiff.NewText, editDiff.OldText)}
	case hasBuilder:
		if builder != nil {
			switch built := builder(arguments).(type) {
			case string:
				content = []pyacp.ToolCallContent{textContent(built)}
			case pyacp.ToolCallContent:
				content = []pyacp.ToolCallContent{built}
			}
		}
	case polishedTools[toolName]:
		content = []pyacp.ToolCallContent{textContent(truncateText(argsJSON(arguments), 1200))}
	case arguments.Len() == 0:
	default:
		content = []pyacp.ToolCallContent{textContent(argsJSON(arguments))}
		rawInput = arguments
	}
	locations, err := ExtractLocations(arguments)
	if err != nil {
		return nil, err
	}
	return pyacp.StartToolCall(toolCallID, BuildToolTitle(toolName, arguments), pyacp.ToolCallFields{
		Kind: pyacp.Ptr(GetToolKind(toolName)), Content: content, Locations: locations, RawInput: rawInput,
	}), nil
}

// Tool call progress closing a completed call with its formatted result and status
func BuildToolComplete(toolCallID, toolName string, result *string, functionArgs *pyjson.Object, snapshot *tools.LocalEditSnapshot, isError bool) *pyacp.ToolCallProgress {
	var content []pyacp.ToolCallContent
	if toolName == "web_extract" {
		if text, ok := formatWebExtractResult(toolName, result, functionArgs); ok && text != "" {
			content = []pyacp.ToolCallContent{textContent(text)}
		}
	} else {
		content = buildToolCompleteContent(toolName, result, functionArgs, snapshot)
	}
	status := "completed"
	if isError || toolResultFailed(result, toolName) {
		status = "failed"
	}
	var rawOutput any
	switch jsonLoadsMaybe(result).(type) {
	case *pyjson.Object, []any:
	default:
		if !polishedTools[toolName] && result != nil {
			rawOutput = *result
		}
	}
	return pyacp.UpdateToolCall(toolCallID, pyacp.ToolCallFields{
		Kind: pyacp.Ptr(GetToolKind(toolName)), Status: pyacp.Ptr(status), Content: content, RawOutput: rawOutput,
	})
}

// Failed tool call progress for a call that ended without a result
func BuildToolAbandoned(toolCallID, toolName string) *pyacp.ToolCallProgress {
	return pyacp.UpdateToolCall(toolCallID, pyacp.ToolCallFields{
		Kind: pyacp.Ptr(GetToolKind(toolName)), Status: pyacp.Ptr("failed"),
		Content: []pyacp.ToolCallContent{textContent("This tool call ended without a result (blocked, denied, or interrupted).")},
	})
}

// File system locations of tool arguments, a path with its offset or line
func ExtractLocations(arguments *pyjson.Object) ([]pyacp.ToolCallLocation, error) {
	path := dget(arguments, "path")
	if !truthy(path) {
		return []pyacp.ToolCallLocation{}, nil
	}
	in := pyjson.NewObject()
	in.Set("path", path)
	in.Set("line", pyrepr.Or(dget(arguments, "offset"), dget(arguments, "line")))
	loc, err := pyacp.Validate[pyacp.ToolCallLocation](in)
	if err != nil {
		return nil, err
	}
	return []pyacp.ToolCallLocation{*loc}, nil
}
