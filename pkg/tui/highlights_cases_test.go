package tui

import (
	"strings"
	"testing"
)

// Reference runs of a composer text
func painted(text string) []string {
	var out []string
	for _, seg := range splitComposerHighlights(text) {
		if seg.Ref {
			out = append(out, seg.Text)
		}
	}
	return out
}

func TestSplitComposerHighlights(t *testing.T) {
	eq := func(t *testing.T, text string, want ...string) {
		t.Helper()
		if got := painted(text); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("%q painted %q want %q", text, got, want)
		}
	}
	t.Run("marks a command invocation and a skill named mid-prose", func(t *testing.T) {
		eq(t, "/work fix the leak", "/work")
		eq(t, "clean this up with /clean", "/clean")
		eq(t, "run /clean then /work", "/clean", "/work")
	})
	t.Run("marks @ references, including quoted values with spaces", func(t *testing.T) {
		eq(t, "see @file:src/a.ts please", "@file:src/a.ts")
		eq(t, "see @file:`my notes.md` please", "@file:`my notes.md`")
		eq(t, "diff @diff and @staged", "@diff", "@staged")
	})
	t.Run("marks attachment and paste tokens", func(t *testing.T) {
		eq(t, "what is in [[ Image 1 ]] here", "[[ Image 1 ]]")
		eq(t, "paste [[ log.. [3 lines] ]] ok", "[[ log.. [3 lines] ]]")
	})
	t.Run("marks every kind in one message", func(t *testing.T) {
		eq(t, "/work with @file:a.ts and [[ Image 2 ]]", "/work", "@file:a.ts", "[[ Image 2 ]]")
	})
	t.Run("leaves paths, bare slashes, and email addresses alone", func(t *testing.T) {
		for _, text := range []string{"look at /usr/local/bin", "check src/foo/bar", "a 3 /4 b", "either / or", "email me@example.com"} {
			got := splitComposerHighlights(text)
			if len(got) != 1 || got[0] != (composerHighlight{Text: text}) {
				t.Fatalf("%q got %+v", text, got)
			}
		}
	})
	t.Run("marks a half-typed token so the accent tracks the caret", func(t *testing.T) {
		eq(t, "/wor", "/wor")
		eq(t, "ref @fi", "@fi")
		eq(t, "/", "/")
	})
	t.Run("round-trips the input exactly", func(t *testing.T) {
		for _, text := range []string{"/work a", "x @file:b [[ Image 1 ]]", "plain text", "", "look at /usr/local/bin"} {
			var b strings.Builder
			for _, seg := range splitComposerHighlights(text) {
				b.WriteString(seg.Text)
			}
			if b.String() != text {
				t.Fatalf("%q round tripped %q", text, b.String())
			}
		}
	})
	t.Run("always returns at least one segment", func(t *testing.T) {
		got := splitComposerHighlights("")
		if len(got) != 1 || got[0] != (composerHighlight{}) {
			t.Fatalf("got %+v", got)
		}
	})
}
