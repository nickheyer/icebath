package engine

import (
	"cmp"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/contexts"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// Text of a tool result cleared to save room
	prunedPlaceholder = "[Old tool output cleared to save context space]"
	// Text of a picture taken out of an older tool result, and of one the person attached
	screenshotPlaceholder = "[screenshot removed to save context]"
	attachedPlaceholder   = "[Attached image — stripped after compression]"
	// Shortest tool result worth cutting down
	pruneMinChars = 200
	// Tool results with pictures that keep them
	keepToolImages = 3
	// Messages at the end of the tail never cut down under pressure
	pressureKeep = 3
	// Skills loaded this many messages from the end keep their loaded text
	skillRecentWindow = 10
	// Most reload notes of skills a summary carries
	maxSkillMarkers = 20
	// Newest tool rounds of the lean tail kept whole, and the shortest result of an older round cut to a line
	leanKeepRounds   = 6
	leanDemoteChars  = 1_500
	prunedSkillsHead = "## Pruned Skills"
	skillSafetyHead  = "## Skill Safety Rule"
	skillSafetyRule  = skillSafetyHead + "\n" +
		"A skill placeholder containing `[SKILL_PRUNED]` lost its content in context compression and is inaccessible — " +
		"reload it with skill_view(name='...') before acting on anything that depends on it. After reloading, ignore any " +
		"remaining `[SKILL_PRUNED]` markers for that same skill; they are historical artifacts of earlier compactions."
	skillSafetySuffix = "\n\n" + skillSafetyRule
)

var (
	exitCode     = regexp.MustCompile(`"exit_code"\s*:\s*(-?\d+)`)
	totalCount   = regexp.MustCompile(`"total_count"\s*:\s*(\d+)`)
	demotedLine  = regexp.MustCompile(`^\[[^\s\]]* output demoted at compaction — [\d,]+ chars preserved in session history\.`)
	invokedSkill = regexp.MustCompile(`^\[IMPORTANT: The user has invoked the "([^"]*)"`)
	bundleSkills = regexp.MustCompile(`(?m)^Skills loaded: (.+)$`)
	autoSkill    = regexp.MustCompile(`\[IMPORTANT: The "([^"]*)" skill is auto-loaded`)
)

// Skills a message of the person loaded: one invoked, those of a bundle, those loaded by the settings
func loadedSkills(text string) []string {
	if strings.HasPrefix(text, `[IMPORTANT: The "`) {
		var out []string
		for _, m := range autoSkill.FindAllStringSubmatch(text, -1) {
			out = append(out, m[1])
		}
		return out
	}
	header, _, _ := strings.Cut(text, "\n")
	if strings.Contains(header, " skill bundle,") {
		m := bundleSkills.FindStringSubmatch(text)
		if m == nil {
			return nil
		}
		var out []string
		for _, name := range strings.Split(m[1], ",") {
			if name = strings.TrimSpace(name); name != "" {
				out = append(out, name)
			}
		}
		return out
	}
	if m := invokedSkill.FindStringSubmatch(text); m != nil && m[1] != "" && strings.HasSuffix(header, "The full skill content is loaded below.]") {
		return []string{m[1]}
	}
	return nil
}

// What the person typed in a message, a skill's loaded text read back as the command that loaded it
func typed(text string) string {
	if len(loadedSkills(text)) > 0 {
		described, _ := sessions.DescribeSkill(text, " ")
		return described
	}
	return text
}

// Whether a line is the one a lean tail cut an older tool result to
func demoted(text string) bool {
	return utf8.RuneCountInString(text) < 1_000 && demotedLine.MatchString(text)
}

// Whether a tool result is already a one line summary: a size summary under 400 code points, or a demoted line
func summaryLine(text string) bool {
	s := strings.ReplaceAll(text, skillSafetySuffix, "")
	return strings.HasPrefix(s, "[") && strings.Contains(s, " chars)") && utf8.RuneCountInString(s) < 400 || demoted(s)
}

// Failure a tool result reports with an error or success false, as a suffix of its summary; empty when it reports none
func failureOf(content string) string {
	var payload map[string]any
	if json.Unmarshal([]byte(content), &payload) != nil {
		return ""
	}
	return payloadFailure(payload)
}

func payloadFailure(payload map[string]any) string {
	failure, hasError := payload["error"]
	success, hasSuccess := payload["success"]
	if !(hasError && config.Truthy(failure)) && !(hasSuccess && success == false) {
		return ""
	}
	return suffix(failure, "FAILED")
}

// The label and the reason's first 80 code points on one line
func suffix(reason any, label string) string {
	preview := ""
	if config.Truthy(reason) {
		preview = strings.Join(strings.Fields(fmt.Sprint(reason)), " ")
		if r := []rune(preview); len(r) > 80 {
			preview = string(r[:80])
		}
	}
	if preview != "" {
		return " " + label + ": " + preview
	}
	return " " + label
}

// Arguments of a call as an object, empty for none or a broken one
func argumentsOf(raw string) map[string]any {
	var args map[string]any
	if json.Unmarshal([]byte(raw), &args) != nil || args == nil {
		return map[string]any{}
	}
	return args
}

// Text of an argument, the default for one absent or null
func text(args map[string]any, key, def string) string {
	value, ok := args[key]
	if !ok || value == nil {
		return def
	}
	if s, ok := value.(string); ok {
		return s
	}
	data, _ := json.Marshal(value)
	return string(data)
}

// The first keep code points and an ellipsis once the text is longer than limit
func clip(s string, limit, keep int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:keep]) + "..."
}

// Whether a skill_view result says it failed rather than loading the skill
func skillViewFailed(content string) bool {
	head, _, _ := strings.Cut(content, " (")
	return failureOf(content) != "" || strings.HasPrefix(content, "[skill_view] ") && strings.Contains(head, " FAILED")
}

type summarizer func(name string, args map[string]any, content string, length, lines int) string

// One line summaries of the tools whose calls say most
var summarizers = map[string]summarizer{
	"terminal": func(_ string, args map[string]any, content string, _, lines int) string {
		code := "?"
		if m := exitCode.FindStringSubmatch(content); m != nil {
			code = m[1]
		}
		return fmt.Sprintf("[terminal] ran `%s` -> exit %s, %d lines output", clip(text(args, "command", ""), 80, 77), code, lines)
	},
	"read_file": func(_ string, args map[string]any, content string, length, _ int) string {
		return fmt.Sprintf("[read_file] read %s from line %s (%s chars)", text(args, "path", "?"), text(args, "offset", "1"), strs.Thousands(length)) +
			failureOf(content)
	},
	"write_file": func(_ string, args map[string]any, content string, _, _ int) string {
		path := text(args, "path", "?")
		if failed := failureOf(content); failed != "" {
			return "[write_file] " + path + failed
		}
		written := "?"
		if config.Truthy(args["content"]) {
			written = strconv.Itoa(strings.Count(text(args, "content", ""), "\n") + 1)
		}
		return "[write_file] wrote to " + path + " (" + written + " lines)"
	},
	"search_files": func(_ string, args map[string]any, content string, _, _ int) string {
		count := "?"
		if m := totalCount.FindStringSubmatch(content); m != nil {
			count = m[1]
		}
		return "[search_files] " + text(args, "target", "content") + " search for '" + text(args, "pattern", "?") + "' in " + text(args, "path", ".") +
			" -> " + count + " matches"
	},
	"patch": func(_ string, args map[string]any, content string, length, _ int) string {
		return fmt.Sprintf("[patch] %s in %s (%s chars result)", text(args, "mode", "replace"), text(args, "path", "?"), strs.Thousands(length)) +
			failureOf(content)
	},
	"web_search": func(_ string, args map[string]any, content string, length, _ int) string {
		return fmt.Sprintf("[web_search] query='%s' (%s chars result)", text(args, "query", "?"), strs.Thousands(length)) + failureOf(content)
	},
	"web_extract": func(_ string, args map[string]any, content string, length, _ int) string {
		urls, _ := args["urls"].([]any)
		first := "?"
		if len(urls) > 0 {
			switch u := urls[0].(type) {
			case string:
				first = u
			case map[string]any:
				first = text(u, "url", text(u, "href", "?"))
			}
		}
		if len(urls) > 1 {
			first += fmt.Sprintf(" (+%d more)", len(urls)-1)
		}
		return "[web_extract] " + first + " (" + strs.Thousands(length) + " chars)" + failureOf(content)
	},
	"delegate_task": func(_ string, args map[string]any, content string, length, _ int) string {
		return "[delegate_task] '" + clip(text(args, "goal", ""), 60, 57) + "' (" + strs.Thousands(length) + " chars result)" + failureOf(content)
	},
	"execute_code": func(_ string, args map[string]any, _ string, _, lines int) string {
		code := text(args, "code", "")
		preview := strings.ReplaceAll(string([]rune(code)[:min(60, utf8.RuneCountInString(code))]), "\n", " ")
		if utf8.RuneCountInString(code) > 60 {
			preview += "..."
		}
		return fmt.Sprintf("[execute_code] `%s` (%d lines output)", preview, lines)
	},
	"skill_view": func(_ string, args map[string]any, content string, length, _ int) string {
		skill := text(args, "name", "?")
		failed := failureOf(content)
		if failed == "" && skillViewFailed(content) {
			failed = " FAILED"
		}
		keep := failed == ""
		if summaryLine(content) {
			keep = strings.Contains(content, contexts.SkillMarkerStart)
		}
		marker := ""
		if skill != "?" && keep {
			marker = " " + contexts.SkillPrunedMarker(skill)
		}
		return "[skill_view] name=" + skill + failed + " (" + strs.Thousands(length) + " chars)" + marker
	},
	"skills_list": func(_ string, args map[string]any, content string, length, _ int) string {
		scope := ""
		if category := text(args, "category", ""); category != "" {
			scope = " category=" + category
		}
		listed := ""
		var payload map[string]any
		if json.Unmarshal([]byte(content), &payload) == nil {
			switch count := payload["count"].(type) {
			case float64:
				listed = fmt.Sprintf(" %d skills", int(count))
			case bool:
				listed = fmt.Sprintf(" %v skills", count)
			}
		}
		return "[skills_list]" + scope + listed + failureOf(content) + " (" + strs.Thousands(length) + " chars)"
	},
	"skill_manage": func(_ string, args map[string]any, content string, length, _ int) string {
		var summary string
		if ops, ok := args["operations"].([]any); ok && len(ops) > 0 {
			var rendered []string
			for _, raw := range ops {
				if op, ok := raw.(map[string]any); ok {
					rendered = append(rendered, text(op, "action", "?")+" "+text(op, "name", "?"))
				}
			}
			summary = "[skill_manage] " + strings.Join(rendered[:min(3, len(rendered))], "; ")
			if len(ops) > 3 {
				summary += fmt.Sprintf(" (+%d more)", len(ops)-3)
			}
		} else {
			summary = "[skill_manage] " + text(args, "action", "?") + " " + text(args, "name", "?")
		}
		return summary + failureOf(content) + " (" + strs.Thousands(length) + " chars)"
	},
	"vision_analyze": func(_ string, args map[string]any, content string, length, _ int) string {
		question := text(args, "question", "")
		return "[vision_analyze] '" + string([]rune(question)[:min(50, utf8.RuneCountInString(question))]) + "' (" + strs.Thousands(length) + " chars)" +
			failureOf(content)
	},
	"memory": func(_ string, args map[string]any, content string, _, _ int) string {
		return fmt.Sprintf("[memory] %s on %s", text(args, "action", "?"), text(args, "target", "?")) + failureOf(content)
	},
	"todo_list": func(string, map[string]any, string, int, int) string { return "[todo] updated task list" },
	"clarify": func(_ string, _ map[string]any, content string, _, _ int) string {
		var payload struct {
			Responses []struct {
				Status   string `json:"status"`
				Response any    `json:"user_response"`
			} `json:"responses"`
		}
		var kept []string
		if json.Unmarshal([]byte(content), &payload) == nil {
			for _, r := range payload.Responses {
				if r.Status != "answered" {
					continue
				}
				values, ok := r.Response.([]any)
				if !ok {
					values = []any{r.Response}
				}
				for _, v := range values {
					if s, ok := v.(string); ok && s != "" {
						kept = append(kept, s)
					}
				}
			}
		}
		if len(kept) == 0 {
			return "[clarify] asked user a question"
		}
		var subject any = kept
		if len(kept) == 1 {
			subject = kept[0]
		}
		data, _ := json.Marshal(subject)
		return contexts.Elide("[clarify] user responded: "+string(data), pruneMinChars-1)
	},
	"text_to_speech": func(_ string, _ map[string]any, content string, length, _ int) string {
		return fmt.Sprintf("[text_to_speech] generated audio (%s chars)", strs.Thousands(length)) + failureOf(content)
	},
	"cronjob_manage": func(_ string, args map[string]any, content string, _, _ int) string {
		var payload map[string]any
		_ = json.Unmarshal([]byte(content), &payload)
		tail := payloadFailure(payload)
		if tail == "" {
			job, _ := payload["job"].(map[string]any)
			execErr, hasErr := job["execution_error"]
			skipped, hasSkipped := job["execution_skipped"]
			success := job["execution_success"]
			switch {
			case hasErr && execErr != nil || (!hasSkipped || skipped == nil) && success == false:
				tail = suffix(execErr, "FAILED")
			case hasSkipped && skipped != nil:
				tail = suffix(skipped, "SKIPPED")
			}
		}
		return "[cronjob] " + text(args, "action", "?") + tail
	},
	"browser_navigate": browse,
	"browser_click":    browse,
	"browser_snapshot": browse,
	"browser_type":     browse,
	"browser_scroll":   browse,
	"browser_vision":   browse,
	"process_manage": func(_ string, args map[string]any, content string, _, _ int) string {
		var payload map[string]any
		_ = json.Unmarshal([]byte(content), &payload)
		tail := payloadFailure(payload)
		if tail == "" {
			if code, ok := payload["exit_code"].(float64); ok && code != 0 && payload["completion_reason"] != "killed" {
				tail = suffix("exit code "+strconv.Itoa(int(code)), "FAILED")
			}
		}
		return "[process] " + text(args, "action", "?") + " session=" + text(args, "session_id", "?") + tail
	},
}

// Summary of a call of a browser tool: the page or the element, and the size of what came back
func browse(name string, args map[string]any, content string, length, _ int) string {
	detail := ""
	if url := text(args, "url", ""); url != "" {
		detail = " " + url
	} else if ref := text(args, "ref", ""); ref != "" {
		detail = " ref=" + ref
	}
	return "[" + name + "]" + detail + " (" + strs.Thousands(length) + " chars)" + failureOf(content)
}

// Summary of a call an approval or a guard refused, below the shortest result worth pruning; false for any other call
func refusedSummary(name string, args map[string]any, content string) (string, bool) {
	var payload map[string]any
	_ = json.Unmarshal([]byte(content), &payload)
	status := payload["status"]
	failure, _ := payload["error"].(string)
	if failure == "" && strings.HasPrefix(strings.TrimLeftFunc(content, unicode.IsSpace), "BLOCKED") {
		failure = strings.TrimSpace(content)
	}
	var outcome string
	switch {
	case status == "pending_approval":
		outcome = "awaiting the user's approval, not run"
	case failure != "" && (status == "blocked" || strings.HasPrefix(strings.TrimLeftFunc(failure, unicode.IsSpace), "BLOCKED")):
		outcome = "BLOCKED, not run"
		if strings.Contains(failure, "NOT consented") {
			outcome += "; the user did NOT consent, do not retry or reach the same outcome another way"
		}
	default:
		return "", false
	}
	target := text(args, "command", "")
	if target == "" {
		target = text(args, "path", "")
	}
	if target != "" {
		target = " `" + clip(target, 60, 57) + "`"
	}
	line := "[" + name + "]" + target + " " + outcome
	return string([]rune(line)[:min(utf8.RuneCountInString(line), pruneMinChars-1)]), true
}

// One line summary of a call and its result
func summarize(name, rawArgs, content string) string {
	args := argumentsOf(rawArgs)
	if refused, ok := refusedSummary(name, args, content); ok {
		return refused
	}
	length, lines := utf8.RuneCountInString(content), 0
	if strings.TrimSpace(content) != "" {
		lines = strings.Count(content, "\n") + 1
	}
	if fn, ok := summarizers[name]; ok {
		return fn(name, args, content, length, lines)
	}
	keys := make([]string, 0, len(args))
	for key := range args {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	first := ""
	for _, key := range keys[:min(2, len(keys))] {
		value := text(args, key, "null")
		first += " " + key + "=" + string([]rune(value)[:min(40, utf8.RuneCountInString(value))])
	}
	return "[" + name + "]" + first + " (" + strs.Thousands(length) + " chars result)" + failureOf(content)
}

// Each call of the replies by its id, with its tool and arguments
func callsByID(messages []*sessionsv1.Message) map[string]*sessionsv1.ToolCall {
	out := map[string]*sessionsv1.ToolCall{}
	for _, m := range messages {
		if isAssistant(m) {
			for _, call := range m.GetToolCalls() {
				out[call.GetId()] = call
			}
		}
	}
	return out
}

// Skill each skill_view call loaded, by call id
func skillViews(messages []*sessionsv1.Message) map[string]string {
	out := map[string]string{}
	for _, m := range messages {
		for _, call := range m.GetToolCalls() {
			if call.GetName() != "skill_view" {
				continue
			}
			if name := text(argumentsOf(call.GetArguments()), "name", ""); name != "" {
				out[call.GetId()] = name
			}
		}
	}
	return out
}

// Skills whose loaded text the messages held, so their reload notes survive the compaction
func ghostedSkills(messages []*sessionsv1.Message) []string {
	views := skillViews(messages)
	var names []string
	for _, m := range messages {
		body := contexts.Text(m)
		names = append(names, contexts.PrunedSkills(body)...)
		switch {
		case isUser(m):
			names = append(names, loadedSkills(body)...)
		case isTool(m) && !skillViewFailed(body) && !demoted(body):
			names = append(names, views[m.GetToolCallId()])
		}
	}
	var out []string
	for _, name := range names {
		if name != "" && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// Summary with a section repeating every reload note it lacks
func withSkillMarkers(summary string, names []string) string {
	var missing []string
	for _, name := range names {
		if marker := contexts.SkillPrunedMarker(name); !strings.Contains(summary, marker) {
			missing = append(missing, marker)
		}
	}
	if len(missing) == 0 {
		return summary
	}
	return summary + masked("\n\n"+prunedSkillsHead+"\n"+strings.Join(missing, "\n"))
}

// Skills loaded recently, or named by a message of the person in the tail, whose loaded text is never pruned, lower case
func protectedSkills(messages []*sessionsv1.Message, boundary int) map[string]bool {
	out := map[string]bool{}
	if len(messages) == 0 {
		return out
	}
	recent := max(0, len(messages)-skillRecentWindow)
	tail := max(0, boundary)
	var said []string
	for _, m := range messages[min(tail, len(messages)):] {
		if isUser(m) && contexts.Text(m) != "" {
			said = append(said, strings.ToLower(contexts.Text(m)))
		}
	}
	for i, m := range messages {
		for _, call := range m.GetToolCalls() {
			if call.GetName() != "skill_view" {
				continue
			}
			name := strings.ToLower(text(argumentsOf(call.GetArguments()), "name", ""))
			if name == "" {
				continue
			}
			if i >= min(recent, tail) || slices.ContainsFunc(said, func(t string) bool { return strings.Contains(t, name) }) {
				out[name] = true
			}
		}
	}
	return out
}

// Pictures of a tool result taken out for a note, nil when it holds none
func stripToolImages(m *sessionsv1.Message) *sessionsv1.Message {
	if !isTool(m) || len(m.GetMedia()) == 0 {
		return nil
	}
	out := contexts.Rewritten(m, strings.TrimSpace(contexts.Text(m)+"\n"+screenshotPlaceholder))
	out.Media = nil
	return out
}

// Cuts the tool result at the index to its one line summary, the loaded text of a protected skill spared; whether it changed it
func demote(messages []*sessionsv1.Message, i int, byID map[string]*sessionsv1.ToolCall, minChars int, protected map[string]bool) bool {
	m := messages[i]
	if !isTool(m) {
		return false
	}
	if len(m.GetMedia()) > 0 {
		if stripped := stripToolImages(m); stripped != nil {
			messages[i] = stripped
			return true
		}
		return false
	}
	content := contexts.Text(m)
	if content == "" || content == prunedPlaceholder || strings.HasPrefix(content, "[Duplicate tool output") || strings.HasPrefix(content, "[screenshot removed") ||
		summaryLine(content) || utf8.RuneCountInString(content) <= minChars {
		return false
	}
	call := byID[m.GetToolCallId()]
	name, args := "unknown", ""
	if call != nil {
		name, args = call.GetName(), call.GetArguments()
	}
	if len(protected) > 0 && name == "skill_view" && protected[strings.ToLower(text(argumentsOf(args), "name", ""))] {
		return false
	}
	messages[i] = contexts.Rewritten(m, summarize(name, args, content))
	return true
}

// Older identical tool results replaced by a note that the newest carries the same, how many
func dedupeResults(messages []*sessionsv1.Message) int {
	pruned, seen := 0, map[string]bool{}
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		content := contexts.Text(m)
		if !isTool(m) || utf8.RuneCountInString(content) < pruneMinChars {
			continue
		}
		sum := md5.Sum([]byte(content))
		key := hex.EncodeToString(sum[:])[:12]
		if seen[key] {
			messages[i] = contexts.Rewritten(m, "[Duplicate tool output — same content as a more recent call]")
			pruned++
		}
		seen[key] = true
	}
	return pruned
}

// Pictures taken out of every tool result but the newest few with pictures and the round still waiting on its answer, how many
func retireImages(messages []*sessionsv1.Message, keep, sparedStart, sparedEnd int) int {
	seen, pruned := 0, 0
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if !isTool(m) || len(m.GetMedia()) == 0 {
			continue
		}
		seen++
		if seen <= max(keep, 0) || i >= sparedStart && i < sparedEnd {
			continue
		}
		if stripped := stripToolImages(m); stripped != nil {
			messages[i] = stripped
			pruned++
		}
	}
	return pruned
}

// First index of the protected tail for pruning: a walk under the token budget held to the count floor, the count alone without a budget
func (c *compressor) pruneBoundary(messages []*sessionsv1.Message, keepCount, keepTokens int) int {
	if keepTokens <= 0 {
		return len(messages) - keepCount
	}
	floor := min(keepCount, len(messages), maxTailFloor)
	boundary, _ := walk(messages, 0, keepTokens, floor, true, c.pricer(messages))
	return min(boundary, len(messages)-floor)
}

// Trailing tool results still waiting on the model, before any steer messages: from and to
func pendingRound(messages []*sessionsv1.Message) (int, int) {
	end := len(messages)
	for end > 0 && messages[end-1].GetDisplay() == "steer" {
		end--
	}
	start := end
	for start > 0 && isTool(messages[start-1]) {
		start--
	}
	return start, end
}

// Round still waiting on the model with the reply that called it, when it fits the tail's share of the input window; 0, 0 otherwise
func (c *compressor) sparedRound(messages []*sessionsv1.Message) (int, int) {
	start, end := pendingRound(messages)
	if end > start && start > 0 && calls(messages[start-1]) {
		start--
	}
	share := int(float64(inputWindow(c.window, c.reserve)) * tailMaxContextFraction)
	total := 0
	for i := start; i < end; i++ {
		total += budgetTokens(messages[i], true)
	}
	if total <= share {
		return start, end
	}
	return 0, 0
}

// Cuts tool results down inside the protected tail when it alone runs over its soft ceiling, the newest few and the waiting round spared;
// how many it cut
func (c *compressor) pressureDemote(messages []*sessionsv1.Message, boundary, keepTokens int, byID map[string]*sessionsv1.ToolCall, minChars, sparedStart,
	sparedEnd int) int {
	soft := c.softCeiling(keepTokens)
	end := len(messages) - min(pressureKeep, len(messages))
	start := max(0, boundary)
	region := func() int {
		total := 0
		for i := start; i < len(messages); i++ {
			total += budgetTokens(messages[i], true)
		}
		return total
	}
	spared := func(i int) bool { return i >= sparedStart && i < sparedEnd }
	demoted := 0
	shrink := func(i int) {
		if !spared(i) && demote(messages, i, byID, minChars, nil) {
			demoted++
		}
	}
	if end <= boundary || region() <= soft {
		return 0
	}
	for i := start; i < end; i++ {
		shrink(i)
		if region() <= soft {
			break
		}
	}
	if region() > soft {
		last := -1
		for i := len(messages) - 1; i >= 0; i-- {
			if isTool(messages[i]) {
				last = i
				break
			}
		}
		for i := start; i < len(messages); i++ {
			if i != last {
				shrink(i)
			}
		}
		if last >= 0 && !spared(last) && last >= boundary && region() > soft && demote(messages, last, byID, minChars, nil) {
			demoted++
		}
	}
	if demoted > 0 {
		slog.Info(fmt.Sprintf("Pre-compression pressure demotion: reclaimed protected-tail tool output (%d change(s); protected region now ~%s tokens, soft ceiling %s)",
			demoted, strs.Thousands(region()), strs.Thousands(soft)))
	}
	return demoted
}

// Copy of the messages with old tool results cut to one line summaries, duplicates noted, older pictures dropped and the tail cut down
// when it runs over; how many changed
func (c *compressor) pruneResults(messages []*sessionsv1.Message, keepCount, keepTokens, minChars int) ([]*sessionsv1.Message, int) {
	if len(messages) == 0 {
		return messages, 0
	}
	result := slices.Clone(messages)
	byID := callsByID(result)
	boundary := c.pruneBoundary(result, keepCount, keepTokens)
	sparedStart, sparedEnd := c.sparedRound(result)
	if sparedEnd > sparedStart {
		boundary = min(boundary, sparedStart)
	}
	pruned := dedupeResults(result)
	protected := protectedSkills(result, boundary)
	for i := 0; i < max(0, boundary); i++ {
		if demote(result, i, byID, minChars, protected) {
			pruned++
		}
	}
	pruned += retireImages(result, keepToolImages, sparedStart, sparedEnd)
	if keepTokens > 0 {
		pruned += c.pressureDemote(result, boundary, keepTokens, byID, minChars, sparedStart, sparedEnd)
	}
	return result, pruned
}

// Line a lean tail cuts an older tool result to, pointing at session_search to read it back
func recoveryLine(name string, length int, session string) string {
	hint := ""
	if session != "" {
		hint = " Recover with session_search(query=..., session_id='" + session + "')"
	}
	return "[" + cmp.Or(name, "tool") + " output demoted at compaction — " + strs.Thousands(length) + " chars preserved in session history." + hint + "]"
}

// Tail tool results older than the newest six rounds cut to recovery lines, how many
func demoteStaleTail(messages []*sessionsv1.Message, tailStart int, session string) ([]*sessionsv1.Message, int) {
	rounds, previous := 0, -1
	protected := map[int]bool{}
	for i := len(messages) - 1; i >= tailStart; i-- {
		if !isTool(messages[i]) {
			continue
		}
		if previous < 0 || previous-i > 1 {
			rounds++
		}
		previous = i
		if rounds > leanKeepRounds {
			break
		}
		protected[i] = true
	}
	result := slices.Clone(messages)
	views := skillViews(messages)
	count := 0
	for i := max(tailStart, 0); i < len(messages); i++ {
		m := messages[i]
		content := contexts.Text(m)
		if !isTool(m) || protected[i] || len(m.GetMedia()) > 0 {
			continue
		}
		length := utf8.RuneCountInString(content)
		if length < leanDemoteChars || strings.Contains(content, contexts.SkillMarkerStart) || summaryLine(content) {
			continue
		}
		line := recoveryLine(m.GetToolName(), length, session)
		if skill := views[m.GetToolCallId()]; skill != "" && failureOf(content) == "" {
			line += " " + contexts.SkillPrunedMarker(skill)
		}
		result[i] = contexts.Rewritten(m, line)
		count++
	}
	if count > 0 {
		slog.Info(fmt.Sprintf("Lean tail: demoted %d stale tool result(s)", count))
	}
	return result, count
}

// Summary, head and tail with the skill safety rule exactly once, on the summary, when any of them holds a reload note
func withSkillSafety(summary string, head, tail []*sessionsv1.Message) (string, []*sessionsv1.Message, []*sessionsv1.Message) {
	unruled := func(rows []*sessionsv1.Message) []*sessionsv1.Message {
		out := make([]*sessionsv1.Message, len(rows))
		for i, m := range rows {
			out[i] = m
			if body := contexts.Text(m); strings.Contains(body, skillSafetyRule) {
				out[i] = contexts.Rewritten(m, strings.ReplaceAll(body, skillSafetySuffix, ""))
			}
		}
		return out
	}
	head, tail = unruled(head), unruled(tail)
	ruled, marked := strings.Contains(summary, skillSafetyRule), strings.Contains(summary, contexts.SkillMarkerStart)
	for _, m := range append(slices.Clone(head), tail...) {
		body := contexts.Text(m)
		ruled = ruled || strings.Contains(body, skillSafetyRule)
		marked = marked || strings.Contains(body, contexts.SkillMarkerStart)
	}
	if ruled || !marked {
		return summary, head, tail
	}
	return summary + skillSafetySuffix, head, tail
}

// Text cut to the limit keeping an attached skill safety rule whole
func elideKeepingRule(content string, limit int) string {
	if !strings.Contains(content, skillSafetyRule) {
		return contexts.Elide(content, limit)
	}
	return contexts.Elide(strings.ReplaceAll(content, skillSafetySuffix, ""), limit-utf8.RuneCountInString(skillSafetySuffix)) + skillSafetySuffix
}

// Pruned messages with the safety rule on the first one a prune gave a reload note, when none carries it yet
func withPrunedSkillSafety(original, pruned []*sessionsv1.Message) []*sessionsv1.Message {
	for _, m := range pruned {
		if strings.Contains(contexts.Text(m), skillSafetyRule) {
			return pruned
		}
	}
	for i := 0; i < min(len(original), len(pruned)); i++ {
		after := contexts.Text(pruned[i])
		if strings.Contains(after, contexts.SkillMarkerStart) && after != contexts.Text(original[i]) {
			out := slices.Clone(pruned)
			out[i] = contexts.Rewritten(pruned[i], after+skillSafetySuffix)
			return out
		}
	}
	return pruned
}
