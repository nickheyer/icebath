package pyrich

import (
	"regexp"
	"strings"
	"testing"
)

var linkIDRE = regexp.MustCompile(`id=\d+;`)

func TestMarkdownMatchesRich(t *testing.T) {
	for _, c := range richMarkdownCases {
		t.Run(c.name, func(t *testing.T) {
			var got string
			if c.panel {
				got = Panel{Content: &Markdown{Source: c.source}, BorderStyle: "green", Fit: true}.Render(c.system != "", c.width) + "\n"
			} else {
				got = strings.Join((&Markdown{Source: c.source}).RenderLines(c.system != "", c.system, c.width), "\n") + "\n"
			}
			want := linkIDRE.ReplaceAllString(c.want, "id=0;")
			if got = linkIDRE.ReplaceAllString(got, "id=0;"); got != want {
				t.Fatalf("got\n%q\nwant\n%q", got, want)
			}
		})
	}
}

func TestPanelFitsBodyAndTitle(t *testing.T) {
	got := Panel{Body: "[green bold]Plugin installed:[/] owner/repo\n[dim]Location:[/] /h/plugins/repo", BorderStyle: "green", Title: "✓ Installed", Fit: true}.Render(false, 40)
	want := "╭──────── ✓ Installed ─────────╮\n│ Plugin installed: owner/repo │\n│ Location: /h/plugins/repo    │\n╰──────────────────────────────╯"
	if got != want {
		t.Fatalf("\n%s\n%s", got, want)
	}
	if got := (Panel{Body: "x", Title: "✓ Installed", Fit: true}).Render(false, 40); got != "╭─ ✓ Installed ─╮\n│ x             │\n╰───────────────╯" {
		t.Fatalf("\n%s", got)
	}
}

func TestColorDowngrade(t *testing.T) {
	for _, c := range []struct{ name, system, want string }{
		{"#272822", "256", "48;5;235"}, {"#f8f8f2", "256", "48;5;231"}, {"#272822", "truecolor", "48;2;39;40;34"}, {"cyan", "256", "46"}, {"bright_blue", "standard", "104"},
	} {
		if got := colorCodes(c.name, false, c.system); got != c.want {
			t.Fatalf("%s on %s %q want %q", c.name, c.system, got, c.want)
		}
	}
}
