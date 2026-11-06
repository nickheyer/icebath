package agent

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	steerFallbackOpen             = "[OUT-OF-BAND USER MESSAGE"
	steerFallbackClose            = "[/OUT-OF-BAND USER MESSAGE]"
	compressedUserTurnInserted    = "inserted"
	compressedUserTurnMerged      = "merged"
	compressedUserTurnPresent     = "already_present"
	compressedUserTurnPlaceholder = "placeholder_appended"
)

// Open and close steer markers of the prompt builder
func steerMarkers() (string, string) { return SteerMarkerOpen, SteerMarkerClose }

// Whether the message carries a busy steer marker pair
func messageContainsBusySteer(message map[string]any) bool {
	text := compressionMessageText(message)
	if text == "" {
		return false
	}
	open, close := steerMarkers()
	return strings.Contains(text, open) && strings.Contains(text, close)
}

// Inner user text of a steer marker, false when none is present
func extractSteerTextFromMessage(message map[string]any) (string, bool) {
	text := compressionMessageText(message)
	if text == "" {
		return "", false
	}
	open, close := steerMarkers()
	start := strings.Index(text, open)
	if start == -1 {
		start = strings.Index(text, steerFallbackOpen)
		if start == -1 {
			return "", false
		}
		if nl := strings.Index(text[start:], "\n"); nl != -1 {
			start += nl + 1
		} else {
			start += len(steerFallbackOpen)
		}
	} else {
		start += len(open)
	}
	end := strings.Index(text[start:], close)
	if end == -1 {
		end = strings.Index(text[start:], steerFallbackClose)
		if end == -1 {
			return "", false
		}
	}
	inner := pystr.Strip(text[start : start+end])
	return inner, inner != ""
}

// Whether a tool row of the transcript carries a steer marker
func compressedHasBusySteer(messages []map[string]any) bool {
	for _, m := range messages {
		if m != nil && m["role"] == "tool" && messageContainsBusySteer(m) {
			return true
		}
	}
	return false
}

// Content with any previously merged todo snapshot block removed
func stripStaleTodoSnapshot(content any) any {
	switch c := content.(type) {
	case string:
		idx := strings.Index(c, tools.TodoInjectionHeader)
		if idx == -1 {
			return c
		}
		return pystr.Rstrip(c[:idx])
	case []any:
		cleaned := []any{}
		for _, part := range c {
			text := ""
			if p, ok := part.(map[string]any); ok && p["type"] == "text" {
				if p["text"] != nil && p["text"] != "" {
					text = fmt.Sprint(p["text"])
				}
			}
			idx := -1
			if text != "" {
				idx = strings.Index(text, tools.TodoInjectionHeader)
			}
			if idx == -1 {
				cleaned = append(cleaned, part)
			} else if stripped := pystr.Rstrip(text[:idx]); stripped != "" {
				clone := map[string]any{}
				for k, v := range part.(map[string]any) {
					clone[k] = v
				}
				clone["text"] = stripped
				cleaned = append(cleaned, clone)
			}
		}
		return cleaned
	}
	return content
}

// Whether stripping the snapshot left no structured content at all
func todoSnapshotIsOnlyContent(content, stripped any) bool {
	_, ok1 := content.(string)
	ss, ok2 := stripped.(string)
	if ok1 && ok2 {
		return pystr.Strip(ss) == ""
	}
	_, ok1 = content.([]any)
	sl, ok2 := stripped.([]any)
	if ok1 && ok2 {
		return len(sl) == 0
	}
	return false
}

// Rewrites message content and drops the stale API sidecar so it never replays
func replaceMessageContent(message map[string]any, content any) {
	message["content"] = content
	dropStaleAPIContent(message)
}

// Reload notice naming each pruned skill once in first seen order, empty when none was pruned
func prunedSkillReloadNotice(compressed []map[string]any) string {
	var names []string
	for _, m := range compressed {
		if m == nil {
			continue
		}
		for _, name := range extractPrunedSkillNames(compressionMessageText(m)) {
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	if len(names) > maxPrunedSkillMarkers {
		names = names[:maxPrunedSkillMarkers]
	}
	if len(names) == 0 {
		return ""
	}
	calls := make([]string, len(names))
	for i, name := range names {
		calls[i] = fmt.Sprintf("skill_view(name='%s')", name)
	}
	return prunedSkillReloadNoticeHeader + "\n" +
		"The task list above crossed the compression boundary verbatim, but " +
		"the skill instructions that governed it were pruned. Before " +
		"executing any preserved task that depends on these skills, reload " +
		"them first: " + strings.Join(calls, "; ") + ". After reloading, re-check that each pending " +
		"task is still justified — findings recorded before the boundary may have invalidated it."
}

// Folds the human anchor into a trailing user scaffolding turn, anchor text leading
func mergeAnchorIntoUserMessage(target, anchor map[string]any) {
	anchorContent, targetContent := anchor["content"], target["content"]
	_, anchorList := anchorContent.([]any)
	_, targetList := targetContent.([]any)
	if anchorList || targetList {
		parts := func(content any) []any {
			if l, ok := content.([]any); ok {
				return append([]any{}, l...)
			}
			return []any{map[string]any{"type": "text", "text": strOrEmpty(content)}}
		}
		replaceMessageContent(target, append(parts(anchorContent), parts(targetContent)...))
	} else {
		replaceMessageContent(target, pystr.Strip(strOrEmpty(anchorContent)+"\n\n"+strOrEmpty(targetContent)))
	}
	for _, flag := range syntheticUserFlags {
		delete(target, flag)
	}
	state.RecordAbsorbedMessage(target, anchor, true)
}

// str(content or "") of Python
func strOrEmpty(v any) string {
	if !pyTruthy(v) {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// Inserts the latest human turn without breaking role alternation
func insertRealUserAnchor(messages *[]map[string]any, anchor map[string]any) string {
	role := func(m map[string]any) any {
		if m == nil {
			return nil
		}
		return m["role"]
	}
	place := func(index int) string {
		anchor[dbPersistedMarker] = true
		*messages = slices.Insert(*messages, index, anchor)
		return compressedUserTurnInserted
	}
	for i, m := range *messages {
		if role(m) == "assistant" && (i == 0 || role((*messages)[i-1]) != "user") {
			return place(i)
		}
	}
	list := *messages
	if len(list) == 0 || role(list[len(list)-1]) != "user" || state.IsContextSummaryContent(compressionMessageText(list[len(list)-1])) {
		return place(len(list))
	}
	mergeAnchorIntoUserMessage(list[len(list)-1], anchor)
	list[len(list)-1][dbPersistedMarker] = true
	return compressedUserTurnMerged
}

// Keeps a real human intent in the compacted transcript, a continuation placeholder only when none exists
func ensureCompressedHasUserTurn(original []map[string]any, compressed *[]map[string]any) string {
	for _, m := range *compressed {
		if isRealUserMessage(m) {
			return compressedUserTurnPresent
		}
	}
	if compressedHasBusySteer(*compressed) {
		return compressedUserTurnPresent
	}
	for _, m := range *compressed {
		if hasMergedInflightReplay(m) {
			return compressedUserTurnPresent
		}
	}
	for i := len(original) - 1; i >= 0; i-- {
		m := original[i]
		if isRealUserMessage(m) {
			return insertRealUserAnchor(compressed, freshCompactionMessageCopy(m))
		}
		if m == nil || m["role"] != "tool" {
			continue
		}
		if text, ok := extractSteerTextFromMessage(m); ok {
			return insertRealUserAnchor(compressed, map[string]any{"role": "user", "content": text})
		}
	}
	state.AppendMessage(compressed, map[string]any{"role": "user", "content": state.CompressionContinuationUserContent}, nil)
	return compressedUserTurnPlaceholder
}

// Role, content and when both carry one the timestamp of the live turn identity
func messagesMatchScopedIdentity(left, right map[string]any) bool {
	if left == nil || right == nil || !pyEqual(left["role"], right["role"]) || !pyEqual(left["content"], right["content"]) {
		return false
	}
	lt, rt := left["timestamp"], right["timestamp"]
	if lt != nil && rt != nil {
		return pyEqual(lt, rt)
	}
	return true
}

// Python == over decoded JSON values, numbers compared by value
func pyEqual(x, y any) bool {
	if fx, ok := pyNumber(x); ok {
		if fy, ok := pyNumber(y); ok {
			return fx == fy
		}
	}
	return reflect.DeepEqual(x, y)
}

func pyNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// Stamps the persisted marker on every unstamped scoped twin, exact timestamp twins preferred
func stampScopedTwins(targets []map[string]any, source map[string]any, exactCountsStamped bool) {
	sourceTimestamp := source["timestamp"]
	if sourceTimestamp != nil {
		exactHit := false
		for _, target := range targets {
			if target == nil || !pyEqual(target["timestamp"], sourceTimestamp) || !messagesMatchScopedIdentity(target, source) {
				continue
			}
			if pyTruthy(target[dbPersistedMarker]) {
				exactHit = exactHit || exactCountsStamped
				continue
			}
			target[dbPersistedMarker] = true
			exactHit = true
		}
		if exactHit {
			return
		}
	}
	for _, target := range targets {
		if target != nil && !pyTruthy(target[dbPersistedMarker]) && messagesMatchScopedIdentity(target, source) {
			target[dbPersistedMarker] = true
		}
	}
}

// Flush baseline after a compression boundary, nil for a rotation so the child gets the full compacted list
func conversationHistoryAfterCompression(a *AIAgent, messages, previousHistory []map[string]any) []map[string]any {
	if a.lastCompressionAttemptRecorded {
		if inPlace := a.lastCompressionAttemptInPlace; inPlace != nil {
			if *inPlace {
				return append([]map[string]any{}, messages...)
			}
			return nil
		}
		return previousHistory
	}
	if a.lastCompactionInPlace {
		return append([]map[string]any{}, messages...)
	}
	return nil
}
