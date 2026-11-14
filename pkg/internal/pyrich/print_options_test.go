package pyrich

import (
	"bytes"
	"testing"
)

func truecolorConsole(width int) (*Console, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return &Console{Out: buf, Color: true, Terminal: true, NoHighlight: true, Width: width, ColorSystem: "truecolor"}, buf
}

func TestPanelHorizontalsBoxStylePaddingAndWidthMatchRich(t *testing.T) {
	c, buf := truecolorConsole(40)
	c.PrintPanel(Panel{Content: &Text{Plain: "Hello world, this is the answer text that wraps."}, Title: "[#CD7F32 bold]☤ Icebath[/]", TitleAlign: "left",
		BorderStyle: "#CD7F32", Style: "#FFF8DC", Box: "horizontals", Padding: &[2]int{1, 0}, Width: 36})
	want := "\x1b[38;2;205;127;50m ─\x1b[0m\x1b[38;2;205;127;50m \x1b[0m\x1b[1;38;2;205;127;50m☤ Icebath\x1b[0m\x1b[38;2;205;127;50m \x1b[0m\x1b[38;2;205;127;50m─────────────────────\x1b[0m\x1b[38;2;205;127;50m─ \x1b[0m\n" +
		"\x1b[38;2;205;127;50m \x1b[0m\x1b[38;2;255;248;220m                                  \x1b[0m\x1b[38;2;205;127;50m \x1b[0m\n" +
		"\x1b[38;2;205;127;50m \x1b[0m\x1b[38;2;255;248;220mHello world, this is the answer \x1b[0m\x1b[38;2;255;248;220m  \x1b[0m\x1b[38;2;205;127;50m \x1b[0m\n" +
		"\x1b[38;2;205;127;50m \x1b[0m\x1b[38;2;255;248;220mtext that wraps.\x1b[0m\x1b[38;2;255;248;220m                  \x1b[0m\x1b[38;2;205;127;50m \x1b[0m\n" +
		"\x1b[38;2;205;127;50m \x1b[0m\x1b[38;2;255;248;220m                                  \x1b[0m\x1b[38;2;205;127;50m \x1b[0m\n" +
		"\x1b[38;2;205;127;50m ────────────────────────────────── \x1b[0m\n"
	if got := buf.String(); got != want {
		t.Fatalf("got\n%q\nwant\n%q", got, want)
	}
}

func TestPanelStyleLiesUnderMarkdownContent(t *testing.T) {
	c, buf := truecolorConsole(40)
	c.PrintPanel(Panel{Content: &Markdown{Source: "Some **bold** text\n\n- item one\n- item two"}, Title: "[#CD7F32 bold]☤ Icebath[/]", TitleAlign: "left",
		BorderStyle: "#CD7F32", Style: "#FFF8DC", Box: "horizontals", Padding: &[2]int{1, 0}, Width: 40})
	want := "\x1b[38;2;205;127;50m ─\x1b[0m\x1b[38;2;205;127;50m \x1b[0m\x1b[1;38;2;205;127;50m☤ Icebath\x1b[0m\x1b[38;2;205;127;50m \x1b[0m\x1b[38;2;205;127;50m─────────────────────────\x1b[0m\x1b[38;2;205;127;50m─ \x1b[0m\n" +
		"\x1b[38;2;205;127;50m \x1b[0m\x1b[38;2;255;248;220m                                      \x1b[0m\x1b[38;2;205;127;50m \x1b[0m\n" +
		"\x1b[38;2;205;127;50m \x1b[0m\x1b[38;2;255;248;220mSome \x1b[0m\x1b[1;38;2;255;248;220mbold\x1b[0m\x1b[38;2;255;248;220m text                        \x1b[0m\x1b[38;2;205;127;50m \x1b[0m\n" +
		"\x1b[38;2;205;127;50m \x1b[0m\x1b[38;2;255;248;220m                                      \x1b[0m\x1b[38;2;205;127;50m \x1b[0m\n" +
		"\x1b[38;2;205;127;50m \x1b[0m\x1b[1;38;2;255;248;220m • \x1b[0m\x1b[38;2;255;248;220mitem one                           \x1b[0m\x1b[38;2;205;127;50m \x1b[0m\n" +
		"\x1b[38;2;205;127;50m \x1b[0m\x1b[1;38;2;255;248;220m • \x1b[0m\x1b[38;2;255;248;220mitem two                           \x1b[0m\x1b[38;2;205;127;50m \x1b[0m\n" +
		"\x1b[38;2;205;127;50m \x1b[0m\x1b[38;2;255;248;220m                                      \x1b[0m\x1b[38;2;205;127;50m \x1b[0m\n" +
		"\x1b[38;2;205;127;50m ────────────────────────────────────── \x1b[0m\n"
	if got := buf.String(); got != want {
		t.Fatalf("got\n%q\nwant\n%q", got, want)
	}
}

func TestPanelPaddingPairAndRightTitleMatchRich(t *testing.T) {
	c, buf := truecolorConsole(50)
	c.PrintPanel(Panel{Body: "line one\nUse [bold]/model[/] now", Title: "[#CD7F32 bold]⚡ Limit reached[/]", TitleAlign: "left", BorderStyle: "#CD7F32",
		Box: "horizontals", Padding: &[2]int{1, 4}, Width: 44})
	want := "\x1b[38;2;205;127;50m ─\x1b[0m\x1b[38;2;205;127;50m \x1b[0m\x1b[1;38;2;205;127;50m⚡ Limit reached\x1b[0m\x1b[38;2;205;127;50m \x1b[0m\x1b[38;2;205;127;50m──────────────────────\x1b[0m\x1b[38;2;205;127;50m─ \x1b[0m\n" +
		"\x1b[38;2;205;127;50m \x1b[0m                                          \x1b[38;2;205;127;50m \x1b[0m\n" +
		"\x1b[38;2;205;127;50m \x1b[0m    line one                              \x1b[38;2;205;127;50m \x1b[0m\n" +
		"\x1b[38;2;205;127;50m \x1b[0m    Use \x1b[1m/model\x1b[0m now                        \x1b[38;2;205;127;50m \x1b[0m\n" +
		"\x1b[38;2;205;127;50m \x1b[0m                                          \x1b[38;2;205;127;50m \x1b[0m\n" +
		"\x1b[38;2;205;127;50m ────────────────────────────────────────── \x1b[0m\n"
	if got := buf.String(); got != want {
		t.Fatalf("got\n%q\nwant\n%q", got, want)
	}
	c, buf = truecolorConsole(30)
	c.PrintPanel(Panel{Body: "right", Title: "T", TitleAlign: "right", Width: 20})
	if got, want := buf.String(), "╭────────────── T ─╮\n│ right            │\n╰──────────────────╯\n"; got != want {
		t.Fatalf("got\n%q\nwant\n%q", got, want)
	}
}

func TestPrintRenderableWithStyleAndSoftWrapMatchesRich(t *testing.T) {
	c, buf := truecolorConsole(30)
	if err := c.PrintRenderableWith(&Text{Plain: "A long line of answer text that is far wider than the console width\nshort"}, PrintOptions{Style: "#FFF8DC", SoftWrap: true}); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String(), "\x1b[38;2;255;248;220mA long line of answer text that is far wider than the console width\x1b[0m\n\x1b[38;2;255;248;220mshort\x1b[0m\n"; got != want {
		t.Fatalf("text got\n%q\nwant\n%q", got, want)
	}
	c, buf = truecolorConsole(30)
	if err := c.PrintRenderableWith(&Markdown{Source: "A long paragraph of answer text that is far wider than the console.\n\n- a list item that is also much longer than the width of it\n\nshort"}, PrintOptions{Style: "#FFF8DC", SoftWrap: true}); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String(), "\x1b[38;2;255;248;220mA long paragraph of answer text that is far wider than the console.\x1b[0m\n\n\x1b[1;38;2;255;248;220m • \x1b[0m\x1b[38;2;255;248;220ma list item that is also mu\x1b[0m\n\n\x1b[38;2;255;248;220mshort\x1b[0m\n"; got != want {
		t.Fatalf("markdown got\n%q\nwant\n%q", got, want)
	}
	c, buf = truecolorConsole(30)
	if err := c.PrintRenderableWith(&Text{Plain: "plain \x1b[1mbold\x1b[0m and \x1b[31mred\x1b[0m tail"}, PrintOptions{Style: "#FFF8DC", SoftWrap: true}); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String(), "\x1b[38;2;255;248;220mplain \x1b[0m\x1b[1;38;2;255;248;220mbold\x1b[0m\x1b[38;2;255;248;220m and \x1b[0m\x1b[31mred\x1b[0m\x1b[38;2;255;248;220m tail\x1b[0m\n"; got != want {
		t.Fatalf("ansi got\n%q\nwant\n%q", got, want)
	}
	if err := c.PrintRenderableWith(&Text{Plain: "x"}, PrintOptions{Style: "#nothex"}); err == nil {
		t.Fatal("a style Rich cannot parse must return its error")
	}
}
