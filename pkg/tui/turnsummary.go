package tui

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// turnSummaryPrefix is the terminal chrome leading every turn summary line
const turnSummaryPrefix = "⋯"

const (
	minToollessSeconds = 2.0
	maxSummarySegments = 4
	editVerb           = "edited"
)

type verbGroup struct{ verb, singular, plural string }

var verbGroups = map[string]verbGroup{
	"write_file":       {"edited", "file", "files"},
	"patch":            {"edited", "file", "files"},
	"read_file":        {"read", "file", "files"},
	"web_extract":      {"read", "page", "pages"},
	"terminal":         {"ran", "command", "commands"},
	"execute_code":     {"ran", "script", "scripts"},
	"search_files":     {"searched", "path", "paths"},
	"web_search":       {"searched the web", "time", "times"},
	"session_search":   {"searched sessions", "time", "times"},
	"browser_navigate": {"browsed", "page", "pages"},
	"skill_view":       {"read", "skill", "skills"},
	"skill_manage":     {"updated", "skill", "skills"},
	"skills_list":      {"listed skills", "time", "times"},
	"todo_list":        {"updated", "task list", "task lists"},
	"delegate_task":    {"delegated", "task", "tasks"},
	"memory":           {"updated", "memory", "memories"},
}

var verbPriority = []string{"edited", "read", "ran"}

var diffResultTools = map[string]bool{"patch": true}

// turnTally is what one turn did as observed from the tool progress feed, verbs and nouns in first seen order
type turnTally struct {
	verbOrder     []string
	nounOrder     map[string][]string
	counts        map[string]map[string]int
	otherTools    int
	linesAdded    int
	linesRemoved  int
	hasLineDeltas bool
}

// Adds n to the plural noun count under a verb, keeping first seen order for both
func (t *turnTally) addVerbCount(verb, plural string, n int) {
	if t.counts == nil {
		t.counts, t.nounOrder = map[string]map[string]int{}, map[string][]string{}
	}
	nouns, ok := t.counts[verb]
	if !ok {
		nouns = map[string]int{}
		t.counts[verb] = nouns
		t.verbOrder = append(t.verbOrder, verb)
	}
	if _, seen := nouns[plural]; !seen {
		t.nounOrder[verb] = append(t.nounOrder[verb], plural)
	}
	nouns[plural] += n
}

// Every grouped tool call plus the ungrouped ones
func (t *turnTally) totalTools() int {
	total := t.otherTools
	for _, nouns := range t.counts {
		for _, n := range nouns {
			total += n
		}
	}
	return total
}

// JSON object of a text, raw control characters inside its strings accepted the way a tool writes them, false for anything else
func lenientJSONObject(text string) (map[string]any, bool) {
	var fixed strings.Builder
	inString := false
	for i := 0; i < len(text); i++ {
		ch := text[i]
		switch {
		case inString && ch == '\\' && i+1 < len(text):
			fixed.WriteByte(ch)
			i++
			fixed.WriteByte(text[i])
		case ch == '"':
			inString = !inString
			fixed.WriteByte(ch)
		case inString && ch < 0x20:
			fixed.WriteString("\\u00")
			fixed.WriteString(strconv.FormatInt(int64(ch)>>4, 16))
			fixed.WriteString(strconv.FormatInt(int64(ch)&0xf, 16))
		default:
			fixed.WriteByte(ch)
		}
	}
	var object map[string]any
	if json.Unmarshal([]byte(fixed.String()), &object) != nil || object == nil {
		return nil, false
	}
	return object, true
}

// Added and removed line counts from a tool result that already reports a diff, false when unknown
func extractLineDeltas(toolName string, result any) (int, int, bool) {
	if !diffResultTools[toolName] {
		return 0, 0, false
	}
	payload, isObject := result.(map[string]any)
	if text, isText := result.(string); isText {
		if payload, isObject = lenientJSONObject(strings.TrimFunc(text, isJSSpace)); !isObject {
			return 0, 0, false
		}
	}
	diff, isText := payload["diff"].(string)
	if !isObject || !isText || strings.TrimFunc(diff, isJSSpace) == "" {
		return 0, 0, false
	}
	added, removed := 0, 0
	for _, ln := range strings.FieldsFunc(diff, func(r rune) bool { return r == '\n' || r == '\r' }) {
		if strings.HasPrefix(ln, "+++") || strings.HasPrefix(ln, "---") {
			continue
		}
		if strings.HasPrefix(ln, "+") {
			added++
		}
		if strings.HasPrefix(ln, "-") {
			removed++
		}
	}
	if added == 0 && removed == 0 {
		return 0, 0, false
	}
	return added, removed, true
}

// turnSummaryCollector accumulates per turn tool tallies from the CLI tool progress feed
type turnSummaryCollector struct{ tally *turnTally }

// Collector with an empty tally
func newTurnSummaryCollector() *turnSummaryCollector {
	return &turnSummaryCollector{tally: &turnTally{}}
}

// Starts a fresh turn dropping any prior tally
func (c *turnSummaryCollector) begin() { c.tally = &turnTally{} }

// Records one completed tool call, skipping failures and internal pseudo tools
func (c *turnSummaryCollector) recordTool(toolName string, result any, isError bool) {
	if toolName == "" || isError || strings.HasPrefix(toolName, "_") {
		return
	}
	group, ok := verbGroups[toolName]
	if !ok {
		c.tally.otherTools++
		return
	}
	c.tally.addVerbCount(group.verb, group.plural, 1)
	if group.verb == editVerb {
		if added, removed, ok := extractLineDeltas(toolName, result); ok {
			c.tally.linesAdded += added
			c.tally.linesRemoved += removed
			c.tally.hasLineDeltas = true
		}
	}
}

// Summary line for the current tally after the elapsed seconds
func (c *turnSummaryCollector) render(elapsedSeconds float64) string {
	return formatTurnSummary(elapsedSeconds, c.tally, maxSummarySegments)
}

// Elapsed time as 12.4s under a minute and 2m05s beyond
func formatTurnElapsed(seconds float64) string {
	seconds = math.Max(seconds, 0.0)
	if seconds < 60 {
		return strconv.FormatFloat(seconds, 'f', 1, 64) + "s"
	}
	total := int64(math.RoundToEven(seconds))
	minutes, rest := total/60, total%60
	pad := ""
	if rest < 10 {
		pad = "0"
	}
	return strconv.FormatInt(minutes, 10) + "m" + pad + strconv.FormatInt(rest, 10) + "s"
}

// Count with the singular noun form for one and the plural otherwise
func pluralizeNoun(count int, pluralNoun string) string {
	if count != 1 {
		return strconv.Itoa(count) + " " + pluralNoun
	}
	for _, rule := range [][2]string{{"ies", "y"}, {"ses", "s"}, {"s", ""}} {
		if strings.HasSuffix(pluralNoun, rule[0]) {
			return "1 " + pluralNoun[:len(pluralNoun)-len(rule[0])] + rule[1]
		}
	}
	return "1 " + pluralNoun
}

// Per turn accounting line, empty for a fast tool less reply
func formatTurnSummary(elapsedSeconds float64, tally *turnTally, maxSegments int) string {
	if tally == nil {
		tally = &turnTally{}
	}
	var ordered []string
	for _, v := range verbPriority {
		if _, ok := tally.counts[v]; ok {
			ordered = append(ordered, v)
		}
	}
	for _, v := range tally.verbOrder {
		if !verbListed(verbPriority, v) {
			ordered = append(ordered, v)
		}
	}
	var segments []string
	for _, verb := range ordered {
		var parts []string
		for _, plural := range tally.nounOrder[verb] {
			if count := tally.counts[verb][plural]; count != 0 {
				parts = append(parts, pluralizeNoun(count, plural))
			}
		}
		if len(parts) == 0 {
			continue
		}
		segment := verb + " " + strings.Join(parts, ", ")
		if verb == editVerb && tally.hasLineDeltas {
			segment += " +" + strconv.Itoa(tally.linesAdded) + " -" + strconv.Itoa(tally.linesRemoved)
		}
		segments = append(segments, segment)
	}
	if tally.otherTools != 0 {
		segments = append(segments, "called "+pluralizeNoun(tally.otherTools, "tools"))
	}
	if len(segments) == 0 && tally.totalTools() == 0 && elapsedSeconds < minToollessSeconds {
		return ""
	}
	if maxSegments > 0 && len(segments) > maxSegments {
		hidden := len(segments) - maxSegments
		segments = append(segments[:maxSegments:maxSegments], "+"+strconv.Itoa(hidden)+" more")
	}
	return turnSummaryPrefix + " " + strings.Join(append([]string{formatTurnElapsed(elapsedSeconds)}, segments...), " · ")
}

func verbListed(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// Tokens of the running turn for the status line, empty until there are some
func formatTokenFlow(tokens int, arrow string) string {
	switch {
	case tokens <= 0:
		return ""
	case tokens < 1000:
		return arrow + " " + strconv.Itoa(tokens) + " tok"
	case tokens < 1_000_000:
		return arrow + " " + strconv.FormatFloat(float64(tokens)/1000, 'f', 1, 64) + "k tok"
	}
	return arrow + " " + strconv.FormatFloat(float64(tokens)/1_000_000, 'f', 1, 64) + "M tok"
}
