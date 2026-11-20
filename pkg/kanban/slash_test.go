package kanban

import (
	"context"
	"errors"
	"testing"
)

// Help tokens, a help dump, an argument error, a bare parser failure and quoted tokens each answer as run_slash does
func TestRunSlashParserOutcomes(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	var seen [][]string
	answer := SlashParse{}
	prev := ParseSlash
	t.Cleanup(func() { ParseSlash = prev })
	ParseSlash = func(tokens []string) SlashParse {
		seen = append(seen, tokens)
		return answer
	}
	for _, rest := range []string{"", "  ", "help", "-h", "--help", "?"} {
		if out, err := RunSlash(ctx, rest); err != nil || out != SlashKanbanHelp {
			t.Fatalf("%q answered %q %v", rest, out, err)
		}
	}
	if len(seen) != 0 {
		t.Fatal("help reached the parser")
	}
	cases := []struct {
		parse SlashParse
		want  string
	}{
		{SlashParse{Out: "usage: /kanban list\n\nList tasks\n"}, "usage: /kanban list\n\nList tasks"},
		{SlashParse{ArgError: "argument --limit: invalid int value: 'x'", Usage: "usage: /kanban list [--limit N]", Failure: errors.New("x")},
			"⚠ /kanban usage error\nusage: /kanban list [--limit N]\nargument --limit: invalid int value: 'x'"},
		{SlashParse{Err: "/kanban: error: unrecognized arguments: --nope\n", Failure: errors.New("exit 2")},
			"⚠ /kanban usage error\n/kanban: error: unrecognized arguments: --nope"},
		{SlashParse{Failure: errors.New("parser broke")}, "⚠ /kanban usage error\nparser broke"},
	}
	for _, c := range cases {
		answer = c.parse
		if out, err := RunSlash(ctx, `show "my task" 'two words'`); err != nil || out != c.want {
			t.Fatalf("answered %q %v want %q", out, err, c.want)
		}
	}
	if got := seen[0]; len(got) != 3 || got[1] != "my task" || got[2] != "two words" {
		t.Fatalf("tokens %q", got)
	}
}
