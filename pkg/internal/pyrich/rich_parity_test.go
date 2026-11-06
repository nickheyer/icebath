package pyrich

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// Recovers the MarkupError a markup panics with, nil when the markup parses
func markupPanic(markup string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			var me *MarkupError
			if e, ok := r.(error); ok && errors.As(e, &me) {
				err = me
				return
			}
			panic(r)
		}
	}()
	Plain(markup)
	return nil
}

func TestRichParityCasesMatchRich15(t *testing.T) {
	for _, c := range richParityCases {
		if c.kind == "error" {
			err := markupPanic(c.markup)
			if err == nil || err.Error() != c.want {
				t.Errorf("Plain(%q) error %v want %q", c.markup, err, c.want)
			}
			continue
		}
		if got := Plain(c.markup); got != c.plain {
			t.Errorf("%s Plain(%q) = %q want %q", c.kind, c.markup, got, c.plain)
		}
		setColorSystem(t, c.system)
		var b bytes.Buffer
		(&Console{Out: &b, Color: true, Width: 40, NoColor: c.kind == "nocolor"}).Print(c.markup)
		got := oscLinkIDRE.ReplaceAllString(b.String(), "\x1b]8;id=;")
		want := oscLinkIDRE.ReplaceAllString(c.want, "\x1b]8;id=;")
		if got != want {
			t.Errorf("%s %s Print(%q)\n got %q\nwant %q", c.kind, c.system, c.markup, got, want)
		}
	}
}

func TestNoColorEnvKeepsStylingOnTerminalsOnly(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	c := NewConsole(&bytes.Buffer{})
	if !c.NoColor || c.Color {
		t.Fatalf("buffer console NoColor %v Color %v", c.NoColor, c.Color)
	}
	t.Setenv("NO_COLOR", "")
	if NewConsole(&bytes.Buffer{}).NoColor {
		t.Fatal("empty NO_COLOR enabled no color mode")
	}
}

func TestStatusRaisesMarkupErrorInCaller(t *testing.T) {
	var b bytes.Buffer
	c := &Console{Out: &b}
	defer func() {
		r := recover()
		e, ok := r.(*MarkupError)
		if !ok || !strings.Contains(e.Msg, "has nothing to close") {
			t.Fatalf("recovered %v", r)
		}
	}()
	c.Status("loading[/]")
}

func TestMarkdownStripsControlCodes(t *testing.T) {
	lines := (&Markdown{Source: "a\x07b\x08c"}).RenderLines(false, "", 20)
	if strings.Join(lines, "\n") != "abc"+strings.Repeat(" ", 17) {
		t.Fatalf("lines %q", lines)
	}
}
