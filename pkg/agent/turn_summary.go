package agent

import (
	"errors"
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// SummaryPrefix is the terminal chrome leading every turn summary line
const SummaryPrefix = "⋯"

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

// TurnTally is what one turn did as observed from the tool progress feed, verbs and nouns in first seen order
type TurnTally struct {
	verbOrder     []string
	nounOrder     map[string][]string
	counts        map[string]map[string]int
	OtherTools    int
	LinesAdded    int
	LinesRemoved  int
	HasLineDeltas bool
}

// Adds n to the plural noun count under a verb, keeping first seen order for both
func (t *TurnTally) AddVerbCount(verb, plural string, n int) {
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

// Count recorded for a plural noun under a verb
func (t *TurnTally) VerbCount(verb, plural string) int { return t.counts[verb][plural] }

// Verbs in first seen order
func (t *TurnTally) Verbs() []string { return append([]string(nil), t.verbOrder...) }

// Every grouped tool call plus the ungrouped ones
func (t *TurnTally) TotalTools() int {
	total := t.OtherTools
	for _, nouns := range t.counts {
		for _, n := range nouns {
			total += n
		}
	}
	return total
}

// Added and removed line counts from a tool result that already reports a diff, false when unknown
func extractLineDeltas(toolName string, result any) (int, int, bool) {
	if !diffResultTools[toolName] {
		return 0, 0, false
	}
	payload := result
	if s, ok := payload.(string); ok {
		text := pystr.Strip(s)
		if !strings.HasPrefix(text, "{") {
			return 0, 0, false
		}
		parsed, err := pyjson.LoadsLenient(text)
		if err != nil {
			return 0, 0, false
		}
		payload = parsed
	}
	var diffValue any
	switch p := payload.(type) {
	case map[string]any:
		diffValue = p["diff"]
	case *pyjson.Object:
		diffValue, _ = p.Get("diff")
	default:
		return 0, 0, false
	}
	diff, ok := diffValue.(string)
	if !ok || pystr.Strip(diff) == "" {
		return 0, 0, false
	}
	added, removed := 0, 0
	for _, ln := range pystr.Splitlines(diff, false) {
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

// TurnSummaryCollector accumulates per turn tool tallies from the CLI tool progress feed
type TurnSummaryCollector struct{ tally *TurnTally }

// Collector with an empty tally
func NewTurnSummaryCollector() *TurnSummaryCollector {
	return &TurnSummaryCollector{tally: &TurnTally{}}
}

// Starts a fresh turn dropping any prior tally
func (c *TurnSummaryCollector) Begin() { c.tally = &TurnTally{} }

// Records one completed tool call, skipping failures and internal pseudo tools
func (c *TurnSummaryCollector) RecordTool(toolName string, result any, isError bool) {
	if toolName == "" || isError || strings.HasPrefix(toolName, "_") {
		return
	}
	group, ok := verbGroups[toolName]
	if !ok {
		c.tally.OtherTools++
		return
	}
	c.tally.AddVerbCount(group.verb, group.plural, 1)
	if group.verb == editVerb {
		if added, removed, ok := extractLineDeltas(toolName, result); ok {
			c.tally.LinesAdded += added
			c.tally.LinesRemoved += removed
			c.tally.HasLineDeltas = true
		}
	}
}

// Current turn tally
func (c *TurnSummaryCollector) Tally() *TurnTally { return c.tally }

// Summary line for the current tally after the elapsed seconds
func (c *TurnSummaryCollector) Render(elapsedSeconds float64) string {
	return FormatTurnSummary(elapsedSeconds, c.tally, maxSummarySegments)
}

// Elapsed time as 12.4s under a minute and 2m05s beyond
func FormatElapsed(seconds float64) string {
	seconds = math.Max(seconds, 0.0)
	if seconds < 60 {
		text, _ := pyrepr.Format(seconds, ".1f")
		return text + "s"
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
func pluralize(count int, pluralNoun string) string {
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
func FormatTurnSummary(elapsedSeconds float64, tally *TurnTally, maxSegments int) string {
	if tally == nil {
		tally = &TurnTally{}
	}
	var ordered []string
	for _, v := range verbPriority {
		if _, ok := tally.counts[v]; ok {
			ordered = append(ordered, v)
		}
	}
	for _, v := range tally.verbOrder {
		if !contains(verbPriority, v) {
			ordered = append(ordered, v)
		}
	}
	var segments []string
	for _, verb := range ordered {
		var parts []string
		for _, plural := range tally.nounOrder[verb] {
			if count := tally.counts[verb][plural]; count != 0 {
				parts = append(parts, pluralize(count, plural))
			}
		}
		if len(parts) == 0 {
			continue
		}
		segment := verb + " " + strings.Join(parts, ", ")
		if verb == editVerb && tally.HasLineDeltas {
			segment += " +" + strconv.Itoa(tally.LinesAdded) + " -" + strconv.Itoa(tally.LinesRemoved)
		}
		segments = append(segments, segment)
	}
	if tally.OtherTools != 0 {
		segments = append(segments, "called "+pluralize(tally.OtherTools, "tools"))
	}
	if len(segments) == 0 && tally.TotalTools() == 0 && elapsedSeconds < minToollessSeconds {
		return ""
	}
	if maxSegments > 0 && len(segments) > maxSegments {
		hidden := len(segments) - maxSegments
		segments = append(segments[:maxSegments:maxSegments], "+"+strconv.Itoa(hidden)+" more")
	}
	return SummaryPrefix + " " + strings.Join(append([]string{FormatElapsed(elapsedSeconds)}, segments...), " · ")
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// Cumulative turn tokens for the live spinner, empty for a non positive or unreadable count, overflow raised
func FormatTokenFlow(outputTokens any, arrow string) (string, error) {
	count, err := pyint.Of(outputTokens)
	var pe *pyint.Error
	if errors.As(err, &pe) && pe.Kind == "OverflowError" {
		return "", err
	}
	if err != nil || count.Sign() <= 0 {
		return "", nil
	}
	scaled := func(div int64, unit string) (string, error) {
		f, _ := new(big.Rat).SetFrac(count, big.NewInt(div)).Float64()
		text, _ := pyrepr.Format(f, ".1f")
		return arrow + " " + text + unit + " tok", nil
	}
	switch {
	case count.Cmp(big.NewInt(1000)) < 0:
		return arrow + " " + count.String() + " tok", nil
	case count.Cmp(big.NewInt(1_000_000)) < 0:
		return scaled(1000, "k")
	}
	return scaled(1_000_000, "M")
}
