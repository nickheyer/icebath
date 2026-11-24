package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Glyph, preview field and preview length of one memory operation line
type memoryOpFormat struct {
	glyph, field string
	limit        int
}

var memoryOpFormats = map[string]memoryOpFormat{
	"add": {"➕", "content", 120}, "replace": {"✏️", "content", 120}, "patch": {"✏️", "content", 120},
	"remove": {"➖", "old_text", 60},
}

// Tool call argument fields surfaced in action summaries with their defaults
var callDetailDefaults = [][2]string{
	{"action", "?"}, {"target", "memory"}, {"content", ""}, {"old_text", ""}, {"name", ""}, {"old_string", ""}, {"new_string", ""},
}

// Python exception raised while summarizing a malformed review transcript
type reviewSummaryError struct{ msg string }

func (e reviewSummaryError) Error() string { return e.msg }

// Dict view of a mapping value, a decoded JSON object included, false for anything else
func reviewDict(v any) (map[string]any, bool) {
	if o, ok := v.(*jsonutil.Object); ok && o != nil {
		m := make(map[string]any, o.Len())
		for _, k := range o.Keys() {
			m[k] = o.Value(k)
		}
		return m, true
	}
	return pyDict(v)
}

// Value of key or the default like dict.get
func reviewGet(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// Text of a review field, a review failure naming the field for anything else
func reviewStr(v any, what string) string {
	s, ok := v.(string)
	if ok {
		return s
	}
	panic(reviewSummaryError{fmt.Sprintf("review %s is %s where text was expected", what, errchain.Kind(v))})
}

// Key of an id placed in a set, a review failure for a list or mapping id
func reviewHashKey(v any) any {
	switch v.(type) {
	case map[string]any, []any, *jsonutil.Object, []map[string]any:
		panic(reviewSummaryError{fmt.Sprintf("review id is %s where text or a number was expected", errchain.Kind(v))})
	}
	return v
}

// List view of a value, false for anything that is not a list
func reviewList(v any) ([]any, bool) {
	switch l := v.(type) {
	case []any:
		return l, true
	case []map[string]any:
		return reviewToolCalls(l), true
	}
	return nil, false
}

// First limit code points of text with an ellipsis when cut
func preview(text string, limit int) string {
	if len([]rune(text)) > limit {
		return runePrefix(text, limit) + "…"
	}
	return text
}

// Verbose line of one memory add, replace or remove, false when the operation carries no preview text
func memoryOpLine(ctx context.Context, label any, action any, fields map[string]any) (string, bool) {
	format, ok := memoryOpFormats[pyKeyString(action)]
	if !ok || !pyTruthy(reviewGet(fields, format.field, nil)) {
		return "", false
	}
	text := reviewStr(fields[format.field], format.field)
	return i18n.T(ctx, "display.review.memory_op_line", "", map[string]any{"label": label, "glyph": format.glyph,
		"preview": preview(text, format.limit)}), true
}

// String key of a hashable lookup value, empty for non string keys that match nothing
func pyKeyString(v any) string {
	reviewHashKey(v)
	s, _ := v.(string)
	return s
}

// Verbose line of one skill_manage result, the patch diff, the change description or the message
func verboseSkillLine(ctx context.Context, data, detail map[string]any, message any) string {
	action, skillName := reviewGet(detail, "action", ""), reviewGet(detail, "name", "")
	change, ok := reviewDict(data["_change"])
	if !ok {
		change = map[string]any{}
	}
	oldString := reviewGet(change, "old", "")
	if !pyTruthy(oldString) {
		oldString = reviewGet(detail, "old_string", "")
	}
	newString := reviewGet(change, "new", "")
	if !pyTruthy(newString) {
		newString = reviewGet(detail, "new_string", "")
	}
	if action == "patch" && (pyTruthy(oldString) || pyTruthy(newString)) {
		oldPreview := strings.ReplaceAll(preview(reviewStr(oldString, "old_string"), 80), "\n", " ")
		newPreview := strings.ReplaceAll(preview(reviewStr(newString, "new_string"), 80), "\n", " ")
		return i18n.T(ctx, "display.review.skill_patched_verbose", "", map[string]any{"name": skillName, "old": oldPreview, "new": newPreview})
	}
	verbKey := map[string]string{"create": "created", "edit": "rewritten"}[pyKeyString(action)]
	if verbKey != "" && pyTruthy(change["description"]) {
		return i18n.T(ctx, "display.review.skill_changed_verbose", "", map[string]any{"name": skillName,
			"verb": i18n.T(ctx, "display.review.skill_verb."+verbKey, "", nil), "description": change["description"]})
	}
	if pyTruthy(message) {
		return i18n.T(ctx, "display.review.skill_message_verbose", "", map[string]any{"message": message})
	}
	return i18n.T(ctx, "display.review.skill_action", "", map[string]any{"action": action})
}

// Verbose memory lines, one per dict operation with a preview, else one line for the whole call
func verboseMemoryLines(ctx context.Context, label any, detail map[string]any) []string {
	if ops, ok := reviewList(detail["operations"]); ok && len(ops) > 0 {
		lines := []string{}
		for _, raw := range ops {
			op, ok := reviewDict(raw)
			if !ok {
				continue
			}
			if line, ok := memoryOpLine(ctx, label, reviewGet(op, "action", ""), op); ok {
				lines = append(lines, line)
			}
		}
		return lines
	}
	if line, ok := memoryOpLine(ctx, label, reviewGet(detail, "action", ""), detail); ok {
		return []string{line}
	}
	return []string{i18n.T(ctx, "display.review.label_updated", "", map[string]any{"label": label})}
}

// Every review tool call id and the parsed arguments of memory and skill_manage calls by id
func collectReviewCallDetails(reviewMessages []map[string]any) (map[any]bool, map[any]map[string]any) {
	notifyTools := map[string]bool{"memory": true, "skill_manage": true}
	allIDs, details := map[any]bool{}, map[any]map[string]any{}
	for _, msg := range reviewMessages {
		if msg == nil || msg["role"] != "assistant" {
			continue
		}
		calls := reviewGet(msg, "tool_calls", []any{})
		if !pyTruthy(calls) {
			continue
		}
		list, ok := reviewList(calls)
		if !ok {
			if _, isDict := reviewDict(calls); isDict {
				continue
			}
			if _, isStr := calls.(string); isStr {
				continue
			}
			panic(reviewSummaryError{fmt.Sprintf("review tool_calls is %s where a list was expected", errchain.Kind(calls))})
		}
		for _, raw := range list {
			tc, ok := reviewDict(raw)
			if !ok {
				continue
			}
			fnRaw := reviewGet(tc, "function", map[string]any{})
			if !pyTruthy(fnRaw) {
				fnRaw = map[string]any{}
			}
			fn, ok := reviewDict(fnRaw)
			if !ok {
				panic(reviewSummaryError{fmt.Sprintf("review tool call function is %s where a mapping was expected", errchain.Kind(fnRaw))})
			}
			fnName, tcid := reviewGet(fn, "name", ""), tc["id"]
			if pyTruthy(tcid) {
				allIDs[reviewHashKey(tcid)] = true
			}
			name := pyKeyString(fnName)
			if !notifyTools[name] || !pyTruthy(tcid) {
				continue
			}
			args := map[string]any{}
			if text, ok := reviewGet(fn, "arguments", "{}").(string); ok {
				if parsed, err := jsonutil.Decode(text); err == nil {
					d, ok := reviewDict(parsed)
					if !ok {
						panic(reviewSummaryError{fmt.Sprintf("review tool call arguments are %s where a mapping was expected", errchain.Kind(parsed))})
					}
					args = d
				}
			}
			ops := reviewGet(args, "operations", nil)
			if !pyTruthy(ops) {
				ops = []any{}
			}
			detail := map[string]any{"tool": name, "operations": ops}
			for _, kv := range callDetailDefaults {
				detail[kv[0]] = reviewGet(args, kv[0], kv[1])
			}
			details[reviewHashKey(tcid)] = detail
		}
	}
	return allIDs, details
}

// Tool role messages of a transcript
func reviewToolMessages(messages []map[string]any) []map[string]any {
	var out []map[string]any
	for _, m := range messages {
		if m != nil && m["role"] == "tool" {
			out = append(out, m)
		}
	}
	return out
}

// Tool call ids and id less string contents of tool messages already in the parent snapshot
func priorToolKeys(priorSnapshot []map[string]any) (map[any]bool, map[string]bool) {
	ids, contents := map[any]bool{}, map[string]bool{}
	for _, m := range reviewToolMessages(priorSnapshot) {
		if id := m["tool_call_id"]; pyTruthy(id) {
			ids[reviewHashKey(id)] = true
		} else if s, ok := m["content"].(string); ok {
			contents[s] = true
		}
	}
	return ids, contents
}

// Summary lines of one successful memory or skill_manage result, none when nothing applied
func actionLines(ctx context.Context, data, detail map[string]any, verbose bool) []string {
	if pyTruthy(data["staged"]) {
		if pyTruthy(data["proposal_staged"]) && pyTruthy(data["message"]) {
			return []string{anyutil.String(data["message"])}
		}
		return []string{}
	}
	message := reviewGet(data, "message", "")
	target := reviewGet(data, "target", "")
	if !pyTruthy(target) {
		target = reviewGet(detail, "target", "")
	}
	isSkill := detail["tool"] == "skill_manage"
	if _, hasResults := data["results"]; isSkill && hasResults {
		verbs := map[string]string{"create": "created", "patch": "patched", "edit": "rewritten", "write_file": "written",
			"remove_file": "removed", "delete": "deleted"}
		results, ok := reviewList(data["results"])
		if !pyTruthy(data["operations_applied"]) || !ok {
			return []string{}
		}
		lines := []string{}
		for _, raw := range results {
			result, ok := reviewDict(raw)
			if !ok || result["success"] != true {
				continue
			}
			verbKey := verbs[pyKeyString(result["action"])]
			if verbKey == "" || !pyTruthy(result["name"]) {
				continue
			}
			path := ""
			if pyTruthy(result["file_path"]) {
				path = " (" + anyutil.String(result["file_path"]) + ")"
			}
			lines = append(lines, i18n.T(ctx, "display.review.skill_result", "", map[string]any{"name": result["name"],
				"verb": i18n.T(ctx, "display.review.skill_verb."+verbKey, "", nil), "path": path}))
		}
		return lines
	}
	lower := strings.ToLower(reviewStr(message, "message"))
	if !verbose && (strings.Contains(lower, "created") || strings.Contains(lower, "updated") ||
		(isSkill && (strings.Contains(lower, "patched") || strings.Contains(lower, "deleted") || strings.Contains(lower, "written")))) {
		return []string{message.(string)}
	}
	if !isSkill && !pyTruthy(target) {
		return []string{}
	}
	labelKey := "skill"
	if !isSkill {
		labelKey = map[string]string{"memory": "memory", "user": "user"}[pyKeyString(target)]
	}
	var label any = target
	if labelKey != "" {
		label = i18n.T(ctx, "display.review.label."+labelKey, "", nil)
	}
	if verbose {
		if isSkill {
			return []string{verboseSkillLine(ctx, data, detail, message)}
		}
		return verboseMemoryLines(ctx, label, detail)
	}
	hit := strings.Contains(lower, "added") || strings.Contains(lower, "replaced") || strings.Contains(lower, "removed") ||
		strings.Contains(lower, "applied")
	if !hit && pyTruthy(target) {
		hit = strings.Contains(lower, "add")
	}
	if hit {
		return []string{i18n.T(ctx, "display.review.label_updated", "", map[string]any{"label": label})}
	}
	return []string{}
}

// Successful memory and skill actions of a review pass, skipping tool results inherited from the parent snapshot
func summarizeBackgroundReviewActions(ctx context.Context, reviewMessages, priorSnapshot []map[string]any, notificationMode any) (actions []string, err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(reviewSummaryError); ok {
				actions, err = nil, e
				return
			}
			panic(r)
		}
	}()
	if !pyTruthy(notificationMode) {
		notificationMode = "on"
	}
	mode := strings.ToLower(anyutil.String(notificationMode))
	if mode == "off" {
		return []string{}, nil
	}
	verbose := mode == "verbose"
	existingIDs, existingContents := priorToolKeys(priorSnapshot)
	allIDs, details := collectReviewCallDetails(reviewMessages)
	actions = []string{}
	for _, msg := range reviewToolMessages(reviewMessages) {
		tcid := msg["tool_call_id"]
		if pyTruthy(tcid) {
			key := reviewHashKey(tcid)
			if _, known := details[key]; existingIDs[key] || (len(allIDs) > 0 && !known) {
				continue
			}
		} else if s, ok := msg["content"].(string); ok && existingContents[s] {
			continue
		}
		text, ok := reviewGet(msg, "content", "{}").(string)
		if !ok {
			continue
		}
		parsed, perr := jsonutil.Decode(text)
		if perr != nil {
			continue
		}
		data, ok := reviewDict(parsed)
		if !ok || !pyTruthy(data["success"]) {
			continue
		}
		var detail map[string]any
		if tcid != nil {
			detail = details[reviewHashKey(tcid)]
		}
		if detail == nil {
			detail = map[string]any{}
		}
		actions = append(actions, actionLines(ctx, data, detail, verbose)...)
	}
	return actions, nil
}

// Provenance metadata for external memory provider mirrors, empty values dropped
func buildMemoryWriteMetadata(a *AIAgent, writeOrigin, executionContext, taskID, toolCallID string) map[string]any {
	platform := a.platform
	if platform == "" {
		platform = tools.GetSessionEnv(a.env, "ICEBATH_SESSION_SOURCE", "cli")
	}
	metadata := map[string]any{
		"write_origin":      pyStrOr(writeOrigin, pyStrOr(a.memoryWriteOrigin, "assistant_tool")),
		"execution_context": pyStrOr(executionContext, pyStrOr(a.memoryWriteContext, "foreground")),
		"session_id":        a.sessionID, "parent_session_id": a.parentSessionID, "platform": platform,
		"tool_name": "memory", "task_id": taskID, "tool_call_id": toolCallID,
	}
	for k, v := range metadata {
		if v == "" {
			delete(metadata, k)
		}
	}
	return metadata
}
