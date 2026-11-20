package tui

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/ui/txt"
)

func TestSameToolTrailGroup(t *testing.T) {
	t.Run("matches bare check lines", func(t *testing.T) {
		if !sameToolTrailGroup("searching", "searching ✓") || !sameToolTrailGroup("searching", "searching ✗") {
			t.Fatal("bare")
		}
	})
	t.Run("matches contextual lines", func(t *testing.T) {
		if !sameToolTrailGroup("searching", "searching: * ✓") || !sameToolTrailGroup("searching", "searching: foo ✓") {
			t.Fatal("contextual")
		}
	})
	t.Run("rejects other tools", func(t *testing.T) {
		if sameToolTrailGroup("searching", "reading ✓") || sameToolTrailGroup("searching", "searching extra ✓") {
			t.Fatal("other")
		}
	})
}

func TestBoundedLiveRenderText(t *testing.T) {
	t.Run("preserves short live text verbatim", func(t *testing.T) {
		if got := boundedLiveRenderText("one\ntwo", 100, 10); got != "one\ntwo" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("keeps the live tail by character budget", func(t *testing.T) {
		out := boundedLiveRenderText("abcdefghij", 4, 10)
		if !strings.Contains(out, "ghij") || !strings.Contains(out, "omitted") || strings.Contains(out, "abcdef") {
			t.Fatalf("out %q", out)
		}
	})
	t.Run("keeps the live tail by line budget", func(t *testing.T) {
		out := boundedLiveRenderText("a\nb\nc\nd", 100, 2)
		if !strings.Contains(out, "c\nd") || !strings.Contains(out, tr(txt.LibTextTextOmittedLinesChars, tr(txt.LibTextTextShowingLiveTail), "2", "4")) || strings.Contains(out, "a\nb") {
			t.Fatalf("out %q", out)
		}
	})
}

func TestEdgePreview(t *testing.T) {
	t.Run("keeps both ends for long text", func(t *testing.T) {
		if got := edgePreview("Vampire Bondage ropes slipped from her neck, still stained with blood", 8, 18); got != "Vampire.. stained with blood" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestPasteTokenLabel(t *testing.T) {
	t.Run("builds readable long-paste labels with counts", func(t *testing.T) {
		label := pasteTokenLabel("Vampire Bondage ropes slipped from her neck, still stained with blood", 250)
		if !strings.HasPrefix(label, "[[ ") || !strings.Contains(label, tr(txt.LibTextTextPasteLinesChip, "250")) || !strings.HasSuffix(label, " ]]") {
			t.Fatalf("label %q", label)
		}
	})
}

func TestTextHelpersMatchJavaScript(t *testing.T) {
	t.Run("toFixed rounds the exact value half up", func(t *testing.T) {
		for in, want := range map[float64]string{0.25: "0.3", 1.25: "1.3", 0.94: "0.9", 2: "2.0", 1.05: "1.1", 0.35: "0.3"} {
			if got := jsToFixed(in, 1); got != want {
				t.Fatalf("%v gave %q want %q", in, got, want)
			}
		}
	})
	t.Run("compact numbers promote at the unit boundary", func(t *testing.T) {
		for in, want := range map[float64]string{0: "0", 250: "250", 999.4: "999", 999.5: "1k", 1500: "1.5k", 999_950: "1M", 2_500_000: "2.5M"} {
			if got := compactNumber(in); got != want {
				t.Fatalf("%v gave %q want %q", in, got, want)
			}
		}
	})
	t.Run("stripAnsi drops escapes and control bytes", func(t *testing.T) {
		if got := stripAnsi("\x1b[31mred\x1b[0m \x1b]8;;http://x\x07link\x1b]8;;\x07\r\x1b[12"); got != "red link" {
			t.Fatalf("got %q", got)
		}
	})
}
