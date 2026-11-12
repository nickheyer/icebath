package tui

import (
	"strings"
	"testing"
)

// End position of text wrapped like wrap-ansi, the last row and its length
func wrapAnsiEndPosition(text string, cols int) caretLayout {
	lines := strings.Split(wrapHard(text, cols), "\n")
	return caretLayout{Line: len(lines) - 1, Column: len(lines[len(lines)-1])}
}

func TestCursorLayoutWordWrapParityWithWrapAnsi(t *testing.T) {
	eq := func(t *testing.T, got, want caretLayout) {
		t.Helper()
		if got != want {
			t.Fatalf("got %+v want %+v", got, want)
		}
	}
	t.Run("places cursor mid-line at its column", func(t *testing.T) {
		eq(t, cursorLayout("hello world", 6, 40), caretLayout{Column: 6, Line: 0})
	})
	t.Run("does not push exact-fill text onto a phantom next line", func(t *testing.T) {
		eq(t, cursorLayout("abcdefgh", 8, 8), caretLayout{Column: 8, Line: 0})
		eq(t, cursorLayout("abcdefgh", 8, 8), wrapAnsiEndPosition("abcdefgh", 8))
	})
	t.Run("keeps short words on the current line when they fit (no phantom wrap)", func(t *testing.T) {
		eq(t, cursorLayout("hello wo", 8, 8), caretLayout{Column: 8, Line: 0})
		eq(t, cursorLayout("hello wo", 8, 8), wrapAnsiEndPosition("hello wo", 8))
	})
	t.Run("moves words across wrap boundaries instead of splitting them", func(t *testing.T) {
		eq(t, cursorLayout("hello wor", 9, 8), caretLayout{Column: 3, Line: 1})
		eq(t, cursorLayout("hello worl", 10, 8), caretLayout{Column: 4, Line: 1})
		eq(t, cursorLayout("hello world", 11, 8), caretLayout{Column: 5, Line: 1})
		for _, s := range []string{"hello wor", "hello worl", "hello world"} {
			eq(t, cursorLayout(s, len(s), 8), wrapAnsiEndPosition(s, 8))
		}
	})
	t.Run("wraps the next word instead of splitting it at the right edge", func(t *testing.T) {
		text := "hello world baby chickens are so cool its really rainy outside but wish"
		eq(t, cursorLayout(text, len(text), 70), caretLayout{Column: 4, Line: 1})
		if inputVisualHeight(text, 70) != 2 {
			t.Fatal("height")
		}
	})
	t.Run("honours explicit newlines", func(t *testing.T) {
		eq(t, cursorLayout("one\ntwo", 5, 40), caretLayout{Column: 1, Line: 1})
		eq(t, cursorLayout("one\ntwo", 4, 40), caretLayout{Column: 0, Line: 1})
	})
	t.Run("matches wrap-ansi end-position for typing-style incremental input", func(t *testing.T) {
		msg := "on a new bb branch investigate and fix the cursor drift bug here"
		for _, cols := range []int{10, 14, 20, 30, 50, 80} {
			acc := ""
			for _, ch := range msg {
				acc += string(ch)
				eq(t, cursorLayout(acc, len(acc), cols), wrapAnsiEndPosition(acc, cols))
			}
		}
	})
}

func TestInputMetricsHelpers(t *testing.T) {
	t.Run("computes visual height matching wrap-ansi line count", func(t *testing.T) {
		if inputVisualHeight("abcdefgh", 8) != 1 || inputVisualHeight("one\ntwo", 40) != 2 || inputVisualHeight("hello world", 8) != 2 {
			t.Fatal("heights")
		}
	})
	t.Run("counts the prompt gap as its own cell", func(t *testing.T) {
		if composerPromptWidth(">") != 2 || composerPromptWidth("\u276f") != 2 || composerPromptWidth("\u03a8 >") != 4 {
			t.Fatal("prompt widths")
		}
	})
	t.Run("reserves gutters on wide panes without starving narrow composer width", func(t *testing.T) {
		for _, c := range [][3]int{{100, 3, 93}, {100, 5, 91}, {10, 3, 5}, {6, 3, 1}} {
			if got := stableComposerColumns(c[0], c[1], false); got != c[2] {
				t.Fatalf("%v got %d", c, got)
			}
		}
	})
}

func TestOffsetFromPositionWordWrapInverseOfCursorLayout(t *testing.T) {
	text := "hello world baby chickens are so cool its really rainy outside but wish"
	cases := []struct {
		name       string
		value      string
		row, col   int
		cols, want int
	}{
		{"returns 0 for empty input", "", 0, 0, 10, 0},
		{"maps clicks within a single line", "hello", 0, 3, 40, 3},
		{"maps clicks past end to value length", "hi", 0, 10, 40, 2},
		{"maps clicks on a wrapped second row at cols boundary", "abcdefghij", 1, 0, 8, 8},
		{"maps clicks on a word-wrapped second row", "hello world", 1, 0, 8, 6},
		{"maps clicks on a word-wrapped second row", "hello world", 1, 3, 8, 9},
		{"maps clicks on the moved final word", text, 1, 0, 70, strings.Index(text, "wish")},
		{"maps clicks on the moved final word", text, 1, 3, 70, strings.Index(text, "wish") + 3},
		{"maps clicks past a \\n into the target line", "one\ntwo", 1, 2, 40, 6},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := offsetFromPosition(c.value, c.row, c.col, c.cols); got != c.want {
				t.Fatalf("got %d want %d", got, c.want)
			}
		})
	}
}
