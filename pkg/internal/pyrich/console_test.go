package pyrich

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintWrapsLikeRichDivideLine(t *testing.T) {
	var b bytes.Buffer
	c := &Console{Out: &b, Width: 20}
	c.Print("hello world this is a test")
	c.Print("[bold]abcdefghij[/bold]")
	c.Print()
	want := "hello world this is \na test\nabcdefghij\n\n"
	if b.String() != want {
		t.Fatalf("%q", b.String())
	}
	if got := WrapStyled("abcdefghij", 4, true); strings.Join(got, "|") != "abcd|efgh|ij" {
		t.Fatalf("%q", got)
	}
	colored := WrapStyled(Render("[red]aaa bbb[/red]", true), 3, true)
	if len(colored) != 2 || colored[1] != "\x1b[31mbbb\x1b[0m" {
		t.Fatalf("%q", colored)
	}
}

func TestPanelCentersTitleInRoundedBorder(t *testing.T) {
	got := Panel{Body: "hi", Title: "T", Subtitle: "s"}.Render(false, 20)
	want := "╭─────── T ────────╮\n│ hi               │\n╰─────── s ────────╯"
	if got != want {
		t.Fatalf("\n%s\n%s", got, want)
	}
}

func TestTableFitsWidthAndFoldsIdentifier(t *testing.T) {
	tb := &Table{Columns: []Column{{Header: "Name", Style: "dim"}, {Header: "Identifier", Overflow: "fold"}}}
	tb.AddRow("x", "abcdefghijklmnopqrstuvwxyz0123456789")
	lines := strings.Split(tb.RenderWidth(false, 20), "\n")
	var cells strings.Builder
	for _, l := range lines {
		if CellLen(l) != 20 {
			t.Fatalf("%q is %d cells", l, CellLen(l))
		}
		if strings.HasPrefix(l, "│") {
			cells.WriteString(strings.TrimSpace(strings.Split(l, "│")[2]))
		}
	}
	if cells.String() != "abcdefghijklmnopqrstuvwxyz0123456789" {
		t.Fatalf("%q", cells.String())
	}
	if w := tb.columnWidths(17); w[0] != 6 || w[1] != 11 {
		t.Fatalf("%v", w)
	}
}
