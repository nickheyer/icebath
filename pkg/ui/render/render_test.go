package render

import (
	"image/color"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/theme"
)

func TestMarkdownRendersPerTheme(t *testing.T) {
	src := "# Title\n\nSome *emphasis* and `code`.\n\n- one\n- two\n\n```go\nfunc main() {}\n```\n\n[link](https://x)\n"
	for _, th := range []theme.Theme{theme.Blackwall(), theme.Light(), theme.Contrast(), theme.Plain()} {
		out := MarkdownText(src, 40, th)
		plain := ansi.Strip(out)
		for _, want := range []string{"Title", "emphasis", "code", "one", "two", "func main", "link"} {
			if !strings.Contains(plain, want) {
				t.Fatalf("%s lacks %q:\n%s", th.Name, want, plain)
			}
		}
		if strings.HasSuffix(out, "\n") {
			t.Fatal("trailing newline must be trimmed")
		}
	}
	m := NewMarkdown()
	a := m.Render("hello", 5, theme.Blackwall())
	b := m.Render("hello", 5, theme.Blackwall())
	if a != b || len(m.cache) != 1 {
		t.Fatal("renderers must be cached per theme and width")
	}
	if m.Render("", 20, theme.Blackwall()) != "" {
		t.Fatal("empty input")
	}
	if hex(nil) != nil || *hex(theme.Hex(0x0a0b0c)) != "#0a0b0c" {
		t.Fatal("hex")
	}
	if !isLight(theme.Light()) || isLight(theme.Blackwall()) || isLight(theme.Theme{}) {
		t.Fatal("light detection")
	}
	if !strings.Contains(Link("x", "https://x", theme.Blackwall()), "https://x") {
		t.Fatal("link must carry the url")
	}
	fl := FileLink("/a/b.go", 12, theme.Blackwall())
	if !strings.Contains(fl, "file:///a/b.go#12") || !strings.Contains(ansi.Strip(fl), "/a/b.go:12") {
		t.Fatalf("%q", fl)
	}
	if !strings.Contains(ansi.Strip(FileLink("/a", 0, theme.Blackwall())), "/a") {
		t.Fatal("file link without line")
	}
}

func TestCodeHighlightsByLanguageOrPath(t *testing.T) {
	src := "package main\nfunc main() {}\n"
	for _, th := range []theme.Theme{theme.Blackwall(), theme.Light(), theme.Contrast(), theme.Plain()} {
		out := Code(src, "go", "", th)
		if ansi.Strip(out) != strings.TrimRight(src, "\n") {
			t.Fatalf("%s: %q", th.Name, ansi.Strip(out))
		}
		if !th.Plain && !strings.Contains(out, "\x1b[") {
			t.Fatal("highlighting must add styles")
		}
	}
	if ansi.Strip(Code("x = 1", "", "a.py", theme.Blackwall())) != "x = 1" {
		t.Fatal("path lexer")
	}
	if ansi.Strip(Code("plain words here", "", "", theme.Blackwall())) != "plain words here" {
		t.Fatal("analysed lexer")
	}
	if Code("", "go", "", theme.Blackwall()) != "" {
		t.Fatal("empty source")
	}
	if Language("main.go") != "Go" || Language("x.unknownext") != "" {
		t.Fatalf("%q", Language("main.go"))
	}
	if CodeStyle(theme.Plain()) != "bw" || CodeStyle(theme.Light()) != "github" || CodeStyle(theme.Contrast()) != "github-dark" || CodeStyle(theme.Blackwall()) != "catppuccin-mocha" {
		t.Fatal("code styles")
	}
}

func TestDiffFindsChanges(t *testing.T) {
	before := "a\nb\nc\nd\ne\nf\ng\n"
	after := "a\nb\nC\nd\ne\nf\ng\nh\n"
	lines := Diff(before, after)
	added, removed := Stat(lines)
	if added != 2 || removed != 1 {
		t.Fatalf("%d %d %+v", added, removed, lines)
	}
	if lines[2].Op != Removed || lines[2].Old != 3 || lines[3].Op != Added || lines[3].New != 3 || lines[len(lines)-1].Text != "h" {
		t.Fatalf("%+v", lines)
	}
	hunks := Hunks(lines, 1)
	if len(hunks) != 2 || len(hunks[0]) != 4 || hunks[1][len(hunks[1])-1].Text != "h" {
		t.Fatalf("%+v", hunks)
	}
	if len(Hunks(Diff("x", "x"), 3)) != 0 {
		t.Fatal("no hunks without change")
	}
	if n := len(Diff("", "one\ntwo")); n != 2 {
		t.Fatalf("%d", n)
	}
	if n := len(Diff("one", "")); n != 1 {
		t.Fatalf("%d", n)
	}
	moved := Diff("1\n2\n3\n4", "2\n3\n4\n1")
	if a, r := Stat(moved); a != 1 || r != 1 {
		t.Fatalf("%d %d", a, r)
	}
	big := strings.Repeat("x\n", MaxDiffLines)
	if a, r := Stat(Diff(big, big+"y\n")); a != MaxDiffLines+1 || r != MaxDiffLines {
		t.Fatal("oversize diffs list both sides")
	}
	th := theme.Blackwall()
	out := ansi.Strip(DiffText(before, after, 40, th))
	for _, want := range []string{"- c", "+ C", "+ h", "┈", th.Glyphs.Info} {
		if !strings.Contains(out, want) {
			t.Fatalf("lacks %q:\n%s", want, out)
		}
	}
	if ansi.Strip(DiffText("same", "same", 40, th)) != "no changes" {
		t.Fatal("no changes")
	}
	tabbed := ansi.Strip(FormatDiff(Diff("\tx", "\ty"), 30, th))
	if strings.Contains(tabbed, "\t") {
		t.Fatal("tabs expanded")
	}
}

func TestFormatters(t *testing.T) {
	th := theme.Blackwall()
	if Truncate("hello", 0, th) != "" || Truncate("hello", 1, th) != "⋯" || Truncate("hello", 3, th) != "he⋯" || Truncate("hi", 5, th) != "hi" {
		t.Fatal("truncate")
	}
	plain := theme.Plain()
	if Truncate("hello", 2, plain) != ".." || Truncate("hello world", 6, plain) != "hel..." || Truncate("hi", 5, plain) != "hi" {
		t.Fatalf("plain truncate %q %q", Truncate("hello", 2, plain), Truncate("hello world", 6, plain))
	}
	if ansi.StringWidth(Fit("hi", 5, th)) != 5 || ansi.StringWidth(Fit("hello world", 5, th)) != 5 {
		t.Fatal("fit")
	}
	if ansi.StringWidth(Divider(20, th)) != 20 || !strings.Contains(Divider(20, th), th.Glyphs.Info) || strings.Contains(Divider(5, th), th.Glyphs.Info) || Divider(0, th) != "" {
		t.Fatalf("divider %q", Divider(20, th))
	}
	if Wrap("aaaa bbbb", 4) != "aaaa\nbbbb" || Wrap("x", 0) != "x" {
		t.Fatalf("%q", Wrap("aaaa bbbb", 4))
	}
	for d, want := range map[time.Duration]string{-1: "0s", 500 * time.Millisecond: "500ms", 1500 * time.Millisecond: "1.5s", 90 * time.Second: "1m30s", 3*time.Hour + 5*time.Minute: "3h05m"} {
		if Elapsed(d) != want {
			t.Fatalf("%v %q", d, Elapsed(d))
		}
	}
	for n, want := range map[int]string{-1: "0", 999: "999", 1500: "1.5k", 2000: "2k", 1_250_000: "1.25M"} {
		if Tokens(n) != want {
			t.Fatalf("%d %q", n, Tokens(n))
		}
	}
	for n, want := range map[int64]string{-1: "0 B", 10: "10 B", 2048: "2 KiB", 1536: "1.5 KiB", 3 << 20: "3 MiB", 5 << 30: "5 GiB"} {
		if Bytes(n) != want {
			t.Fatalf("%d %q", n, Bytes(n))
		}
	}
	if Money(0) != "$0.00" || Money(0.001) != "$0.0010" || Money(1.5) != "$1.50" || Percent(0.456) != "46%" || Percent(2) != "100%" {
		t.Fatal("money or percent")
	}
	if trimZero("7") != "7" {
		t.Fatal("trim zero")
	}
	if Bar(0.5, 0, th) != "" || ansi.StringWidth(Bar(0.5, 10, th)) != 10 || ansi.StringWidth(Bar(2, 4, theme.Plain())) != 4 {
		t.Fatal("bar")
	}
	if Gauge(0.5, 0, th) != "" || ansi.StringWidth(Gauge(0.95, 10, th)) != 10 || ansi.StringWidth(Gauge(0.75, 10, theme.Plain())) != 10 {
		t.Fatal("gauge")
	}
	if Spinner(3, th) != th.Glyphs.Spinner[3] || Spinner(1, theme.Plain()) != "/" || Spinner(2, theme.Theme{}) != "" {
		t.Fatal("spinner")
	}
	for name, want := range map[string]string{"Read": th.Glyphs.Read, "Bash": th.Glyphs.Bash, "mcp__x__y": th.Glyphs.Server, "Other": th.Glyphs.Tool, "Agent": th.Glyphs.Spawn} {
		if ToolGlyph(name, th) != want {
			t.Fatalf("%s %s", name, ToolGlyph(name, th))
		}
	}
	if ToolGlyph("Bash", theme.Plain()) != "$" {
		t.Fatal("plain tool glyph")
	}
	for _, s := range []string{KV("k", "v", th), Heading("h", th), Dim("d", th), Bad("b", th), Good("g", th), Warm("w", th), Cool("c", th), Hot("h", th)} {
		if ansi.Strip(s) == "" {
			t.Fatal("styled text")
		}
	}
	if len(Lines("")) != 0 || len(Lines("a\nb\n")) != 2 || Tail("a\nb\nc", 2) != "b\nc" || Tail("a", 5) != "a" {
		t.Fatal("lines")
	}
	if Summary("  first line\nsecond", 20, th) != "first line ⋯" || Summary("x", 1, th) != "x" {
		t.Fatalf("%q", Summary("  first line\nsecond", 20, th))
	}
}

func TestTableFitsWidth(t *testing.T) {
	th := theme.Blackwall()
	cols := []Column{{Title: "NAME", Flex: 1}, {Title: "N", Right: true}, {Title: "STATE", Max: 4}}
	rows := [][]string{{"alpha", "1", "running"}, {"beta-longer-name", "22", "idle"}, {"short"}}
	out := Table(cols, rows, 40, 1, th)
	lines := strings.Split(ansi.Strip(out), "\n")
	if len(lines) != 3+TableHead {
		t.Fatalf("%v", lines)
	}
	for _, l := range lines {
		if ansi.StringWidth(l) > 40 {
			t.Fatalf("line too wide: %q", l)
		}
	}
	if !strings.Contains(lines[0], "NAME") || !strings.HasPrefix(lines[1], "──") || !strings.HasSuffix(strings.TrimRight(lines[2], " "), "run⋯") || !strings.HasPrefix(lines[3], "►") || !strings.Contains(lines[3], "22") {
		t.Fatalf("%v", lines)
	}
	tight := strings.Split(ansi.Strip(Table(cols, rows, 12, -1, th)), "\n")
	for _, l := range tight {
		if ansi.StringWidth(l) > 12 {
			t.Fatalf("tight line too wide: %q", l)
		}
	}
	if Table(nil, rows, 40, 0, th) != "" || Table(cols, rows, 0, 0, th) != "" {
		t.Fatal("empty")
	}
	noFlex := Table([]Column{{Title: "A"}, {Title: "B"}}, [][]string{{"1", "2"}}, 4, 0, th)
	if ansi.StringWidth(strings.Split(ansi.Strip(noFlex), "\n")[0]) > 4 {
		t.Fatal("fixed columns cut from the end")
	}
	marked := strings.Split(ansi.Strip(TableMarked(cols, rows, 40, 1, func(i int) bool { return i != 2 }, nil, th)), "\n")
	if !strings.HasPrefix(marked[2], "▮") || !strings.HasPrefix(marked[3], "►") || strings.HasPrefix(marked[4], "▮") {
		t.Fatalf("marked rows carry a tick, the cursor its arrow %v", marked)
	}
}

func TestSelectedRowKeepsItsBackgroundPastStyledCells(t *testing.T) {
	th := theme.Blackwall()
	cols := []Column{{Title: "NAME"}, {Title: "STATE"}}
	out := Table(cols, [][]string{{"idk", Good("on", th)}}, 30, 0, th)
	buf := uv.NewScreenBuffer(30, 3)
	uv.NewStyledString(out).Draw(buf, buf.Bounds())
	same := func(a, b color.Color) bool {
		if a == nil || b == nil {
			return false
		}
		ar, ag, ab, _ := a.RGBA()
		br, bg, bb, _ := b.RGBA()
		return ar == br && ag == bg && ab == bb
	}
	for x := range 30 {
		if c := buf.CellAt(x, 2); c == nil || !same(c.Style.Bg, th.Steel) {
			t.Fatalf("cell %d of the selected row lost the row background %+v", x, c)
		}
	}
	xs, _ := Columns(cols, [][]string{{"idk", "on"}}, 30)
	if c := buf.CellAt(xs[1], 2); c == nil || c.Content != "o" || !same(c.Style.Fg, th.Alive) {
		t.Fatalf("a styled cell keeps its own color %+v", c)
	}
	painted := Paint(lipgloss.NewStyle().Background(th.Steel), "a\x1b[0mb\x1b[mc")
	buf = uv.NewScreenBuffer(3, 1)
	uv.NewStyledString(painted).Draw(buf, buf.Bounds())
	for x := range 3 {
		if c := buf.CellAt(x, 0); c == nil || !same(c.Style.Bg, th.Steel) {
			t.Fatalf("paint restarts the style after either reset, cell %d %+v", x, c)
		}
	}
}

func TestCompactDropsGlamourPaddingButKeepsBackgrounds(t *testing.T) {
	fg := "\x1b[38;2;1;2;3m"
	cases := map[string]string{
		fg + "hello\x1b[m" + fg + " \x1b[m" + fg + " \x1b[m": fg + "hello\x1b[m",
		"abc   ":                        "abc\x1b[m",
		fg + " \x1b[m" + fg + " \x1b[m": "\x1b[m",
		"\x1b[48;2;9;9;9mcode\x1b[m\x1b[48;2;9;9;9m   \x1b[m": "\x1b[48;2;9;9;9mcode\x1b[m\x1b[48;2;9;9;9m   \x1b[m",
		"\x1b[7mcode   \x1b[m":                                "\x1b[7mcode   \x1b[m",
		"\x1b[7mcode\x1b[m   ":                                "\x1b[7mcode\x1b[m",
		"\x1b[44mx\x1b[49m  ":                                 "\x1b[44mx\x1b[m",
		"\x1b[1;7mx  \x1b[27m  ":                              "\x1b[1;7mx  \x1b[m",
		"\x1b[38;2;47;47;47mtext\x1b[m \x1b[m":                "\x1b[38;2;47;47;47mtext\x1b[m",
		"plain":                                               "plain",
		"":                                                    "",
	}
	for in, want := range cases {
		if got := compact(in); got != want {
			t.Fatalf("compact(%q) = %q, want %q", in, got, want)
		}
	}
	th := theme.Blackwall()
	out := MarkdownText("A paragraph of prose that wraps around.\n\n```go\nx := 1\n```\n\nMore prose.", 24, th)
	for _, l := range strings.Split(out, "\n") {
		if strings.HasSuffix(ansi.Strip(l), " ") {
			t.Fatalf("no line carries trailing padding %q", l)
		}
	}
	if !strings.Contains(ansi.Strip(out), "  x := 1") {
		t.Fatal("the code block keeps its indent")
	}
	if len(out) > 20*len(ansi.Strip(out)) {
		t.Fatalf("the render is not many times its text, %d bytes for %d", len(out), len(ansi.Strip(out)))
	}
}
