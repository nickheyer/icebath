package strs

import (
	"slices"
	"testing"
)

func TestShellWords(t *testing.T) {
	for line, want := range map[string][]string{
		`npx -y server --root "/my docs"`: {"npx", "-y", "server", "--root", "/my docs"},
		`grep "\d+\.json" 'it''s' a\ b`:   {"grep", `\d+\.json`, "its", "a b"},
		`echo "\$HOME \"q\" \\ \x"`:       {"echo", `$HOME "q" \ \x`},
		"one \\\n two":                    {"one", "two"},
		"jo\\\nined \"a\\\nb\"":           {"joined", "ab"},
		`'' ""`:                           {"", ""},
	} {
		got, err := Shell(line)
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("%q splits to %q, %v", line, got, err)
		}
		if again, err := Shell(Quote(got)); err != nil || !slices.Equal(again, got) {
			t.Fatalf("%q quoted reads back as %q, %v", got, again, err)
		}
	}
	for _, line := range []string{`"open`, `'open`, `ends\`} {
		if _, err := Shell(line); err == nil {
			t.Fatalf("%q split without an error", line)
		}
	}
}
