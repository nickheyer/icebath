package acp

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/tools"
	acputil "github.com/nickheyer/icebath/pkg/utils/acp"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Tool name to ACP tool kind, unlisted tools are other
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

// ACP tool kind of a tool, other when unlisted
func GetToolKind(toolName string) string {
	if k, ok := ToolKindMap[toolName]; ok {
		return k
	}
	return "other"
}

func textContent(content string) acputil.ToolCallContent {
	return acputil.ToolContent(acputil.TextBlock(content))
}

func dget(d *jsonutil.Object, key string) any {
	if d == nil {
		return nil
	}
	v, _ := d.Get(key)
	return v
}

func dhas(d *jsonutil.Object, key string) bool {
	if d == nil {
		return false
	}
	_, ok := d.Get(key)
	return ok
}

// Value of key or def when the key is absent, the dict.get default form
func dgetOr(d *jsonutil.Object, key string, def any) any {
	if dhas(d, key) {
		return dget(d, key)
	}
	return def
}

// First truthy value among keys, else def
func dfirst(d *jsonutil.Object, def any, keys ...string) any {
	for _, k := range keys {
		if v := dget(d, k); anyutil.IsTrue(v) {
			return v
		}
	}
	return def
}

// First truthy argument as a stripped string, else def
func argText(args *jsonutil.Object, def string, keys ...string) string {
	if s := strip(anyutil.String(dfirst(args, def, keys...))); s != "" {
		return s
	}
	return def
}

func strip(s string) string { return strings.TrimSpace(s) }

// Template with its braces replaced by str(value) when value is truthy, else fallback
func fmtValue(value any, template, fallback string) string {
	if !anyutil.IsTrue(value) {
		return fallback
	}
	return strings.Replace(template, "{}", anyutil.String(value), 1)
}

func nonempty(result *string) (string, bool) {
	if result != nil && strip(*result) != "" {
		return *result, true
	}
	return "", false
}

func plural(count any, word, suffix string) string {
	s := ""
	if !anyutil.Equal(count, int64(1)) {
		s = suffix
	}
	return anyutil.String(count) + " " + word + s
}

func failure(data *jsonutil.Object, prefix string) (string, bool) {
	if dget(data, "success") == false || anyutil.IsTrue(dget(data, "error")) {
		return prefix + ": " + anyutil.String(dgetOr(data, "error", "unknown error")), true
	}
	return "", false
}

func isList(v any) ([]any, bool) {
	l, ok := v.([]any)
	return l, ok
}

func isDict(v any) (*jsonutil.Object, bool) {
	o, ok := v.(*jsonutil.Object)
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

func runePrefix(s string, n int) string { return textutil.First(s, n) }

type formatter func(toolName string, result *string, args *jsonutil.Object) (string, bool)

// Completion formatter over the parsed dict result, text fallback returning the raw text for non dict payloads
func structured(textFallback bool, fn func(toolName string, data, args *jsonutil.Object) (string, bool)) formatter {
	return func(toolName string, result *string, args *jsonutil.Object) (string, bool) {
		if data, ok := isDict(jsonLoadsMaybe(result)); ok {
			if args == nil {
				args = jsonutil.NewObject()
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
func CoerceToolArgs(raw any) *jsonutil.Object {
	if s, ok := raw.(string); ok {
		v, err := jsonutil.Decode(s)
		if err != nil {
			o := jsonutil.NewObject()
			o.Set("raw", s)
			return o
		}
		raw = v
	}
	switch x := raw.(type) {
	case *jsonutil.Object:
		return x
	case map[string]any:
		o := jsonutil.NewObject()
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
	return jsonutil.NewObject()
}

func strDefault(v any) (any, error) { return anyutil.String(v), nil }

func argsJSON(arguments any) string {
	s, err := jsonutil.Encode(arguments, jsonutil.Indent(2), jsonutil.Convert(strDefault), jsonutil.ASCII())
	if err != nil {
		return anyutil.String(arguments)
	}
	return s
}

// Decoded JSON of a result string, its first JSON value when a hint follows, nil when undecodable
func jsonLoadsMaybe(value *string) any {
	if value == nil {
		return nil
	}
	if v, err := jsonutil.Decode(*value); err == nil {
		return v
	}
	dec := json.NewDecoder(strings.NewReader(strings.TrimLeftFunc(*value, unicode.IsSpace)))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil
	}
	v, err := jsonutil.Decode(string(raw))
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
	return polishedTools[toolName] && anyutil.IsTrue(dget(data, "error")) && !anyutil.IsTrue(dget(data, "content"))
}

// Tool name and its shared preview, the bare name when the arguments give none
func BuildToolTitle(toolName string, args *jsonutil.Object) string {
	plain, _ := jsonutil.Plain(args).(map[string]any)
	limit := 80
	preview := tools.BuildToolPreview(nil, toolName, plain, &limit)
	if preview != "" {
		return toolName + ": " + preview
	}
	return toolName
}

var todoIcons = map[string]string{"completed": "✅", "in_progress": "\U0001f504", "pending": "⏳", "cancelled": "✗"}

var formatTodoResult = structured(false, func(toolName string, data, args *jsonutil.Object) (string, bool) {
	items, ok := isList(dget(data, "todos"))
	if !ok {
		return "", false
	}
	var todos []*jsonutil.Object
	for _, t := range items {
		if o, ok := isDict(t); ok {
			todos = append(todos, o)
		}
	}
	byID := map[string]*jsonutil.Object{}
	for _, t := range todos {
		byID[anyutil.String(anyutil.Or(dget(t, "id"), ""))] = t
	}
	depth := func(item *jsonutil.Object) int {
		d, seen := 0, map[string]bool{}
		for {
			parent := anyutil.String(anyutil.Or(dget(item, "parent"), ""))
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
		content := strip(anyutil.String(anyutil.Or(anyutil.Or(dget(item, "content"), dget(item, "id")), "")))
		if content == "" {
			continue
		}
		icon, ok := todoIcons[anyutil.String(anyutil.Or(dget(item, "status"), "pending"))]
		if !ok {
			icon = "•"
		}
		lines = append(lines, strings.Repeat("  ", depth(item))+"- "+icon+" "+content)
	}
	if summary, ok := isDict(dget(data, "summary")); ok && summary.Len() > 0 {
		cancelled := dgetOr(summary, "cancelled", int64(0))
		progress := fmt.Sprintf("%s completed, %s in progress, ", anyutil.String(dgetOr(summary, "completed", int64(0))), anyutil.String(dgetOr(summary, "in_progress", int64(0))))
		progress += anyutil.String(dgetOr(summary, "pending", int64(0))) + " pending"
		if anyutil.IsTrue(cancelled) {
			progress += ", " + anyutil.String(cancelled) + " cancelled"
		}
		lines = append(lines, "", "**Progress:** "+progress)
	}
	return strings.Join(lines, "\n"), true
})

var formatReadFileResult = structured(false, func(toolName string, data, a *jsonutil.Object) (string, bool) {
	if anyutil.IsTrue(dget(data, "error")) && !anyutil.IsTrue(dget(data, "content")) {
		return "Read failed: " + anyutil.String(dget(data, "error")), true
	}
	content, ok := dget(data, "content").(string)
	if !ok {
		return "", false
	}
	var bits []string
	for _, kl := range [][2]string{{"offset", "from line"}, {"limit", "limit"}} {
		if anyutil.IsTrue(dget(a, kl[0])) {
			bits = append(bits, kl[1]+" "+anyutil.String(dget(a, kl[0])))
		}
	}
	header := "Read " + strip(anyutil.String(anyutil.Or(anyutil.Or(dget(a, "path"), dget(data, "path")), "file")))
	if len(bits) > 0 {
		header += " (" + strings.Join(bits, ", ") + ")"
	}
	if dget(data, "total_lines") != nil {
		header += " — " + anyutil.String(dget(data, "total_lines")) + " total lines"
	}
	return truncateText(header+"\n\n"+fencedText(content, ""), 5000), true
})

var formatSearchFilesResult = structured(false, func(toolName string, data, args *jsonutil.Object) (string, bool) {
	if files, ok := isList(dget(data, "files")); ok {
		shown := min(len(files), 20)
		lines := []string{"File search results", fmt.Sprintf("Found %s; showing %d.", plural(dgetOr(data, "total_count", int64(len(files))), "file", "s"), shown), ""}
		for _, p := range files[:shown] {
			lines = append(lines, "- "+anyutil.String(p))
		}
		if anyutil.IsTrue(dget(data, "truncated")) || len(files) > shown {
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
			lines = append(lines, "- "+anyutil.String(m))
			continue
		}
		path := anyutil.String(dfirst(match, "?", "path", "file", "filename"))
		line := anyutil.Or(dget(match, "line"), dget(match, "line_number"))
		if anyutil.IsTrue(line) {
			lines = append(lines, "- "+path+":"+anyutil.String(line))
		} else {
			lines = append(lines, "- "+path)
		}
		if content := strip(anyutil.String(anyutil.Or(anyutil.Or(dget(match, "content"), dget(match, "text")), ""))); content != "" {
			lines = append(lines, "  "+truncateText(strings.Join(strings.Fields(content), " "), 300))
		}
	}
	if anyutil.IsTrue(dget(data, "truncated")) || len(matches) > shown {
		lines = append(lines, "", "Results truncated. Narrow the search, add file_glob, or use offset to page.")
	}
	return truncateText(strings.Join(lines, "\n"), 7000), true
})

var formatExecuteCodeResult = structured(true, func(toolName string, data, args *jsonutil.Object) (string, bool) {
	parts := []string{"Execution complete"}
	if ec := dget(data, "exit_code"); ec != nil {
		parts[0] = "Exit code: " + anyutil.String(ec)
	}
	if anyutil.IsTrue(dget(data, "stdout_truncated")) {
		total, captured, omitted := dget(data, "stdout_bytes_total"), dget(data, "stdout_bytes_captured"), dget(data, "stdout_bytes_omitted")
		if isPyInt(captured) && isPyInt(total) && isPyInt(omitted) {
			parts = append(parts, "", fmt.Sprintf("Output truncated: captured %s of %s bytes (%s omitted).",
				textutil.Thousands(pyIntValue(captured)), textutil.Thousands(pyIntValue(total)), textutil.Thousands(pyIntValue(omitted))))
		} else {
			parts = append(parts, "", "Output truncated.")
		}
	}
	for _, e := range []struct{ label, value string }{
		{"Warning:", strip(anyutil.String(anyutil.Or(dget(data, "warning"), "")))},
		{"Output:", anyutil.String(anyutil.Or(dget(data, "output"), ""))},
		{"Error:", anyutil.String(anyutil.Or(dget(data, "error"), ""))},
	} {
		if e.value != "" {
			parts = append(parts, "", e.label, e.value)
		}
	}
	return truncateText(strings.Join(parts, "\n"), 5000), true
})

var formatSkillViewResult = structured(false, func(toolName string, data, args *jsonutil.Object) (string, bool) {
	if dget(data, "success") == false {
		return "Skill view failed: " + anyutil.String(dgetOr(data, "error", "unknown error")), true
	}
	content := anyutil.String(anyutil.Or(dget(data, "content"), ""))
	lines := []string{"**Skill loaded**", "", "- **Name:** `" + anyutil.String(anyutil.Or(dget(data, "name"), "skill")) + "`",
		"- **File:** `" + anyutil.String(anyutil.Or(anyutil.Or(dget(data, "file"), dget(data, "path")), "SKILL.md")) + "`"}
	if d := strip(anyutil.String(anyutil.Or(dget(data, "description"), ""))); d != "" {
		lines = append(lines, "- **Description:** "+d)
	}
	if content != "" {
		lines = append(lines, "- **Content:** "+textutil.Thousands(int64(runeLen(content)))+" chars loaded into agent context")
	}
	if linked, ok := isDict(dget(data, "linked_files")); ok && linked.Len() > 0 {
		n := 0
		for _, k := range linked.Keys() {
			if l, ok := isList(linked.Value(k)); ok {
				n += len(l)
			}
		}
		lines = append(lines, fmt.Sprintf("- **Linked files:** %d", n))
	}
	var headings []string
	for _, line := range textutil.Lines(content) {
		s := strip(line)
		if !strings.HasPrefix(s, "#") {
			continue
		}
		if h := strip(strings.TrimLeft(s, "#")); h != "" {
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

var formatSkillManageResult = structured(false, func(toolName string, data, a *jsonutil.Object) (string, bool) {
	action := argText(a, "manage", "action")
	name := strip(anyutil.String(anyutil.Or(anyutil.Or(dget(a, "name"), dget(data, "name")), "skill")))
	if name == "" {
		name = "skill"
	}
	filePath := strip(anyutil.String(anyutil.Or(anyutil.Or(dget(a, "file_path"), dget(data, "file_path")), "SKILL.md")))
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
	if msg := strip(anyutil.String(anyutil.Or(anyutil.Or(dget(data, "message"), dget(data, "error")), ""))); msg != "" {
		lines = append(lines, "- **Result:** "+msg)
	}
	if r := anyutil.Or(dget(data, "replacements"), dget(data, "replacement_count")); r != nil {
		lines = append(lines, "- **Replacements:** "+anyutil.String(r))
	}
	if p := strip(anyutil.String(anyutil.Or(dget(data, "path"), ""))); p != "" {
		lines = append(lines, "- **Path:** `"+p+"`")
	}
	return strings.Join(lines, "\n"), true
})

var formatWebSearchResult = structured(false, func(toolName string, data, args *jsonutil.Object) (string, bool) {
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
		url := strip(anyutil.String(anyutil.Or(dget(item, "url"), "")))
		line := "• " + strip(anyutil.String(anyutil.Or(anyutil.Or(dget(item, "title"), dget(item, "url")), "result")))
		if url != "" {
			line += " — " + url
		}
		lines = append(lines, line)
		if d := strip(anyutil.String(anyutil.Or(dget(item, "description"), ""))); d != "" {
			lines = append(lines, "  "+d)
		}
	}
	return truncateText(strings.Join(lines, "\n"), 5000), true
})

var formatWebExtractResult = structured(false, func(toolName string, data, args *jsonutil.Object) (string, bool) {
	if dget(data, "success") == false && anyutil.IsTrue(dget(data, "error")) {
		return "Web extract failed: " + anyutil.String(dget(data, "error")), true
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
			errText = strip(anyutil.String(anyutil.Or(dget(item, "error"), "")))
		}
		if errText == "" || errText == "None" || errText == "null" {
			continue
		}
		url := strip(anyutil.String(anyutil.Or(dget(item, "url"), "")))
		title := strip(anyutil.String(anyutil.Or(anyutil.Or(dget(item, "title"), url), "Untitled")))
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

var formatProcessResult = structured(true, func(toolName string, data, args *jsonutil.Object) (string, bool) {
	if dget(data, "success") == false && anyutil.IsTrue(dget(data, "error")) {
		return "Process error: " + anyutil.String(dget(data, "error")), true
	}
	action := argText(args, "process", "action")
	if procs, ok := isList(dget(data, "processes")); ok {
		lines := []string{fmt.Sprintf("Processes: %d", len(procs))}
		for _, p := range procs[:min(len(procs), 20)] {
			proc, ok := isDict(p)
			if !ok {
				lines = append(lines, "- "+anyutil.String(p))
				continue
			}
			st := dget(proc, "status")
			if !anyutil.IsTrue(st) {
				st = "running"
				if anyutil.IsTrue(dget(proc, "exited")) {
					st = "exited"
				}
			}
			bits := []string{anyutil.String(st)}
			for _, kl := range [][2]string{{"pid", "pid"}, {"exit_code", "exit"}} {
				if dget(proc, kl[0]) != nil {
					bits = append(bits, kl[1]+" "+anyutil.String(dget(proc, kl[0])))
				}
			}
			cmd := strip(anyutil.String(anyutil.Or(dget(proc, "command"), "")))
			line := "- `" + anyutil.String(dfirst(proc, "?", "session_id", "id")) + "` — " + strings.Join(bits, ", ")
			if cmd != "" {
				line += " — " + runePrefix(cmd, 120)
			}
			lines = append(lines, line)
		}
		return strings.Join(append(lines, more(len(procs), 20, " process(es)")...), "\n"), true
	}
	status := strip(anyutil.String(anyutil.Or(anyutil.Or(dget(data, "status"), dget(data, "state")), action)))
	sid := strip(anyutil.String(anyutil.Or(anyutil.Or(dget(data, "session_id"), dget(args, "session_id")), "")))
	head := "Process " + action + ": " + status
	if sid != "" {
		head += " (`" + sid + "`)"
	}
	lines := []string{head}
	for _, kl := range [][2]string{{"command", "Command"}, {"pid", "PID"}, {"exit_code", "Exit code"}, {"returncode", "Exit code"}, {"lines", "Lines"}} {
		if dget(data, kl[0]) != nil {
			lines = append(lines, "- **"+kl[1]+":** "+anyutil.String(dget(data, kl[0])))
		}
	}
	output := dfirst(data, nil, "output", "new_output", "log", "stdout")
	errV := dfirst(data, nil, "error", "stderr")
	for _, e := range []struct {
		label string
		value any
		limit int
	}{{"Output:", output, 5000}, {"Error:", errV, 2000}} {
		if anyutil.IsTrue(e.value) {
			lines = append(lines, "", e.label, truncateText(anyutil.String(e.value), e.limit))
		}
	}
	if anyutil.IsTrue(dget(data, "message")) && !anyutil.IsTrue(output) && !anyutil.IsTrue(errV) {
		lines = append(lines, anyutil.String(dget(data, "message")))
	}
	return truncateText(strings.Join(lines, "\n"), 7000), true
})

var delegateIcons = map[string]string{"completed": "✅", "failed": "✗", "error": "✗", "timeout": "⏱", "interrupted": "⚠"}

var formatDelegateResult = structured(false, func(toolName string, data, args *jsonutil.Object) (string, bool) {
	results, ok := isList(dget(data, "results"))
	if !ok {
		if anyutil.IsTrue(dget(data, "error")) {
			return "Delegation failed: " + anyutil.String(dget(data, "error")), true
		}
		return "", false
	}
	head := "Delegation results: " + plural(int64(len(results)), "task", "s")
	if total := dget(data, "total_duration_seconds"); total != nil {
		head += " in " + anyutil.String(total) + "s"
	}
	lines := []string{head}
	for _, it := range results {
		item, ok := isDict(it)
		if !ok {
			lines = append(lines, "- "+anyutil.String(it))
			continue
		}
		idx, status := dget(item, "task_index"), anyutil.String(anyutil.Or(dget(item, "status"), "unknown"))
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
		if anyutil.IsTrue(dget(item, "model")) {
			bits = append(bits, anyutil.String(dget(item, "model")))
		}
		if anyutil.IsTrue(dget(item, "_child_role")) {
			bits = append(bits, "role="+anyutil.String(dget(item, "_child_role")))
		}
		if dget(item, "duration_seconds") != nil {
			bits = append(bits, anyutil.String(dget(item, "duration_seconds"))+"s")
		}
		if len(bits) > 0 {
			header += " (" + strings.Join(bits, ", ") + ")"
		}
		lines = append(lines, "", header)
		for _, e := range []struct {
			key, prefix string
			limit       int
		}{{"summary", "", 1200}, {"error", "Error: ", 800}} {
			if v := strip(anyutil.String(anyutil.Or(dget(item, e.key), ""))); v != "" {
				lines = append(lines, e.prefix+truncateText(v, e.limit))
			}
		}
		trace, _ := isList(dget(item, "tool_trace"))
		var names []string
		for _, t := range trace {
			if to, ok := isDict(t); ok {
				names = append(names, anyutil.String(anyutil.Or(dget(to, "tool"), "?")))
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

var formatSessionSearchResult = structured(false, func(toolName string, data, args *jsonutil.Object) (string, bool) {
	if dget(data, "success") == false {
		return "Session search failed: " + anyutil.String(dgetOr(data, "error", "unknown error")), true
	}
	results, ok := isList(dget(data, "results"))
	if !ok {
		return "", false
	}
	recent := anyutil.Equal(anyutil.Or(dget(data, "mode"), "search"), "recent")
	head := "Session search results" + fmtValue(dget(data, "query"), " for `{}`", "")
	if recent {
		head = "Recent sessions"
	}
	lines := []string{head}
	if len(results) == 0 {
		lines = append(lines, anyutil.String(anyutil.Or(dget(data, "message"), "No matching sessions found.")))
		return strings.Join(lines, "\n"), true
	}
	for _, it := range results {
		item, ok := isDict(it)
		if !ok {
			continue
		}
		title := strip(anyutil.String(dfirst(item, "Untitled session", "title", "when")))
		when := strip(anyutil.String(dfirst(item, "", "last_active", "started_at", "when")))
		count := dget(item, "message_count")
		countText := ""
		if count != nil {
			countText = anyutil.String(count) + " msgs"
		}
		var metaBits []string
		for _, x := range []string{when, strip(anyutil.String(anyutil.Or(dget(item, "source"), ""))), countText} {
			if x != "" {
				metaBits = append(metaBits, x)
			}
		}
		line := "- **" + title + "** (`" + anyutil.String(anyutil.Or(dget(item, "session_id"), "?")) + "`)"
		if len(metaBits) > 0 {
			line += " — " + strings.Join(metaBits, ", ")
		}
		lines = append(lines, line)
		if summary := strip(anyutil.String(anyutil.Or(anyutil.Or(dget(item, "summary"), dget(item, "preview")), ""))); summary != "" {
			lines = append(lines, "  "+truncateText(strings.Join(strings.Fields(summary), " "), 500))
		}
	}
	return truncateText(strings.Join(lines, "\n"), 7000), true
})

var formatMemoryResult = structured(false, func(toolName string, data, args *jsonutil.Object) (string, bool) {
	action := argText(args, "memory", "action")
	target := anyutil.String(anyutil.Or(anyutil.Or(dget(data, "target"), dget(args, "target")), "memory"))
	if dget(data, "success") == false {
		lines := []string{"✗ Memory " + action + " failed (" + target + ")", anyutil.String(anyutil.Or(dget(data, "error"), "unknown error"))}
		if matches, ok := isList(dget(data, "matches")); ok && len(matches) > 0 {
			lines = append(lines, "Matches:")
			for _, m := range matches[:min(len(matches), 5)] {
				lines = append(lines, "- "+truncateText(anyutil.String(m), 160))
			}
		}
		return strings.Join(lines, "\n"), true
	}
	lines := []string{"✅ Memory " + action + " saved (" + target + ")"}
	if anyutil.IsTrue(dget(data, "message")) {
		lines = append(lines, anyutil.String(dget(data, "message")))
	}
	if dget(data, "entry_count") != nil {
		lines = append(lines, "Entries: "+anyutil.String(dget(data, "entry_count")))
	}
	if anyutil.IsTrue(dget(data, "usage")) {
		lines = append(lines, "Usage: "+anyutil.String(dget(data, "usage")))
	}
	if preview := argText(args, "", "content", "old_text"); preview != "" {
		lines = append(lines, "Preview: "+truncateText(preview, 300))
	}
	return strings.Join(lines, "\n"), true
})

func formatEditResult(toolName string, result *string, args *jsonutil.Object) (string, bool) {
	path := strip(anyutil.String(anyutil.Or(dget(args, "path"), "file")))
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
	if msg := strip(anyutil.String(anyutil.Or(dget(data, "message"), ""))); msg != "" {
		lines = append(lines, msg)
	}
	if r := anyutil.Or(dget(data, "replacements"), dget(data, "replacement_count")); r != nil {
		lines = append(lines, "Replacements: "+anyutil.String(r))
	}
	if files, ok := isList(dget(data, "files_modified")); ok && len(files) > 0 {
		var names []string
		for _, f := range files[:min(len(files), 8)] {
			names = append(names, "`"+anyutil.String(f)+"`")
		}
		lines = append(lines, "Files: "+strings.Join(names, ", "))
	}
	return strings.Join(lines, "\n"), true
}

var formatBrowserResult = structured(true, func(toolName string, data, args *jsonutil.Object) (string, bool) {
	if failed, ok := failure(data, toolName+" failed"); ok {
		return failed, true
	}
	if toolName == "browser_get_images" {
		if images, ok := isList(anyutil.Or(dget(data, "images"), dget(data, "data"))); ok {
			lines := []string{fmt.Sprintf("Images found: %d", len(images))}
			for _, it := range images[:min(len(images), 12)] {
				img, ok := isDict(it)
				if !ok {
					continue
				}
				url := strip(anyutil.String(anyutil.Or(anyutil.Or(dget(img, "url"), dget(img, "src")), "")))
				alt := strip(anyutil.String(anyutil.Or(dget(img, "alt"), "")))
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
	title := anyutil.String(dfirst(data, toolName, "title", "url", "status"))
	lines := []string{title}
	if u := dget(data, "url"); anyutil.IsTrue(u) && !anyutil.Equal(u, title) {
		lines = append(lines, anyutil.String(u))
	}
	if text := strip(anyutil.String(dfirst(data, "", "text", "content", "snapshot", "analysis", "message"))); text != "" {
		lines = append(lines, "", truncateText(text, 5000))
	}
	return truncateText(strings.Join(lines, "\n"), 7000), true
})

var formatMediaOrCronResult = structured(true, func(toolName string, data, args *jsonutil.Object) (string, bool) {
	if failed, ok := failure(data, toolName+" failed"); ok {
		return failed, true
	}
	lines := []string{"✅ " + toolName + " completed"}
	for _, k := range []string{"file_path", "path", "url", "image_url", "job_id", "id", "status", "message", "next_run"} {
		if anyutil.IsTrue(dget(data, k)) {
			lines = append(lines, "- **"+k+":** "+anyutil.String(dget(data, k)))
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
	case *jsonutil.Object:
		return x.Len() == 0
	}
	return false
}

func dumpsLoose(v any) string {
	s, err := jsonutil.Encode(v, jsonutil.Convert(strDefault), jsonutil.Spaced())
	if err != nil {
		return anyutil.String(v)
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
		preview := anyutil.String(value)
		switch value.(type) {
		case *jsonutil.Object, []any:
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
		for _, ck := range obj.Keys() {
			cv := obj.Value(ck)
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
			case *jsonutil.Object:
				headline := strip(anyutil.String(dfirst(item, "", "content", "message", "title", "name", "id")))
				if headline != "" {
					lines = append(lines, fmt.Sprintf("%s  %d. %s", pad, idx, truncateText(headline, 220)))
					for _, ck := range []string{"id", "status", "type", "scope", "quality_score", "score", "path", "url"} {
						if !emptyish(dget(item, ck)) {
							lines = append(lines, fmt.Sprintf("%s    - **%s:** %s", pad, ck, truncateText(anyutil.String(dget(item, ck)), 180)))
						}
					}
				} else {
					lines = append(lines, fmt.Sprintf("%s  %d.", pad, idx))
					for _, ck := range item.Keys()[:min(item.Len(), maxItems)] {
						lines = append(lines, child(ck, item.Value(ck), 2)...)
					}
				}
			case []any:
				lines = append(lines, fmt.Sprintf("%s  %d. %d items", pad, idx, len(item)))
				for _, nested := range item[:min(len(item), maxItems)] {
					lines = append(lines, child("", nested, 2)...)
				}
			default:
				lines = append(lines, fmt.Sprintf("%s  %d. %s", pad, idx, truncateText(anyutil.String(it), 240)))
			}
		}
		if len(list) > maxItems {
			lines = append(lines, fmt.Sprintf("%s  ... %d more items", pad, len(list)-maxItems))
		}
		return lines
	}
	return []string{line(truncateText(anyutil.String(value), 500))}
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
			case *jsonutil.Object, []any:
				lines = append(lines, formatStructuredValue("", it, 0, 2, 6)...)
			default:
				lines = append(lines, "- "+truncateText(anyutil.String(it), 240))
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
			lines = append(lines, "- **"+k+":** "+truncateText(anyutil.String(dget(data, k)), 500))
		}
	}
	for _, k := range data.Keys() {
		v := data.Value(k)
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
func parseUnifiedDiffContent(diffText string) []acputil.ToolCallContent {
	var content []acputil.ToolCallContent
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
				oldText = new(strings.Join(oldLines, "\n"))
			}
			content = append(content, acputil.ToolDiffContent(p, strings.Join(newLines, "\n"), oldText))
		}
		oldPath, newPath, oldLines, newLines = nil, nil, nil, nil
	}
	for _, line := range textutil.Lines(diffText) {
		switch {
		case strings.HasPrefix(line, "--- "):
			flush()
			oldPath = new(strip(line[4:]))
		case strings.HasPrefix(line, "+++ "):
			newPath = new(strip(line[4:]))
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
func buildToolCompleteContent(toolName string, result *string, functionArgs *jsonutil.Object, snapshot *tools.LocalEditSnapshot) []acputil.ToolCallContent {
	if toolName == "skill_manage" {
		plain, _ := jsonutil.Plain(functionArgs).(map[string]any)
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
		return []acputil.ToolCallContent{textContent(text)}
	}
	r := ""
	if result != nil {
		r = *result
	}
	return []acputil.ToolCallContent{textContent(truncateText(r, 5000))}
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

func startTodo(args *jsonutil.Object) any {
	items, ok := isList(dget(args, "todos"))
	if !ok {
		return "Reading todo list"
	}
	lines := []string{"Updating todo list", ""}
	for _, it := range items[:min(len(items), 8)] {
		if i, ok := isDict(it); ok {
			lines = append(lines, "- "+anyutil.String(dgetOr(i, "status", "pending"))+": "+anyutil.String(dgetOr(i, "content", dgetOr(i, "id", ""))))
		}
	}
	return strings.Join(append(lines, more(len(items), 8, "")...), "\n")
}

func startSkillManage(args *jsonutil.Object) any {
	action := argText(args, "manage", "action")
	name := argText(args, "?", "name")
	filePath := argText(args, "SKILL.md", "file_path")
	path := "skills/" + name + "/" + filePath
	switch action {
	case "patch":
		old := anyutil.String(anyutil.Or(dget(args, "old_string"), ""))
		var oldText *string
		if old != "" {
			oldText = &old
		}
		return acputil.ToolDiffContent(path, anyutil.String(anyutil.Or(dget(args, "new_string"), "")), oldText)
	case "edit", "create":
		return acputil.ToolDiffContent(path, anyutil.String(anyutil.Or(dget(args, "content"), "")), nil)
	case "write_file":
		target := anyutil.String(anyutil.Or(dget(args, "file_path"), "file"))
		return acputil.ToolDiffContent("skills/"+name+"/"+target, anyutil.String(anyutil.Or(dget(args, "file_content"), "")), nil)
	case "delete", "remove_file":
		return "Removing " + anyutil.String(anyutil.Or(dget(args, "file_path"), filePath)) + " from skill '" + name + "'"
	}
	return "Running skill_manage action '" + action + "' on skill '" + name + "' (" + filePath + ")"
}

func startExecuteCode(args *jsonutil.Object) any {
	code := argText(args, "", "code")
	preview := runePrefix(code, 1200)
	if n := runeLen(code); n > 1200 {
		preview += fmt.Sprintf("\n... (%d chars total, truncated)", n)
	}
	return fmtValue(preview, "Running Python helper script:\n\n```python\n{}\n```", "Running Python helper script")
}

func startDelegate(args *jsonutil.Object) any {
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

var startContentBuilders = map[string]func(*jsonutil.Object) any{
	"patch": func(a *jsonutil.Object) any {
		return "Preparing " + anyutil.String(dgetOr(a, "mode", "replace")) + " edit for " + anyutil.String(anyutil.Or(dget(a, "path"), "patch input")) + ". Approval prompt shows the diff."
	},
	"write_file": func(a *jsonutil.Object) any {
		return fmtValue(dgetOr(a, "path", ""), "Preparing write to {}. Approval prompt shows the diff.", "Preparing file write. Approval prompt shows the diff.")
	},
	"terminal":  func(a *jsonutil.Object) any { return "$ " + anyutil.String(dgetOr(a, "command", "")) },
	"read_file": nil,
	"search_files": func(a *jsonutil.Object) any {
		return "Searching for '" + anyutil.String(dgetOr(a, "pattern", "")) + "' (" + anyutil.String(dgetOr(a, "target", "content")) + ")" + fmtValue(dget(a, "path"), " in {}", "")
	},
	"todo": startTodo,
	"skill_view": func(a *jsonutil.Object) any {
		return "Loading skill '" + argText(a, "?", "name") + "' (" + argText(a, "SKILL.md", "file_path") + ")"
	},
	"skill_manage": startSkillManage,
	"execute_code": startExecuteCode,
	"web_search": func(a *jsonutil.Object) any {
		return fmtValue(argText(a, "", "query"), "Searching the web for: {}", "Searching the web")
	},
	"web_extract": nil,
	"process": func(a *jsonutil.Object) any {
		return "Process action: " + argText(a, "manage", "action") + fmtValue(argText(a, "", "session_id"), "\nSession: {}", "") +
			previewLine("Input", argText(a, "", "data"), 500)
	},
	"delegate_task": startDelegate,
	"session_search": func(a *jsonutil.Object) any {
		return fmtValue(argText(a, "", "query"), "Searching past sessions for: {}", "Loading recent sessions")
	},
	"memory": func(a *jsonutil.Object) any {
		return "Memory " + argText(a, "manage", "action") + " (" + argText(a, "memory", "target") + ")" +
			previewLine("Preview", argText(a, "", "content", "old_text"), 500)
	},
}

// Tool call start for a tool, a minimal start when rendering the arguments fails
func BuildToolStart(toolCallID, toolName string, arguments *jsonutil.Object, editDiff *tools.EditProposal) *acputil.ToolCallStart {
	start, err := buildToolStartGuarded(toolCallID, toolName, arguments, editDiff)
	if err == nil {
		return start
	}
	pkgLog().Debug(fmt.Sprintf("ACP tool-start render failed for %s: %s", anyutil.Quote(toolName), err))
	safe := toolName
	if safe == "" {
		safe = "tool"
	}
	return acputil.StartToolCall(toolCallID, safe, acputil.ToolCallFields{Kind: new(GetToolKind(safe)), Locations: []acputil.ToolCallLocation{}})
}

func buildToolStartGuarded(toolCallID, toolName string, arguments *jsonutil.Object, editDiff *tools.EditProposal) (start *acputil.ToolCallStart, err error) {
	defer func() {
		if r := recover(); r != nil {
			start, err = nil, fmt.Errorf("%v", r)
		}
	}()
	if arguments == nil {
		arguments = jsonutil.NewObject()
	}
	var content []acputil.ToolCallContent
	var rawInput any
	builder, hasBuilder := startContentBuilders[toolName]
	switch {
	case (toolName == "patch" || toolName == "write_file") && editDiff != nil:
		content = []acputil.ToolCallContent{acputil.ToolDiffContent(editDiff.Path, editDiff.NewText, editDiff.OldText)}
	case hasBuilder:
		if builder != nil {
			switch built := builder(arguments).(type) {
			case string:
				content = []acputil.ToolCallContent{textContent(built)}
			case acputil.ToolCallContent:
				content = []acputil.ToolCallContent{built}
			}
		}
	case polishedTools[toolName]:
		content = []acputil.ToolCallContent{textContent(truncateText(argsJSON(arguments), 1200))}
	case arguments.Len() == 0:
	default:
		content = []acputil.ToolCallContent{textContent(argsJSON(arguments))}
		rawInput = arguments
	}
	locations, err := ExtractLocations(arguments)
	if err != nil {
		return nil, err
	}
	return acputil.StartToolCall(toolCallID, BuildToolTitle(toolName, arguments), acputil.ToolCallFields{
		Kind: new(GetToolKind(toolName)), Content: content, Locations: locations, RawInput: rawInput,
	}), nil
}

// Tool call progress closing a completed call with its formatted result and status
func BuildToolComplete(toolCallID, toolName string, result *string, functionArgs *jsonutil.Object, snapshot *tools.LocalEditSnapshot, isError bool) *acputil.ToolCallProgress {
	var content []acputil.ToolCallContent
	if toolName == "web_extract" {
		if text, ok := formatWebExtractResult(toolName, result, functionArgs); ok && text != "" {
			content = []acputil.ToolCallContent{textContent(text)}
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
	case *jsonutil.Object, []any:
	default:
		if !polishedTools[toolName] && result != nil {
			rawOutput = *result
		}
	}
	return acputil.UpdateToolCall(toolCallID, acputil.ToolCallFields{
		Kind: new(GetToolKind(toolName)), Status: new(status), Content: content, RawOutput: rawOutput,
	})
}

// Failed tool call progress for a call that ended without a result
func BuildToolAbandoned(toolCallID, toolName string) *acputil.ToolCallProgress {
	return acputil.UpdateToolCall(toolCallID, acputil.ToolCallFields{
		Kind: new(GetToolKind(toolName)), Status: new("failed"),
		Content: []acputil.ToolCallContent{textContent("This tool call ended without a result (blocked, denied, or interrupted).")},
	})
}

// File system locations of tool arguments, a path with its offset or line
func ExtractLocations(arguments *jsonutil.Object) ([]acputil.ToolCallLocation, error) {
	path := dget(arguments, "path")
	if !anyutil.IsTrue(path) {
		return []acputil.ToolCallLocation{}, nil
	}
	text, ok := path.(string)
	if !ok {
		return nil, fmt.Errorf("tool path is %s, expected text", anyutil.Kind(path))
	}
	loc := acputil.ToolCallLocation{Path: text}
	if raw := anyutil.Or(dget(arguments, "offset"), dget(arguments, "line")); raw != nil {
		n, ok := anyutil.Int(raw)
		if !ok {
			return nil, fmt.Errorf("tool line is %s, expected a whole number", anyutil.Kind(raw))
		}
		line := int(n)
		loc.Line = &line
	}
	return []acputil.ToolCallLocation{loc}, nil
}
