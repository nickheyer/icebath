package pyrich

import (
	"bytes"
	"reflect"
	"regexp"
	"testing"
)

func TestReprSpansMatchRichHighlighter(t *testing.T) {
	for _, c := range reprSpanCases {
		got := reprSpans(c.text)
		if len(got) == 0 && len(c.spans) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.spans) {
			t.Errorf("reprSpans(%q)\n got %v\nwant %v", c.text, got, c.spans)
		}
	}
}

var oscLinkIDRE = regexp.MustCompile("\x1b\\]8;id=[^;]*;")

func setColorSystem(t *testing.T, system string) {
	switch system {
	case "truecolor":
		t.Setenv("COLORTERM", "truecolor")
		t.Setenv("TERM", "xterm")
	case "256":
		t.Setenv("COLORTERM", "")
		t.Setenv("TERM", "xterm-256color")
	default:
		t.Setenv("COLORTERM", "")
		t.Setenv("TERM", "xterm")
	}
}

func TestConsolePrintMatchesRichOnColorTerminals(t *testing.T) {
	for _, c := range richPrintCases {
		setColorSystem(t, c.system)
		var b bytes.Buffer
		(&Console{Out: &b, Color: true, Width: 40}).Print(c.markup)
		got := oscLinkIDRE.ReplaceAllString(b.String(), "\x1b]8;id=;")
		want := oscLinkIDRE.ReplaceAllString(c.want, "\x1b]8;id=;")
		if got != want {
			t.Errorf("%s Print(%q)\n got %q\nwant %q", c.system, c.markup, got, want)
		}
	}
}

func TestPrintNoHighlightKeepsOnlyMarkupStyles(t *testing.T) {
	setColorSystem(t, "standard")
	var b bytes.Buffer
	c := &Console{Out: &b, Color: true, Width: 40}
	c.PrintNoHighlight("--- a/x 1 (2)")
	c.Print("[green]+ added 1[/]")
	want := "--- a/x 1 (2)\n\x1b[32m+ added \x1b[0m\x1b[1;32m1\x1b[0m\n"
	if b.String() != want {
		t.Fatalf("got %q want %q", b.String(), want)
	}
	b.Reset()
	(&Console{Out: &b, Color: true, Width: 40, NoHighlight: true}).Print("x 1 (2)")
	if b.String() != "x 1 (2)\n" {
		t.Fatalf("NoHighlight console printed %q", b.String())
	}
}

func TestParseStyleFollowsRichGrammar(t *testing.T) {
	cases := []struct {
		def, str string
		err      bool
	}{
		{"bold red on blue", "bold red on blue", false},
		{"RED b not italic", "bold not italic red", false},
		{"link https://x.io u", "underline link https://x.io", false},
		{"none", "none", false},
		{"", "none", false},
		{"color(300)", "", true},
		{"rgb(1,2)", "", true},
		{"on", "", true},
		{"not purple", "", true},
		{"y/N", "", true},
		{"grey23 on #FF8800", "grey23 on #ff8800", false},
	}
	for _, c := range cases {
		st, err := ParseStyle(c.def)
		if (err != nil) != c.err {
			t.Errorf("ParseStyle(%q) err %v", c.def, err)
			continue
		}
		if !c.err && st.String() != c.str {
			t.Errorf("ParseStyle(%q) = %q want %q", c.def, st.String(), c.str)
		}
	}
	if normalizeStyle(" Red  BOLD ") != "bold red" || normalizeStyle("Y/N") != "y/n" {
		t.Fatalf("normalizeStyle %q %q", normalizeStyle(" Red  BOLD "), normalizeStyle("Y/N"))
	}
}

func TestColorDowngradeMatchesRich(t *testing.T) {
	cases := []struct{ name, system, fg string }{
		{"grey23", "standard", "90"}, {"grey23", "256", "38;5;237"}, {"grey23", "truecolor", "38;5;237"},
		{"rgb(249,38,114)", "256", "38;5;197"}, {"rgb(249,38,114)", "standard", "91"},
		{"#808080", "256", "38;5;244"}, {"default", "standard", "39"}, {"bright_red", "256", "91"},
		{"color(3)", "truecolor", "33"},
	}
	for _, c := range cases {
		if got := colorCodes(c.name, true, c.system); got != c.fg {
			t.Errorf("colorCodes(%q, %s) = %q want %q", c.name, c.system, got, c.fg)
		}
	}
}
