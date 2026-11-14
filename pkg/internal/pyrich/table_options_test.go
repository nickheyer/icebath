package pyrich

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestTableWithoutBoxPadsCellsAndSkipsRules(t *testing.T) {
	tbl := &Table{HideHeader: true, NoBox: true, PadX: 2, Columns: []Column{{Style: "bold"}, {}}}
	tbl.AddRow("Enabled", "yes")
	tbl.AddRow("Token env var", "BWS")
	want := "  Enabled          yes  \n  Token env var    BWS  "
	if got := tbl.RenderWidth(false, 80); got != want {
		t.Fatalf("%q", got)
	}
	headed := &Table{NoBox: true, PadX: 2, Columns: []Column{{Header: "#", Style: "cyan", Width: 4}, {Header: "Region"}}}
	headed.AddRow("1", "US")
	want = "  #       Region  \n  1       US      "
	if got := headed.RenderWidth(false, 80); got != want {
		t.Fatalf("%q", got)
	}
	panel := Panel{Title: "T", Content: tbl}.Render(false, 40)
	if !strings.HasPrefix(panel, "╭") || !strings.Contains(panel, "│   Enabled          yes  ") {
		t.Fatalf("%q", panel)
	}
	if tbl.MeasureMax(80) != 24 || headed.MeasureMax(80) != 18 {
		t.Fatalf("measure %d %d", tbl.MeasureMax(80), headed.MeasureMax(80))
	}
}

func TestConsoleInputReadsOneLineAfterThePrompt(t *testing.T) {
	var b bytes.Buffer
	c := &Console{Out: &b, In: strings.NewReader("2\r\nab")}
	got, err := c.Input("Pick [1-3]: ")
	if err != nil || got != "2" || b.String() != "Pick [1-3]: " {
		t.Fatalf("%q %v %q", got, err, b.String())
	}
	if got, err = c.Input(""); err != nil || got != "ab" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err = c.Input(""); err != io.EOF {
		t.Fatalf("%v", err)
	}
}
