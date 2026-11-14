package agent

import (
	"math"
	"strings"
	"testing"
)

func tallyOf(verbs ...[3]any) *TurnTally {
	t := &TurnTally{}
	for _, v := range verbs {
		t.AddVerbCount(v[0].(string), v[1].(string), v[2].(int))
	}
	return t
}

func TestFormatElapsed(t *testing.T) {
	cases := map[float64]string{0.0: "0.0s", 12.44: "12.4s", 59.9: "59.9s", 60.0: "1m00s", 125.0: "2m05s", -3.0: "0.0s", 150.5: "2m30s", 151.5: "2m32s"}
	for seconds, want := range cases {
		if got := FormatElapsed(seconds); got != want {
			t.Fatalf("%v: got %q want %q", seconds, got, want)
		}
	}
}

func TestPluralizationSingularAndPlural(t *testing.T) {
	if got := FormatTurnSummary(1.0, tallyOf([3]any{"read", "files", 1}), maxSummarySegments); got != "⋯ 1.0s · read 1 file" {
		t.Fatalf("one: %q", got)
	}
	if got := FormatTurnSummary(1.0, tallyOf([3]any{"read", "files", 3}), maxSummarySegments); got != "⋯ 1.0s · read 3 files" {
		t.Fatalf("many: %q", got)
	}
}

func TestPluralizationIrregularNouns(t *testing.T) {
	if got := FormatTurnSummary(1.0, tallyOf([3]any{"updated", "memories", 1}), maxSummarySegments); got != "⋯ 1.0s · updated 1 memory" {
		t.Fatalf("memories: %q", got)
	}
	if got := FormatTurnSummary(1.0, tallyOf([3]any{"searched the web", "times", 1}), maxSummarySegments); got != "⋯ 1.0s · searched the web 1 time" {
		t.Fatalf("times: %q", got)
	}
	if got := pluralize(1, "processes"); got != "1 process" {
		t.Fatalf("ses: %q", got)
	}
}

func TestLongTalliesTruncateToMoreTail(t *testing.T) {
	tally := tallyOf([3]any{"edited", "files", 1}, [3]any{"read", "files", 2}, [3]any{"ran", "commands", 3},
		[3]any{"searched", "paths", 4}, [3]any{"browsed", "pages", 5}, [3]any{"delegated", "tasks", 6})
	line := FormatTurnSummary(9.0, tally, maxSummarySegments)
	if !strings.HasSuffix(line, "· +2 more") || strings.Count(line, "·") != 5 {
		t.Fatalf("line: %q", line)
	}
	ordered := FormatTurnSummary(9.0, tallyOf([3]any{"browsed", "pages", 1}, [3]any{"ran", "commands", 2}, [3]any{"edited", "files", 1}), 0)
	if ordered != "⋯ 9.0s · edited 1 file · ran 2 commands · browsed 1 page" {
		t.Fatalf("priority order: %q", ordered)
	}
}

func TestBeginResetsPreviousTurn(t *testing.T) {
	collector := NewTurnSummaryCollector()
	collector.Begin()
	collector.RecordTool("read_file", "x", false)
	collector.Begin()
	if collector.Tally().TotalTools() != 0 || collector.Render(0.5) != "" {
		t.Fatal("begin did not reset the tally")
	}
	collector.RecordTool("_thinking", nil, false)
	collector.RecordTool("write_file", nil, true)
	collector.RecordTool("mcp_custom", nil, false)
	collector.RecordTool("", nil, false)
	if got := collector.Render(0.5); got != "⋯ 0.5s · called 1 tool" {
		t.Fatalf("skips: %q", got)
	}
	if got := NewTurnSummaryCollector().Render(2.5); got != "⋯ 2.5s" {
		t.Fatalf("slow tool less turn: %q", got)
	}
}

func TestFormatTokenFlowBadInputIsEmpty(t *testing.T) {
	for _, in := range []any{nil, "lots", 0, -5} {
		if got, err := FormatTokenFlow(in, "↓"); got != "" || err != nil {
			t.Fatalf("%v: %q %v", in, got, err)
		}
	}
	cases := map[any]string{999: "↓ 999 tok", 1200: "↓ 1.2k tok", "2500000": "↓ 2.5M tok", 12.9: "↓ 12 tok"}
	for in, want := range cases {
		if got, _ := FormatTokenFlow(in, "↓"); got != want {
			t.Fatalf("%v: got %q want %q", in, got, want)
		}
	}
	if _, err := FormatTokenFlow(math.Inf(1), "↓"); err == nil {
		t.Fatal("infinity raises OverflowError")
	}
}

func TestContentFreeDiffReportsUnknownNotZeroZero(t *testing.T) {
	c := NewTurnSummaryCollector()
	c.Begin()
	c.RecordTool("patch", map[string]any{"success": true, "diff": "@@ -1,3 +1,15 @@"}, false)
	line := c.Render(3.0)
	if !strings.Contains(line, "edited 1 file") || strings.Contains(line, "+0 -0") {
		t.Fatalf("content free: %q", line)
	}
	real := NewTurnSummaryCollector()
	real.Begin()
	real.RecordTool("patch", map[string]any{"success": true, "diff": "--- a/x\n+++ b/x\n@@ -1 +1,2 @@\n-old\n+new\n+extra\n"}, false)
	if !strings.Contains(real.Render(1.0), "+2 -1") {
		t.Fatalf("real diff: %q", real.Render(1.0))
	}
	str := NewTurnSummaryCollector()
	str.RecordTool("patch", "{\"diff\": \"+a\tb\n-c\"}", false)
	if !strings.Contains(str.Render(1.0), "edited 1 file +1 -1") {
		t.Fatalf("json text with a raw control character: %q", str.Render(1.0))
	}
}
