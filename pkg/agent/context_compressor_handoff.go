package agent

import (
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	userAskedAttributionRE = mustPyRE(`(?i)\bUser\s+asked\s*:`)
)

// Summary body without the current, legacy or any historical prefix and without the end marker
func stripSummaryPrefix(summary string) string {
	text := pystr.Strip(summary)
	if _, after, found := strings.Cut(text, state.MergedSummaryDelimiter); found {
		text = pystr.Strip(after)
	}
	for _, prefix := range append([]string{state.SummaryPrefix, state.LegacySummaryPrefix}, state.HistoricalSummaryPrefixes()...) {
		if strings.HasPrefix(text, prefix) {
			text = pystr.Lstrip(text[len(prefix):])
			break
		}
	}
	if idx := strings.Index(text, state.SummaryEndMarker); idx >= 0 {
		text = pystr.Rstrip(text[:idx])
	}
	return text
}

// Summary text normalized to the current handoff format
func withSummaryPrefix(summary string) string {
	if text := stripSummaryPrefix(summary); text != "" {
		return state.SummaryPrefix + "\n" + text
	}
	return state.SummaryPrefix
}

// Whether the rows hold a user authored turn rather than synthetic scaffolding
func transcriptHasRealUserTurn(messages []map[string]any) bool {
	for _, m := range messages {
		if m != nil && m["role"] == "user" && !state.IsSyntheticCompressionUserTurn(m) {
			return true
		}
	}
	return false
}

// Rejects user attribution when the source transcript has no user
func validateSummaryUserProvenance(summary string, hasUserTurn bool) error {
	if hasUserTurn {
		return nil
	}
	snapshot := ""
	if m, err := historicalTaskSectionRE.FindStringMatch(summary); err == nil && m != nil {
		parts := strings.SplitN(m.String(), "\n", 2)
		snapshot = pystr.Strip(parts[len(parts)-1])
	}
	attributed, _ := userAskedAttributionRE.MatchString(summary)
	if snapshot != noUserTaskSentinel || attributed {
		return &summaryRuntimeError{"Context compression summary invented user attribution for a session with no user-authored turns"}
	}
	return nil
}

// Contiguous blank echoes after a user event, removable only when an assistant follows
func blankEchoIndicesAfter(messages []map[string]any, userIdx int) map[int]bool {
	out := map[int]bool{}
	if userIdx < 0 {
		return out
	}
	idx := userIdx + 1
	for idx < len(messages) && state.IsBlankUserTurn(messages[idx]) {
		idx++
	}
	if idx == userIdx+1 || idx >= len(messages) || messages[idx]["role"] != "assistant" {
		return out
	}
	for i := userIdx + 1; i < idx; i++ {
		out[i] = true
	}
	return out
}

// Deterministic task snapshot line from the newest real user turn, empty when none exists
func latestUserTaskSnapshot(messages []map[string]any) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg["role"] != "user" || !isRealUserMessage(msg) {
			continue
		}
		text := redactCompactionText(pystr.Strip(typedUserText(state.ContentTextForContains(msg["content"]))))
		if text == "" {
			continue
		}
		if utf8.RuneCountInString(text) > activeTaskMaxChars {
			if authored := redactCompactionText(authoredRequestText(msg["content"])); authored != "" {
				text = authored
			}
		}
		text, _ = whitespaceRunRE.Replace(text, " ", -1, -1)
		if utf8.RuneCountInString(text) <= activeTaskMaxChars {
			text = pyrepr.Repr(text)
		} else {
			text = tools.Elide(pyrepr.Repr(text), activeTaskMaxChars)
		}
		return "User asked (deterministic, from compacted turns): " + text + "\n" +
			"Historical only; newer protected-tail messages after this summary win."
	}
	return ""
}

// Forces the task snapshot section to match a real user turn when one exists
func groundHistoricalTaskSnapshot(summary string, messages []map[string]any) string {
	snapshot := latestUserTaskSnapshot(messages)
	if snapshot == "" {
		return summary
	}
	body := stripSummaryPrefix(summary)
	replacement := state.HistoricalTaskHeading + "\n" + snapshot + "\n\n"
	if m, err := historicalTaskSectionRE.FindStringMatch(body); err == nil && m != nil {
		seen := false
		out, _ := historicalTaskSectionRE.ReplaceFunc(body, func(regexp2.Match) string {
			if seen {
				return ""
			}
			seen = true
			return replacement
		}, -1, -1)
		return pystr.Strip(out)
	}
	return pystr.Strip(replacement + body)
}

type contextSummaryRef struct {
	idx  int
	body string
}

// Handoff summaries inside a compression window with their unprefixed bodies
func findContextSummaries(messages []map[string]any, start, end int) []contextSummaryRef {
	n := len(messages)
	start = max(0, min(start, n))
	end = max(start, min(end, n))
	var out []contextSummaryRef
	for idx := start; idx < end; idx++ {
		if messages[idx] != nil && state.IsCompactionSummaryMessage(messages[idx]) {
			out = append(out, contextSummaryRef{idx, stripSummaryPrefix(state.ContentTextForContains(messages[idx]["content"]))})
		}
	}
	return out
}

// Newest handoff summary inside a compression window, index minus one when none
func findLatestContextSummary(messages []map[string]any, start, end int) (int, string) {
	found := findContextSummaries(messages, start, end)
	if len(found) == 0 {
		return -1, ""
	}
	last := found[len(found)-1]
	return last.idx, last.body
}

// Canonical call id for logging, matching uses the id variants
func getToolCallID(tc any) string {
	v := tcGet(tc, "call_id", nil)
	if !pyTruthy(v) {
		v = tcGet(tc, "id", nil)
	}
	if !pyTruthy(v) {
		return ""
	}
	return pystr.Strip(pyStrOf(v))
}

// Every id variant a result might reference the call by, the sanitizer's own expansion
func (c *ContextCompressor) toolCallIDVariants(tc any) map[string]bool {
	return state.ToolCallIDVariants(tc)
}

// Removes orphaned tool results and strips orphaned tool calls after compression
func (c *ContextCompressor) sanitizeToolPairs(messages []map[string]any) []map[string]any {
	_, resultCallIDs, orphanedResults, missingToolCalls := classifyToolCallOrphans(messages)
	if len(orphanedResults) > 0 {
		orphaned := map[uintptr]bool{}
		for i := range orphanedResults {
			orphaned[rowPointer(orphanedResults[i])] = true
		}
		kept := make([]map[string]any, 0, len(messages))
		for _, m := range messages {
			if !orphaned[rowPointer(m)] {
				kept = append(kept, m)
			}
		}
		messages = kept
		if !c.quietMode {
			slog.Info(fmt.Sprintf("Compression sanitizer: removed %d orphaned tool result(s)", len(orphaned)))
		}
	}
	if len(missingToolCalls) == 0 {
		return messages
	}
	idx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i]["role"] != "tool" {
			idx = i
			break
		}
	}
	var trailing map[string]any
	if idx >= 0 && messages[idx]["role"] == "assistant" {
		trailing = messages[idx]
	}
	stripped := 0
	for _, msg := range messages {
		tcs, _ := msg["tool_calls"].([]any)
		if msg["role"] != "assistant" || (trailing != nil && rowPointer(msg) == rowPointer(trailing)) || len(tcs) == 0 {
			continue
		}
		var kept []any
		for _, tc := range tcs {
			for v := range c.toolCallIDVariants(tc) {
				if resultCallIDs[v] {
					kept = append(kept, tc)
					break
				}
			}
		}
		if len(kept) == len(tcs) {
			continue
		}
		stripped += len(tcs) - len(kept)
		if len(kept) > 0 {
			msg["tool_calls"] = kept
		} else {
			delete(msg, "tool_calls")
			content := msg["content"]
			if s, isStr := content.(string); !pyTruthy(content) || (isStr && pystr.Strip(s) == "") {
				msg["content"] = "(tool call removed)"
			}
		}
	}
	if stripped > 0 && !c.quietMode {
		slog.Info(fmt.Sprintf("Compression sanitizer: stripped %d orphaned tool_call(s) from assistant messages", stripped))
	}
	return messages
}

// Identity of a row map, the Python id of the dict
func rowPointer(m map[string]any) uintptr {
	if m == nil {
		return 0
	}
	return uintptr(reflect.ValueOf(m).UnsafePointer())
}
