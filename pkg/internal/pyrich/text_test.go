package pyrich

import (
	"bytes"
	"strings"
	"testing"
)

func TestTextRenderLinesWrapsAndDropsStylingWithoutColor(t *testing.T) {
	text := &Text{Plain: "\x1b[1mbold words\x1b[0m here\nsecond"}
	colored := text.RenderLines(true, "truecolor", 6)
	if len(colored) < 3 || !strings.Contains(strings.Join(colored, ""), "\x1b[1m") {
		t.Fatalf("colored %q", colored)
	}
	plain := text.RenderLines(false, "truecolor", 80)
	if strings.Join(plain, "|") != "bold words here|second" {
		t.Fatalf("plain %q", plain)
	}
	if text.MeasureMax(42) != 42 {
		t.Fatal("a text measures the whole width")
	}
}

func TestPrintRenderableWritesTheRenderedLines(t *testing.T) {
	var out bytes.Buffer
	c := &Console{Out: &out, Width: 20}
	c.PrintRenderable(&Text{Plain: "hello"})
	if out.String() != "hello\n" {
		t.Fatalf("got %q", out.String())
	}
}
