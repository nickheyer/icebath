package pyrich

import "testing"

func TestPlainAndEscape(t *testing.T) {
	cases := map[string]string{
		"[red]Error:[/red] boom":           "Error: boom",
		"[bold green]ok[/] done":           "ok done",
		`literal \[red] tag`:               "literal [red] tag",
		`two \\[red]x[/red]`:               `two \x`,
		"list [1] and [A] stay":            "list [1] and [A] stay",
		"[link=https://x.y]site[/link] ok": "site ok",
		"no tags":                          "no tags",
	}
	for in, want := range cases {
		if got := Plain(in); got != want {
			t.Errorf("Plain(%q) = %q, want %q", in, got, want)
		}
	}
	raw := "git said [error] at C:\\"
	if got := Plain(Escape(raw)); got != raw+`\` {
		t.Errorf("escape round trip %q -> %q", raw, got)
	}
	if got := Plain(`brace \[1] stays`); got != "brace [1] stays" {
		t.Errorf("open brace escape = %q", got)
	}
	if got := Escape("[red]x"); got != `\[red]x` {
		t.Errorf("Escape = %q", got)
	}
	if got := Render("[red]x[/red]", true); got != "\x1b[31mx\x1b[0m" {
		t.Errorf("Render color = %q", got)
	}
}

func TestTableRender(t *testing.T) {
	tb := &Table{Title: "T", Columns: []Column{{Header: "Name", Style: "bold"}, {Header: "Tier"}}}
	tb.AddRow("abc", "[green]verified[/green]")
	tb.AddRow("x\ny", "c")
	want := "         T\n" +
		"┏━━━━━━┳━━━━━━━━━━┓\n" +
		"┃ Name ┃ Tier     ┃\n" +
		"┡━━━━━━╇━━━━━━━━━━┩\n" +
		"│ abc  │ verified │\n" +
		"│ x    │ c        │\n" +
		"│ y    │          │\n" +
		"└──────┴──────────┘"
	if got := tb.Render(false); got != want {
		t.Errorf("table render\n%s\nwant\n%s", got, want)
	}
}
