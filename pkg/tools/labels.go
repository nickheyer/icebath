package tools

import (
	"context"
	"encoding/json"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	ConnectorEmoji   = "🔗"
	MCPEmoji         = "🔌"
	SearchEmoji      = "🔎"
	DescribeEmoji    = "📖"
	DefaultToolEmoji = "⚡"
)

var appTitles = map[string]string{
	"discord":        "Discord",
	"figma":          "Figma",
	"github":         "GitHub",
	"gmail":          "Gmail",
	"googlecalendar": "Google Calendar",
	"googledocs":     "Google Docs",
	"googledrive":    "Google Drive",
	"jira":           "Jira",
	"linear":         "Linear",
	"notion":         "Notion",
	"outlook":        "Outlook",
	"slack":          "Slack",
	"stripe_mcp":     "Stripe",
	"todoist":        "Todoist",
}

// ToolLabel is the friendly app and action label of one tool call
type ToolLabel struct {
	Kind    string
	App     string
	Action  string
	Emoji   string
	Name    string
	Preview string
}

// Text joins the app and action with a middle dot
func (l ToolLabel) Text() string {
	if l.Action != "" {
		return l.App + " · " + l.Action
	}
	return l.App
}

// AsPayload renders the label as the string map gateway cards consume
func (l ToolLabel) AsPayload() map[string]string {
	return map[string]string{"kind": l.Kind, "app": l.App, "action": l.Action, "emoji": l.Emoji,
		"text": l.Text(), "name": l.Name, "preview": l.Preview}
}

// ConnectorName is a parsed connectors__<connector>__<tool> name
type ConnectorName struct {
	Raw       string
	Connector string
	Tool      string
}

// ParseConnectorName uses a bounded split so tool slugs retain internal underscores
func ParseConnectorName(name string) (ConnectorName, bool) {
	parts := strings.SplitN(name, "__", 3)
	if len(parts) != 3 {
		return ConnectorName{}, false
	}
	prefix, connector, tool := parts[0], parts[1], parts[2]
	if prefix != "connectors" || connector == "" || tool == "" {
		return ConnectorName{}, false
	}
	return ConnectorName{Raw: name, Connector: connector, Tool: tool}, true
}

// FormatConnectorName builds the connector tool name without the conventional vendor prefix
func FormatConnectorName(connector, tool string) string {
	tool = strings.TrimPrefix(tool, strings.ToUpper(connector)+"_")
	return ConnectorNamePrefix + connector + "__" + tool
}

// VendorSlugCandidates tries the stripped conventional prefix before a literal vendor slug
func VendorSlugCandidates(connector, tool string) []string {
	return []string{strings.ToUpper(connector) + "_" + tool, tool}
}

func splitWords(text string) []string {
	return strings.Fields(strings.NewReplacer("-", " ", "_", " ").Replace(text))
}

// AppTitle renders a connector or tool slug as a display title
func AppTitle(slug string) string {
	key := strings.TrimSpace(slug)
	if known, ok := appTitles[strings.ToLower(key)]; ok && known != "" {
		return known
	}
	words := splitWords(key)
	for i, word := range words {
		r, size := utf8.DecodeRuneInString(word)
		words[i] = strings.ToUpper(string(r)) + word[size:]
	}
	if joined := strings.Join(words, " "); joined != "" {
		return joined
	}
	return key
}

// Lower cased action words of a connector or MCP tool with the app prefix dropped
func actionPhrase(tool, appSlug string) string {
	text := strings.TrimSpace(tool)
	prefix := strings.ToUpper(strings.ReplaceAll(appSlug, "-", "_")) + "_"
	if appSlug != "" && strings.HasPrefix(strings.ToUpper(text), prefix) {
		text = text[len(prefix):]
	}
	return strings.ToLower(strings.Join(splitWords(text), " "))
}

// Registry emoji of a tool, the default when none is registered
func registryEmoji(env *Env, name string) string {
	if e := Default.Emoji(context.Background(), ScopeKey(envHome(env)), name, ""); e != "" {
		return e
	}
	return DefaultToolEmoji
}

func argPreview(env *Env, name string, arguments any) string {
	args, ok := arguments.(map[string]any)
	if !ok || len(args) == 0 {
		return ""
	}
	return BuildToolPreview(env, name, args, nil)
}

func localToolLabel(env *Env, name string, arguments any) ToolLabel {
	verb := GetToolVerb(name)
	app := verb
	if app == "" {
		app = AppTitle(name)
	}
	return ToolLabel{Kind: "tool", App: app, Emoji: registryEmoji(env, name), Name: name, Preview: argPreview(env, name, arguments)}
}

func unnamedLabel(env *Env, arguments any) ToolLabel {
	return ToolLabel{Kind: "tool", App: "Unnamed call", Emoji: DefaultToolEmoji, Preview: argPreview(env, "", arguments)}
}

// LabelForToolName labels a connector, MCP, or local tool name, ok false for a blank name
func LabelForToolName(env *Env, name string, arguments any) (ToolLabel, bool) {
	if strings.TrimSpace(name) == "" {
		return ToolLabel{}, false
	}
	if connector, ok := ParseConnectorName(name); ok {
		action := actionPhrase(connector.Tool, connector.Connector)
		if args, ok := arguments.(map[string]any); ok {
			if alias, ok := args["connector_alias"].(string); ok && strings.TrimSpace(alias) != "" {
				action = action + " (" + strings.TrimSpace(alias) + ")"
			}
		}
		return ToolLabel{Kind: "connector", App: AppTitle(connector.Connector), Action: action,
			Emoji: ConnectorEmoji, Name: name, Preview: argPreview(env, name, arguments)}, true
	}
	if strings.HasPrefix(name, MCPToolPrefix) {
		server, _, tool := partition(name[len(MCPToolPrefix):], "__")
		if tool != "" {
			return ToolLabel{Kind: "mcp", App: AppTitle(server), Action: actionPhrase(tool, ""),
				Emoji: MCPEmoji, Name: name, Preview: argPreview(env, name, arguments)}, true
		}
	}
	return localToolLabel(env, name, arguments), true
}

func anyList(v any) ([]any, bool) {
	switch x := v.(type) {
	case []any:
		return x, true
	case []string:
		out := make([]any, len(x))
		for i, s := range x {
			out[i] = s
		}
		return out, true
	}
	return nil, false
}

func bridgeLabels(functionName string, args map[string]any) []ToolLabel {
	if functionName == "tool_search" {
		queries := args["queries"]
		if s, ok := queries.(string); ok {
			queries = []any{s}
		}
		first := ""
		if list, ok := anyList(queries); ok {
			for _, q := range list {
				text := pyStrOrEmpty(q)
				if strings.TrimSpace(text) != "" {
					first = text
					break
				}
			}
		}
		return []ToolLabel{{Kind: "tool", App: "Searching tools", Action: first, Emoji: SearchEmoji, Name: functionName}}
	}
	names := args["names"]
	if s, ok := names.(string); ok {
		names = []any{s}
	}
	count := 0
	if list, ok := anyList(names); ok {
		count = len(list)
	}
	action := strconv.Itoa(count) + " tools"
	if count == 1 {
		action = "1 tool"
	}
	return []ToolLabel{{Kind: "tool", App: "Reading tool details", Action: action, Emoji: DescribeEmoji, Name: functionName}}
}

// LabelsForCall lists the friendly labels of a tool call, one per bridged call
func LabelsForCall(env *Env, functionName string, functionArgs any) []ToolLabel {
	args, _ := functionArgs.(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	if functionName == "tool_search" || functionName == "tool_describe" {
		return bridgeLabels(functionName, args)
	}
	if functionName != "tool_call" {
		label, ok := LabelForToolName(env, functionName, args)
		if ok && label.Kind != "tool" {
			return []ToolLabel{label}
		}
		return nil
	}
	entries, ok := anyList(args["calls"])
	if !ok {
		entries = []any{args}
	}
	var labels []ToolLabel
	for _, entry := range entries {
		m, _ := entry.(map[string]any)
		if m == nil {
			m = map[string]any{}
		}
		arguments := m["arguments"]
		name, _ := m["name"].(string)
		if label, ok := LabelForToolName(env, name, arguments); ok {
			labels = append(labels, label)
		} else {
			labels = append(labels, unnamedLabel(env, arguments))
		}
	}
	return labels
}

var (
	displayMu           sync.RWMutex
	toolPreviewMaxLen   = 0
	friendlyToolLabels  = true
	skinToolEmojiHook   func() map[string]string
	toolVerbsNoPreview  = setOf("skills_list", "session_search")
	toolVerbsForConnect = setOf("web_search", "search_files")
	bridgeGenerating    = map[string]string{"tool_call": "a tool call", "tool_search": "a tool search", "tool_describe": "tool details"}
)

// SetToolPreviewMaxLen sets the global max length for tool call previews, zero means no limit
func SetToolPreviewMaxLen(n int) {
	displayMu.Lock()
	defer displayMu.Unlock()
	toolPreviewMaxLen = max(n, 0)
}

// GetToolPreviewMaxLen returns the configured max preview length, zero means unlimited
func GetToolPreviewMaxLen() int {
	displayMu.RLock()
	defer displayMu.RUnlock()
	return toolPreviewMaxLen
}

// SetFriendlyToolLabels toggles friendly human phrased tool labels
func SetFriendlyToolLabels(enabled bool) {
	displayMu.Lock()
	defer displayMu.Unlock()
	friendlyToolLabels = enabled
}

func friendlyLabelsEnabled() bool {
	displayMu.RLock()
	defer displayMu.RUnlock()
	return friendlyToolLabels
}

// SetSkinToolEmojis registers the active skin's per tool emoji overrides
func SetSkinToolEmojis(hook func() map[string]string) {
	displayMu.Lock()
	defer displayMu.Unlock()
	skinToolEmojiHook = hook
}

func skinToolEmojis() map[string]string {
	displayMu.RLock()
	hook := skinToolEmojiHook
	displayMu.RUnlock()
	if hook == nil {
		return nil
	}
	return hook()
}

// GetToolEmoji resolves a tool's display emoji, skin override then registry then the default
func GetToolEmoji(env *Env, toolName, def string) string {
	if override := skinToolEmojis()[toolName]; override != "" {
		return override
	}
	if e := Default.Emoji(context.Background(), ScopeKey(envHome(env)), toolName, ""); e != "" {
		return e
	}
	return def
}

// Collapses whitespace including newlines to single spaces
func oneline(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// Tail truncates to limit runes with an ellipsis, the result never exceeds the limit
func tailTrunc(text string, limit int) string {
	runes := []rune(text)
	if limit <= 0 || len(runes) <= limit {
		return text
	}
	if limit <= 3 {
		return strings.Repeat(".", limit)
	}
	return string(runes[:limit-3]) + "..."
}

// First n runes plus an ellipsis when longer
func clip(text string, n int) string {
	runes := []rune(text)
	if len(runes) > n {
		return string(runes[:n]) + "..."
	}
	return text
}

// ToolPreview is a compact tool preview plus presentation facts lost to truncation
type ToolPreview struct {
	Text      string
	Truncated bool
	URL       string
}

var (
	shellSilentHeads   = setOf("cd", "pushd", "popd", "export", "set", "unset", "source", ".", "true", "false", ":")
	shellPipeTailHeads = setOf("head", "tail", "wc", "sort", "uniq")
	envAssignHeadRe    = regexp.MustCompile(`^[A-Za-z_]\w*=`)
	redirectOpRe       = regexp.MustCompile(`^\d*(?:>>?|<)$`)
	redirectDupRe      = regexp.MustCompile(`^\d*(?:>&|<&)\d+$`)
	boundaryEchoRe     = regexp.MustCompile(`-{2,}|_exit=|(?:^|\s|=)\$[?{]|PIPESTATUS`)
)

func shellBasename(head string) string {
	if head == "" {
		return ""
	}
	return head[strings.LastIndex(head, "/")+1:]
}

type quotedRune struct {
	index  int
	ch     rune
	quoted bool
}

// Runes with their quote state, a quote closes unless backslash escaped
func scanQuoted(text string) []quotedRune {
	var out []quotedRune
	var quote rune
	runes := []rune(text)
	for i, ch := range runes {
		switch {
		case quote != 0:
			out = append(out, quotedRune{i, ch, true})
			if ch == quote && (i == 0 || runes[i-1] != '\\') {
				quote = 0
			}
		case ch == '\'' || ch == '"':
			quote = ch
			out = append(out, quotedRune{i, ch, true})
		default:
			out = append(out, quotedRune{i, ch, false})
		}
	}
	return out
}

func splitShellWords(segment string) []string {
	parts := [][]rune{{}}
	for _, q := range scanQuoted(segment) {
		if !q.quoted && pyIsSpace(q.ch) {
			parts = append(parts, []rune{})
		} else {
			parts[len(parts)-1] = append(parts[len(parts)-1], q.ch)
		}
	}
	var out []string
	for _, p := range parts {
		if len(p) > 0 {
			out = append(out, string(p))
		}
	}
	return out
}

// Drops a trailing pipe into head, tail, wc, sort, or uniq
func stripShellPipeTail(segment string) string {
	words := splitShellWords(segment)
	for i, word := range words {
		next := ""
		if i+1 < len(words) {
			next = words[i+1]
		}
		if word == "|" && shellPipeTailHeads[shellBasename(next)] {
			words = words[:i]
			break
		}
	}
	return strings.TrimSpace(strings.Join(words, " "))
}

// Splits on unquoted and or lists, semicolons, and newlines, dropping pipe tails per segment
func splitShellCompound(command string) []string {
	raw := [][]rune{{}}
	skip := false
	runes := []rune(command)
	for _, q := range scanQuoted(command) {
		i := q.index
		switch {
		case skip:
			skip = false
		case !q.quoted && i+1 < len(runes) && ((runes[i] == '&' && runes[i+1] == '&') || (runes[i] == '|' && runes[i+1] == '|')):
			raw = append(raw, []rune{})
			skip = true
		case !q.quoted && (q.ch == ';' || q.ch == '\n'):
			raw = append(raw, []rune{})
		default:
			raw[len(raw)-1] = append(raw[len(raw)-1], q.ch)
		}
	}
	var segments []string
	for _, buf := range raw {
		if s := stripShellPipeTail(strings.TrimSpace(string(buf))); s != "" {
			segments = append(segments, s)
		}
	}
	return segments
}

// Command name of a segment skipping leading assignments
func shellHeadWord(segment string) string {
	words := splitShellWords(segment)
	for len(words) > 0 && envAssignHeadRe.MatchString(words[0]) {
		words = words[1:]
	}
	if len(words) == 0 {
		return ""
	}
	return shellBasename(words[0])
}

// Drops redirections from a segment
func cleanShellSegment(segment string) string {
	words := splitShellWords(segment)
	var out []string
	i := 0
	for i < len(words) {
		word := words[i]
		switch {
		case redirectOpRe.MatchString(word):
			i += 2
		case redirectDupRe.MatchString(word):
			i++
		default:
			out = append(out, word)
			i++
		}
	}
	return strings.TrimSpace(strings.Join(out, " "))
}

func isShellBoundaryEcho(segment string) bool {
	words := splitShellWords(segment)
	if len(words) == 0 || shellBasename(words[0]) != "echo" {
		return false
	}
	return boundaryEchoRe.MatchString(strings.Join(words[1:], " "))
}

// SummarizeShellCommand compacts shell wrapper plumbing for display while the raw command stays elsewhere
func SummarizeShellCommand(command string) string {
	original := oneline(command)
	if original == "" {
		return ""
	}
	segments := splitShellCompound(original)
	if len(segments) <= 1 {
		first := original
		if len(segments) == 1 {
			first = segments[0]
		}
		if cleaned := cleanShellSegment(first); cleaned != "" {
			return cleaned
		}
		return original
	}
	var core []string
	for _, segment := range segments {
		cleaned := cleanShellSegment(segment)
		if cleaned != "" && !shellSilentHeads[shellHeadWord(cleaned)] && !isShellBoundaryEcho(cleaned) {
			core = append(core, cleaned)
		}
	}
	if len(core) == 0 {
		return original
	}
	count := len(core) - 1
	if count == 0 {
		return core[0]
	}
	if count == 1 {
		return core[0] + " + 1 command"
	}
	return core[0] + " + " + strconv.Itoa(count) + " commands"
}

func readFileLineLabel(args map[string]any) string {
	offset, ok := asIntArg(args["offset"])
	if !ok || offset <= 0 {
		return ""
	}
	if limit, ok := asIntArg(args["limit"]); ok && limit > 1 {
		return "L" + strconv.Itoa(offset) + "-" + strconv.Itoa(offset+limit-1)
	}
	return "L" + strconv.Itoa(offset)
}

// Integer argument the model sent as an int or a whole float
func asIntArg(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		if x == float64(int(x)) {
			return int(x), true
		}
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return int(n), true
		}
	}
	return 0, false
}

// RedactToolArgsForDisplay copies tool args safe for logs and progress UI
func RedactToolArgsForDisplay(env *Env, toolName string, args map[string]any) map[string]any {
	if args == nil {
		return nil
	}
	if text, ok := args["text"].(string); ok && toolName == "browser_type" {
		out := map[string]any{}
		for k, v := range args {
			out[k] = v
		}
		out["text"] = RedactSensitiveTextWith(env, text, RedactOptions{Force: true})
		return out
	}
	return args
}

// One truncated goal string per dict task, a question mark when missing
func delegateTaskGoals(tasks any, perGoalLen int) []string {
	list, ok := anyList(tasks)
	if !ok {
		return nil
	}
	var goals []string
	for _, task := range list {
		m, ok := task.(map[string]any)
		if !ok {
			continue
		}
		g, present := m["goal"]
		text := "?"
		if present && g != nil {
			text = oneline(pyStrOrEmpty(g))
		}
		if text == "" {
			text = "?"
		}
		goals = append(goals, tailTrunc(text, perGoalLen))
	}
	return goals
}

// User friendly step label of a browser_exec call, the leading comment of its code
func browserExecStepLabel(args map[string]any, maxChars int) (string, bool) {
	first, _, _ := strings.Cut(pystr.Strip(pyStrOrEmpty(args["code"])), "\n")
	first = pystr.Strip(first)
	label := ""
	if strings.HasPrefix(first, "#") {
		label = pystr.Strip(strings.TrimLeft(first, "#"))
	}
	if label == "" {
		return "", false
	}
	if runes := []rune(label); len(runes) > maxChars {
		return string(runes[:maxChars-1]) + "…", true
	}
	return label, true
}

// PrimaryArgs maps each core tool to the argument its preview shows
var PrimaryArgs = map[string]string{
	"terminal": "command", "web_search": "query", "web_extract": "urls", "read_file": "path",
	"write_file": "path", "patch": "path", "search_files": "pattern", "browser_navigate": "url",
	"browser_click": "ref", "browser_type": "text", "image_generate": "prompt", "text_to_speech": "text",
	"vision_analyze": "question", "skill_view": "name", "skills_list": "category", "cronjob_manage": "action",
	"execute_code": "code", "browser_exec": "code", "delegate_task": "goal",
	"skill_manage": "name",
}

var fallbackPreviewKeys = []string{"query", "text", "command", "path", "name", "prompt", "code", "goal"}

// Shared list, steer, or stop preview for delegate_task, ok false for spawn calls
func delegateActionPreview(args map[string]any) (string, bool) {
	action := strings.ToLower(strings.TrimSpace(pyStrOrEmpty(args["action"])))
	if action == "list" || action == "steer" || action == "stop" {
		return strings.TrimSpace(action + " " + strings.TrimSpace(pyStrOrEmpty(args["subagent_id"]))), true
	}
	return "", false
}

func previewBrowserExec(env *Env, args map[string]any, maxLen int) string {
	if label, ok := browserExecStepLabel(args, 80); ok {
		return tailTrunc(label, maxLen)
	}
	return tailTrunc(oneline(pyStrOrEmpty(args["code"])), maxLen)
}

func previewDelegateTask(env *Env, args map[string]any, maxLen int) string {
	if action, ok := delegateActionPreview(args); ok {
		return tailTrunc(action, maxLen)
	}
	if tasks, ok := anyList(args["tasks"]); ok && len(tasks) > 0 {
		goals := delegateTaskGoals(tasks, 40)
		preview := strconv.Itoa(len(tasks)) + " parallel tasks"
		if len(goals) > 0 {
			preview = strconv.Itoa(len(goals)) + " tasks: " + strings.Join(goals, " | ")
		}
		return tailTrunc(preview, maxLen)
	}
	goal, present := args["goal"]
	if present && goal == nil {
		return ""
	}
	return tailTrunc(oneline(pyStrOrEmpty(goal)), maxLen)
}

func previewProcessManage(env *Env, args map[string]any, _ int) string {
	action, sid, data, timeout := args["action"], args["session_id"], args["data"], args["timeout"]
	var parts []string
	if action != nil && pyStrOrEmpty(action) != "" {
		parts = append(parts, pyStr(action))
	}
	if sid != nil && pyStrOrEmpty(sid) != "" {
		parts = append(parts, string([]rune(pyStr(sid))[:min(16, len([]rune(pyStr(sid))))]))
	}
	if data != nil && pyStrOrEmpty(data) != "" {
		text := []rune(pyStr(data))
		parts = append(parts, "\""+oneline(string(text[:min(20, len(text))]))+"\"")
	}
	if timeout != nil && pyStrOrEmpty(timeout) != "" && pyStrOrEmpty(timeout) != "0" && action == "wait" {
		parts = append(parts, pyStr(timeout)+"s")
	}
	return strings.Join(parts, " ")
}

func previewTodoList(env *Env, args map[string]any, _ int) string {
	todos, present := args["todos"]
	verb := "planning"
	if merge, ok := args["merge"].(bool); ok && merge {
		verb = "updating"
	}
	if !present || todos == nil {
		return "reading task list"
	}
	count := 0
	if list, ok := anyList(todos); ok {
		count = len(list)
	}
	return verb + " " + strconv.Itoa(count) + " task(s)"
}

func previewShell(key string) previewBuilder {
	return func(env *Env, args map[string]any, maxLen int) string {
		command, present := args[key]
		if !present || command == nil {
			return ""
		}
		return tailTrunc(SummarizeShellCommand(sanitizeCommandForDisplay(pyStr(command), false)), maxLen)
	}
}

func previewReadFile(env *Env, args map[string]any, maxLen int) string {
	p := args["path"]
	if p == nil || pyStrOrEmpty(p) == "" {
		p = args["file"]
	}
	if p == nil || pyStrOrEmpty(p) == "" {
		p = args["filepath"]
	}
	if p == nil {
		return ""
	}
	text := pyStr(p)
	label := path.Base(strings.ReplaceAll(text, "\\", "/"))
	if label == "." || label == "/" || label == "" {
		label = text
	}
	return tailTrunc(strings.TrimSpace(label+" "+readFileLineLabel(args)), maxLen)
}

// The op a memory progress line shows and an N more suffix for the rest of a batch
func memoryFirstOp(args map[string]any) (map[string]any, string) {
	ops, ok := anyList(args["operations"])
	if !ok || len(ops) == 0 {
		return args, ""
	}
	first, _ := ops[0].(map[string]any)
	if first == nil {
		first = map[string]any{}
	}
	if len(ops) > 1 {
		return first, " (" + strconv.Itoa(len(ops)-1) + " more)"
	}
	return first, ""
}

func previewMemory(env *Env, args map[string]any, _ int) string {
	op, more := memoryFirstOp(args)
	action, target := pyStrOrEmpty(op["action"]), pyStrOrEmpty(args["target"])
	switch action {
	case "add":
		content := pyStrOrEmpty(op["content"])
		if content == "" {
			content = pyStrOrEmpty(op["new_text"])
		}
		return "+" + target + ": \"" + clip(oneline(content), 25) + "\"" + more
	case "replace", "patch", "remove":
		old := oneline(pyStrOrEmpty(op["old_text"]))
		if old == "" {
			old = "<missing old_text>"
		}
		marker := "~"
		if action == "remove" {
			marker = "-"
		}
		runes := []rune(old)
		return marker + target + ": \"" + string(runes[:min(20, len(runes))]) + "\"" + more
	}
	return action
}

func previewSendMessage(env *Env, args map[string]any, _ int) string {
	target := "?"
	if t, ok := args["target"]; ok {
		target = pyStr(t)
	}
	return "to " + target + ": \"" + tailTrunc(oneline(pyStrOrEmpty(args["message"])), 20) + "\""
}

func previewSkillView(env *Env, args map[string]any, maxLen int) string {
	name := oneline(pyStrOrEmpty(args["name"]))
	filePath, present := args["file_path"]
	label := name
	if present && filePath != nil && pyStrOrEmpty(filePath) != "" {
		if name != "" {
			label = name + " → " + oneline(pyStr(filePath))
		} else {
			label = oneline(pyStr(filePath))
		}
	}
	return tailTrunc(label, maxLen)
}

func previewClarify(env *Env, args map[string]any, maxLen int) string {
	questions, ok := anyList(args["questions"])
	if !ok || len(questions) == 0 {
		return ""
	}
	first, ok := questions[0].(map[string]any)
	if !ok {
		return ""
	}
	return tailTrunc(oneline(pyStrOrEmpty(first["question"])), maxLen)
}

func previewBridgeCall(toolName string) previewBuilder {
	return func(env *Env, args map[string]any, maxLen int) string {
		labels := BridgeToolLabels(env, toolName, args)
		if len(labels) == 0 {
			return primaryArgPreview(toolName, args, maxLen)
		}
		extra := ""
		if len(labels) > 1 {
			extra = " +" + strconv.Itoa(len(labels)-1)
		}
		return tailTrunc(labels[0].Text()+extra, maxLen)
	}
}

func previewSessionSearch(env *Env, args map[string]any, _ int) string {
	return "recall: \"" + clip(oneline(pyStrOrEmpty(args["query"])), 25) + "\""
}

type previewBuilder func(env *Env, args map[string]any, maxLen int) string

// Tool specific preview builder, nil when the tool falls through to the primary argument lookup
func previewBuilderFor(toolName string) previewBuilder {
	switch toolName {
	case "browser_exec":
		return previewBrowserExec
	case "delegate_task":
		return previewDelegateTask
	case "process_manage":
		return previewProcessManage
	case "todo_list":
		return previewTodoList
	case "terminal":
		return previewShell("command")
	case "execute_code":
		return previewShell("code")
	case "read_file":
		return previewReadFile
	case "memory":
		return previewMemory
	case "send_message":
		return previewSendMessage
	case "skill_view":
		return previewSkillView
	case "clarify":
		return previewClarify
	case "session_search":
		return previewSessionSearch
	case "tool_call", "tool_search", "tool_describe":
		return previewBridgeCall(toolName)
	}
	return nil
}

// BuildToolPreview renders a short preview of a tool call's primary argument, nil max len uses the global cap
func BuildToolPreview(env *Env, toolName string, args map[string]any, maxLen *int) string {
	limit := GetToolPreviewMaxLen()
	if maxLen != nil {
		limit = *maxLen
	}
	if len(args) == 0 {
		return ""
	}
	if redacted := RedactToolArgsForDisplay(env, toolName, args); redacted != nil {
		args = redacted
	}
	if builder := previewBuilderFor(toolName); builder != nil {
		return builder(env, args, limit)
	}
	return primaryArgPreview(toolName, args, limit)
}

func primaryArgPreview(toolName string, args map[string]any, maxLen int) string {
	key := PrimaryArgs[toolName]
	if key == "" {
		for _, k := range fallbackPreviewKeys {
			if _, ok := args[k]; ok {
				key = k
				break
			}
		}
	}
	value, ok := args[key]
	if key == "" || !ok {
		return ""
	}
	if list, isList := anyList(value); isList {
		if len(list) == 0 {
			value = ""
		} else {
			value = list[0]
		}
	}
	preview := oneline(pyStr(value))
	if preview == "" {
		return ""
	}
	return tailTrunc(preview, maxLen)
}

// Display only URL extracted from a string or a dict with url or href
func displayURL(value any) string {
	if m, ok := value.(map[string]any); ok {
		value = m["url"]
		if value == nil || value == "" {
			value = m["href"]
		}
	}
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// The candidate when it parses as an absolute http or https URL, empty otherwise
func httpURL(candidate string) string {
	parsed, err := url.Parse(candidate)
	if err != nil {
		return ""
	}
	scheme := strings.ToLower(parsed.Scheme)
	if (scheme == "http" || scheme == "https") && parsed.Host != "" {
		return candidate
	}
	return ""
}

// PrepareToolPreview builds a compact preview plus explicit truncation and URL facts
func PrepareToolPreview(env *Env, toolName string, args map[string]any, fallback string, maxLen int) ToolPreview {
	unlimited := 0
	fullText := BuildToolPreview(env, toolName, args, &unlimited)
	if fullText == "" {
		fullText = fallback
	}
	text := tailTrunc(fullText, maxLen)
	truncated := text != fullText
	link := ""
	if truncated {
		link = httpURL(displayURL(fullText))
	}
	return ToolPreview{Text: text, Truncated: truncated, URL: link}
}

// ToolVerbs are the curated friendly verbs of the built in tools
var ToolVerbs = map[string]string{
	"web_search":       "Searching the web",
	"web_extract":      "Reading",
	"browser_navigate": "Browsing",
	"browser_click":    "Clicking",
	"browser_type":     "Typing",
	"read_file":        "Reading",
	"write_file":       "Writing",
	"patch":            "Editing",
	"search_files":     "Searching files",
	"terminal":         "Running",
	"execute_code":     "Running code",
	"image_generate":   "Generating image",
	"video_generate":   "Generating video",
	"text_to_speech":   "Generating speech",
	"vision_analyze":   "Looking at the image",
	"session_search":   "Searching past sessions",
	"skill_view":       "Reading skill",
	"skills_list":      "Listing skills",
	"skill_manage":     "Updating skill",
	"delegate_task":    "Delegating",
	"cronjob_manage":   "Scheduling",
	"clarify":          "Asking",
	"memory":           "Updating memory",
	"todo_list":        "Updating tasks",
}

func toolVerb(toolName string) string {
	return ToolVerbs[toolName]
}

// BridgeGeneratingPhrase names what a bridge tool is generating while friendly labels are on
func BridgeGeneratingPhrase(toolName string) string {
	if !friendlyLabelsEnabled() {
		return ""
	}
	return bridgeGenerating[toolName]
}

// ToolLabelsForCall applies the skin's emoji overrides to the labels of a call
func ToolLabelsForCall(env *Env, toolName string, args map[string]any) []ToolLabel {
	labels := LabelsForCall(env, toolName, args)
	overrides := skinToolEmojis()
	for i, label := range labels {
		if override, ok := overrides[label.Name]; ok && override != "" {
			labels[i].Emoji = override
		}
	}
	return labels
}

// BridgeToolLabels lists the labels of a call when friendly labels are enabled
func BridgeToolLabels(env *Env, toolName string, args map[string]any) []ToolLabel {
	if !friendlyLabelsEnabled() {
		return nil
	}
	return ToolLabelsForCall(env, toolName, args)
}

// ToolRowEmoji picks the first label's emoji else the tool's own
func ToolRowEmoji(env *Env, toolName string, args map[string]any, def string) string {
	if len(args) > 0 {
		if labels := BridgeToolLabels(env, toolName, args); len(labels) > 0 {
			return labels[0].Emoji
		}
	}
	return GetToolEmoji(env, toolName, def)
}

// GetToolVerb returns the friendly verb for a built in tool, empty when labels are off or none is curated
func GetToolVerb(toolName string) string {
	if !friendlyLabelsEnabled() {
		return ""
	}
	return toolVerb(toolName)
}

// ToolVerbConnector joins a verb and its preview with for or a space
func ToolVerbConnector(toolName string) string {
	if toolVerbsForConnect[toolName] {
		return " for "
	}
	return " "
}

// VerbDropsPreview reports whether the verb renders alone without the argument preview
func VerbDropsPreview(toolName string) bool {
	return toolVerbsNoPreview[toolName]
}

// BuildStatusPhrase renders the lowercase is verb preview phrase that follows the bot's display name
func BuildStatusPhrase(env *Env, toolName string, args map[string]any, maxLen int) string {
	if toolName == "" || toolName == "_thinking" || !friendlyLabelsEnabled() {
		return ""
	}
	verb := toolVerb(toolName)
	phrase := "is using " + toolName
	if verb != "" {
		r, size := utf8.DecodeRuneInString(verb)
		phrase = "is " + strings.ToLower(string(r)) + verb[size:]
	}
	if len(args) > 0 && verb != "" && !toolVerbsNoPreview[toolName] {
		if preview := BuildToolPreview(env, toolName, args, nil); preview != "" {
			first := strings.TrimSpace(strings.SplitN(preview, "\n", 2)[0])
			phrase = phrase + ToolVerbConnector(toolName) + first
		}
	}
	runes := []rune(phrase)
	if len(runes) > maxLen-1 {
		return strings.TrimRight(string(runes[:max(maxLen-2, 0)]), " \t\n\r") + "…"
	}
	return phrase + "…"
}

// BuildToolLabel renders the one line friendly label of a tool call
func BuildToolLabel(env *Env, toolName string, args map[string]any, maxLen *int) string {
	limit := GetToolPreviewMaxLen()
	if maxLen != nil {
		limit = *maxLen
	}
	if labels := BridgeToolLabels(env, toolName, args); len(labels) > 0 {
		label := labels[0]
		preview := tailTrunc(label.Preview, limit)
		extra := ""
		if len(labels) > 1 {
			extra = " +" + strconv.Itoa(len(labels)-1)
		}
		if preview != "" {
			return label.Text() + " " + preview + extra
		}
		return label.Text() + extra
	}
	verb := GetToolVerb(toolName)
	if verb != "" && toolVerbsNoPreview[toolName] {
		return verb
	}
	preview := BuildToolPreview(env, toolName, args, maxLen)
	if verb == "" {
		return preview
	}
	if preview != "" {
		return verb + ToolVerbConnector(toolName) + preview
	}
	return verb
}

// VerboseToolArgsText renders the typed ordered args as json.dumps does, capped at maxLen, code blocked with real line breaks
func VerboseToolArgsText(args *pyjson.Object, maxLen int, codeBlock bool) string {
	text := pyDumps(args, pyjson.EnsureASCII(false), pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil }))
	if maxLen > 0 && len(pystr.Runes(text)) > maxLen {
		text = pystr.Slice(text, 0, maxLen-3) + "..."
	}
	if !codeBlock || strings.Contains(text, "```") {
		return text
	}
	decoded := jsonEscapeRe.ReplaceAllStringFunc(text, func(m string) string {
		switch m[1:] {
		case "n":
			return "\n"
		case "t":
			return "\t"
		}
		return m
	})
	return "```\n" + decoded + "\n```"
}

var jsonEscapeRe = regexp.MustCompile(`\\(.)`)
