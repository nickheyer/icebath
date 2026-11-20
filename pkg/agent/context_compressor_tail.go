package agent

import (
	"strings"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	leanTailKeepToolRounds   = 6
	leanTailDemoteMinChars   = 1_500
	tailMaxThresholdFraction = 0.50
	skillSafetyHeading       = "## Skill Safety Rule"
	skillSafetyRule          = skillSafetyHeading + "\n" +
		"A skill placeholder containing `[SKILL_PRUNED]` lost its content in context compression and is inaccessible — " +
		"reload it with skill_view(name='...') before acting on anything that depends on it. After reloading, ignore any " +
		"remaining `[SKILL_PRUNED]` markers for that same skill; they are historical artifacts of earlier compactions."
	skillSafetySuffix = "\n\n" + skillSafetyRule
)

// Matches what leanRecoveryLine emits so later passes treat a demoted row as already summarised
var demotedRowRE = mustPyRE(`\[[^\s\]]* output demoted at compaction — [\d,]+ chars preserved in session history\.`)

func mustPyRE(pattern string) *regexp2.Regexp {
	re, err := pyre.Compile(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

// TailCutCompressor is the compressor surface the tail cut price and progress floor walk over
type TailCutCompressor interface {
	TailRowPricer(messages []map[string]any) func(int) int
	IsContextSummaryMessage(message map[string]any) bool
	AlignBoundaryForward(messages []map[string]any, idx int) int
	FindLastUserMessageIdx(messages []map[string]any, headEnd int) int
}

// Lean tail budget of 2.5 percent of the window inside the floor and cap, held to half the trigger
func LeanTailBudget(contextLength, thresholdTokens int) int {
	floor, windowTerm := leanTailFloorTokens, min(leanTailCapTokens, int(float64(contextLength)*0.025))
	if thresholdTokens > 0 {
		share := int(float64(thresholdTokens) * tailMaxThresholdFraction)
		floor = min(floor, share)
		if thresholdTokens > minSummaryTokens {
			windowTerm = min(windowTerm, share)
		}
	}
	return max(floor, windowTerm)
}

// True for a tail tool result already replaced by a lean recovery line
func IsDemotedRow(content string) bool { return isDemotedRow(content) }

func isDemotedRow(content string) bool {
	if utf8.RuneCountInString(content) >= 1_000 {
		return false
	}
	m, err := demotedRowRE.FindStringMatch(content)
	return err == nil && m != nil && m.RuneIndex == 0
}

// One line replacement for a demoted tail tool result with a session_search recovery hint
func leanRecoveryLine(toolName string, contentLen int, sessionID string) string {
	hint := ""
	if sessionID != "" {
		hint = " Recover with session_search(query=..., session_id='" + sessionID + "')"
	}
	if toolName == "" {
		toolName = "tool"
	}
	count, _ := pyrepr.Format(contentLen, ",")
	return "[" + toolName + " output demoted at compaction — " + count + " chars preserved in session history." + hint + "]"
}

// Each skill_view tool call id mapped to the skill name it loaded
func skillViewNamesByCallID(messages []map[string]any) map[string]string {
	names := map[string]string{}
	for _, msg := range messages {
		if msg["role"] != "assistant" {
			continue
		}
		for _, tc := range pyIterRows(msg["tool_calls"]) {
			fn := tcGet(tc, "function", map[string]any{})
			if tcGet(fn, "name", "") != "skill_view" {
				continue
			}
			skill, _ := jsonDict(tcGet(fn, "arguments", "")).Get("name")
			if s, ok := skill.(string); ok && s != "" {
				id := tcGet(tc, "id", "")
				key := ""
				if pyrepr.Truthy(id) {
					key = pyrepr.Str(id)
				}
				names[key] = s
			}
		}
	}
	return names
}

// Demotes tail tool results older than the newest six rounds to recovery lines and counts them
func DemoteStaleTailTools(messages []map[string]any, tailStart int, sessionID string) ([]map[string]any, int) {
	roundsSeen, prevIdx := 0, -1
	protected := map[int]bool{}
	for i := len(messages) - 1; i >= tailStart; i-- {
		if messages[i]["role"] != "tool" {
			continue
		}
		if prevIdx < 0 || prevIdx-i > 1 {
			roundsSeen++
		}
		prevIdx = i
		if roundsSeen > leanTailKeepToolRounds {
			break
		}
		protected[i] = true
	}
	result := append([]map[string]any(nil), messages...)
	demoted := 0
	var skills map[string]string
	for i := max(tailStart, 0); i < len(messages); i++ {
		msg := messages[i]
		content, isStr := msg["content"].(string)
		if msg["role"] != "tool" || protected[i] || !isStr {
			continue
		}
		n := utf8.RuneCountInString(content)
		if n < leanTailDemoteMinChars || strings.Contains(content, skillPrunedMarkerPrefix) || isSummaryLine(content) {
			continue
		}
		toolName, _ := msg["tool_name"].(string)
		line := leanRecoveryLine(toolName, n, sessionID)
		if skills == nil {
			skills = skillViewNamesByCallID(messages)
		}
		callID := ""
		if pyrepr.Truthy(msg["tool_call_id"]) {
			callID = pyrepr.Str(msg["tool_call_id"])
		}
		if skill := skills[callID]; skill != "" && resultFailureSuffix(content) == "" {
			line += " " + skillPrunedMarker(skill)
		}
		result[i] = rewritten(msg, line)
		demoted++
	}
	return result, demoted
}

// Index of the newest in transcript handoff at or after headEnd, else -1
func newestHandoffIndex(c TailCutCompressor, messages []map[string]any, headEnd int) int {
	for i := len(messages) - 1; i >= headEnd; i-- {
		if c.IsContextSummaryMessage(messages[i]) {
			return i
		}
	}
	return -1
}

// Tail cut walk price in which rows at or before the newest handoff cost nothing
func TailCutPricer(c TailCutCompressor, messages []map[string]any, headEnd int) func(int) int {
	price := c.TailRowPricer(messages)
	newest := newestHandoffIndex(c, messages, headEnd)
	if newest < 0 {
		return price
	}
	return func(i int) int {
		if i <= newest {
			return 0
		}
		return price(i)
	}
}

// Lowest tail cut that leaves the summary window a row the handoff scan keeps
func TailCutProgressFloor(c TailCutCompressor, messages []map[string]any, headEnd, cutIdx int) int {
	floor := headEnd + 1
	newest := newestHandoffIndex(c, messages, headEnd)
	if newest < 0 {
		return floor
	}
	for _, m := range pySlice(messages, headEnd, max(cutIdx, newest+1)) {
		if !c.IsContextSummaryMessage(m) {
			return floor
		}
	}
	raised := c.AlignBoundaryForward(messages, newest+2)
	lastUser := c.FindLastUserMessageIdx(messages, newest+1)
	if raised >= len(messages) || 0 <= lastUser && lastUser < raised {
		return floor
	}
	return raised
}

// Python slice rows[lo:hi] with negative and out of range bounds clamped
func pySlice(rows []map[string]any, lo, hi int) []map[string]any {
	n := len(rows)
	clamp := func(i int) int {
		if i < 0 {
			i += n
		}
		return min(max(i, 0), n)
	}
	lo, hi = clamp(lo), clamp(hi)
	if lo >= hi {
		return nil
	}
	return rows[lo:hi]
}

// Rows with an attached safety rule removed from every non system string content
func unruledRows(rows []map[string]any) []map[string]any {
	out := make([]map[string]any, len(rows))
	for i, m := range rows {
		content, ok := m["content"].(string)
		if m != nil && m["role"] != "system" && ok && strings.Contains(content, skillSafetyRule) {
			cp := make(map[string]any, len(m))
			for k, v := range m {
				cp[k] = v
			}
			cp["content"] = withoutSkillSafetyRule(content)
			out[i] = cp
			continue
		}
		out[i] = m
	}
	return out
}

// Summary, head and tail carrying the skill safety rule exactly once when any of them holds a prune marker
func WithSkillSafetyRule(summary string, head, tail []map[string]any) (string, []map[string]any, []map[string]any) {
	head, tail = unruledRows(head), unruledRows(tail)
	texts := []string{summary}
	for _, rows := range [][]map[string]any{head, tail} {
		for _, m := range rows {
			if m != nil {
				texts = append(texts, contentTextForContains(m["content"]))
			}
		}
	}
	ruled, marked := false, false
	for _, t := range texts {
		ruled = ruled || strings.Contains(t, skillSafetyRule)
		marked = marked || strings.Contains(t, skillPrunedMarkerPrefix)
	}
	if ruled || !marked {
		return summary, head, tail
	}
	return summary + skillSafetySuffix, head, tail
}

// Content minus an attached skill safety rule
func withoutSkillSafetyRule(content string) string {
	return strings.ReplaceAll(content, skillSafetySuffix, "")
}

// Elides content to limit code points while keeping an attached skill safety rule whole
func ElideKeepingSkillSafetyRule(content string, limit int) string {
	if !strings.Contains(content, skillSafetyRule) {
		return tools.Elide(content, limit)
	}
	return tools.Elide(withoutSkillSafetyRule(content), limit-utf8.RuneCountInString(skillSafetySuffix)) + skillSafetySuffix
}

// Pruned rows with the rule on the first row turned into a prune marker when no row carries it yet
func WithPrunedRowSkillSafetyRule(original, pruned []map[string]any) []map[string]any {
	for _, m := range pruned {
		if m != nil && strings.Contains(contentTextForContains(m["content"]), skillSafetyRule) {
			return pruned
		}
	}
	for i := 0; i < min(len(original), len(pruned)); i++ {
		after := pruned[i]
		if after == nil {
			continue
		}
		content, ok := after["content"].(string)
		before, beforeIsStr := original[i]["content"].(string)
		if ok && strings.Contains(content, skillPrunedMarkerPrefix) && (!beforeIsStr || content != before) {
			cp := make(map[string]any, len(after))
			for k, v := range after {
				cp[k] = v
			}
			cp["content"] = content + skillSafetySuffix
			out := append([]map[string]any(nil), pruned[:i]...)
			out = append(out, cp)
			return append(out, pruned[i+1:]...)
		}
	}
	return pruned
}
