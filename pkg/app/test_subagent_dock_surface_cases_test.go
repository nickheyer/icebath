package app

import (
	"strconv"
	"strings"
	"testing"
)

func TestExpandedRosterReservesActivityBeforeLongTaskNames(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := bareCLI()
	monitor := newSubagentMonitor(c)
	for i := 0; i < 6; i++ {
		monitor.entries = append(monitor.entries, map[string]any{"subagent_id": strconv.Itoa(i), "goal": strings.Repeat("Inspect 界 ", 30),
			"elapsed": 24, "status": "running", "last_tool": "terminal"})
	}
	monitor.selectedID = "0"
	lastTool := c.t("cli.subagents.last_tool", map[string]any{"tool": "terminal"})
	for _, size := range [][2]int{{100, 30}, {80, 20}} {
		columns := size[0]
		app := buildMonitorApplication(monitor, strings.NewReader(""), &strings.Builder{})
		app.cols, app.rows = size[0], size[1]
		fragments, _ := app.rosterText(columns)
		if len(fragments) != len(monitor.entries) {
			t.Fatalf("%d fragments for %d entries", len(fragments), len(monitor.entries))
		}
		for _, f := range fragments {
			if !strings.Contains(f.Text, "24s") || !strings.Contains(f.Text, "running") || !strings.Contains(f.Text, lastTool) {
				t.Fatalf("row %q", f.Text)
			}
			if got := cellWidth(strings.TrimRight(f.Text, "\n")); got != columns {
				t.Fatalf("row %q is %d cells, want %d", f.Text, got, columns)
			}
		}
	}
}
