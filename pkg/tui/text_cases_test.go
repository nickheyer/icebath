package tui

import (
	"strings"
	"testing"
)

func float(v float64) *float64 { return &v }

func TestIsToolTrailResultLine(t *testing.T) {
	t.Run("detects completion markers", func(t *testing.T) {
		if !isToolTrailResultLine("foo ✓") || !isToolTrailResultLine("foo ✗") || isToolTrailResultLine("drafting x…") {
			t.Fatal("markers")
		}
	})
}

func TestBuildToolTrailLine(t *testing.T) {
	t.Run("puts completion duration inline before the result marker", func(t *testing.T) {
		line := buildToolTrailLine("read_file", "x", false, "", float(0.94))
		if line != `Read File("x") (0.9s) ✓` {
			t.Fatalf("line %q", line)
		}
		if got, _ := parseToolTrailResultLine(line); got != (toolTrailResult{Call: `Read File("x") (0.9s)`, Mark: "✓"}) {
			t.Fatalf("parsed %#v", got)
		}
		if label, duration := splitToolDuration(`Read File("x") (0.9s)`); label != `Read File("x")` || duration != " (0.9s)" {
			t.Fatalf("split %q %q", label, duration)
		}
	})
}

func TestBuildVerboseToolTrailLine(t *testing.T) {
	t.Run("preserves multiline args and result details", func(t *testing.T) {
		line := buildVerboseToolTrailLine("terminal", "npm test", false, float(1.25), "{\n  \"cmd\": \"npm test\"\n}", "first line\nsecond :: line")
		if !strings.Contains(line, tr("libText.text.argsLabel")+":\n{") || !strings.Contains(line, tr("libText.text.resultLabel")+":\nfirst line\nsecond :: line") {
			t.Fatalf("line %q", line)
		}
		want := toolTrailResult{Call: `Terminal("npm test") (1.3s)`, Detail: "Args:\n{\n  \"cmd\": \"npm test\"\n}\nResult:\nfirst line\nsecond :: line", Mark: "✓"}
		if got, _ := parseToolTrailResultLine(line); got != want {
			t.Fatalf("parsed %#v", got)
		}
	})
	t.Run("labels verbose failures as errors", func(t *testing.T) {
		line := buildVerboseToolTrailLine("terminal", "npm test", true, float(0.5), "", "command failed")
		if !strings.Contains(line, "Error:\ncommand failed") || strings.Contains(line, "Result:\ncommand failed") {
			t.Fatalf("line %q", line)
		}
		want := toolTrailResult{Call: `Terminal("npm test") (0.5s)`, Detail: "Error:\ncommand failed", Mark: "✗"}
		if got, _ := parseToolTrailResultLine(line); got != want {
			t.Fatalf("parsed %#v", got)
		}
	})
	t.Run("caps a large result to a small persisted preview (#34095)", func(t *testing.T) {
		line := buildVerboseToolTrailLine("browser_snapshot", "https://x.example", false, float(2), "", strings.Repeat("A", 40_000))
		if !strings.Contains(line, "Result:\n") || unitLen(line) >= 2_000 || !strings.Contains(line, "omitted") || !strings.HasSuffix(line, " ✓") {
			t.Fatalf("line %d %q", unitLen(line), line[:80])
		}
	})
	t.Run("does not truncate a result that already fits the preview budget", func(t *testing.T) {
		small := "ok: 3 files changed"
		line := buildVerboseToolTrailLine("patch", "index.html", false, float(0.1), "", small)
		if !strings.Contains(line, "Result:\n"+small) || strings.Contains(line, "omitted") {
			t.Fatalf("line %q", line)
		}
	})
}

func TestLastCotTrailIndex(t *testing.T) {
	t.Run("finds last non-result line", func(t *testing.T) {
		if lastCotTrailIndex([]string{"a ✓", "thinking…"}) != 1 || lastCotTrailIndex([]string{"only result ✓"}) != -1 {
			t.Fatal("index")
		}
	})
}

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

func TestThinkingPreview(t *testing.T) {
	t.Run("adds paragraph breaks before markdown thinking headings", func(t *testing.T) {
		raw := "**Considering user instructions**\nI need to answer.**Planning tool execution**\nI can run tools.**Determining weather search parameters**\nUse SF."
		want := "**Considering user instructions**\nI need to answer.\n\n**Planning tool execution**\nI can run tools.\n\n**Determining weather search parameters**\nUse SF."
		if got := thinkingPreview(raw, "full", thinkingCotMax); got != want {
			t.Fatalf("got %q", got)
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
		if !strings.Contains(out, "c\nd") || !strings.Contains(out, tr("libText.text.omittedLinesChars", tr("libText.text.showingLiveTail"), "2", "4")) || strings.Contains(out, "a\nb") {
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

func TestThinkingPreviewOverBoundTail(t *testing.T) {
	t.Run("retains the live tail when reasoning exceeds the clean bound", func(t *testing.T) {
		const tail = "<<<LIVE_TAIL_MARKER>>>"
		result := thinkingPreview(strings.Repeat("A", 25_000)+"\n"+tail, "full", thinkingCotMax)
		if !strings.Contains(result, tail) || unitLen(result) > 25_000 {
			t.Fatalf("result %d", unitLen(result))
		}
	})
}

func TestPasteTokenLabel(t *testing.T) {
	t.Run("builds readable long-paste labels with counts", func(t *testing.T) {
		label := pasteTokenLabel("Vampire Bondage ropes slipped from her neck, still stained with blood", 250)
		if !strings.HasPrefix(label, "[[ ") || !strings.Contains(label, tr("libText.text.pasteLinesChip", "250")) || !strings.HasSuffix(label, " ]]") {
			t.Fatalf("label %q", label)
		}
	})
}

func TestEstimateRows(t *testing.T) {
	t.Run("handles tilde code fences", func(t *testing.T) {
		if estimateRows("~~~markdown\n# heading\n~~~", 40, false) < 2 {
			t.Fatal("rows")
		}
	})
	t.Run("handles checklist bullets as list rows", func(t *testing.T) {
		if got := estimateRows("- [x] done\n- [ ] todo", 40, false); got != 2 {
			t.Fatalf("rows %d", got)
		}
	})
	t.Run("keeps intraword underscores when sizing snake_case identifiers", func(t *testing.T) {
		if estimateRows("look at test_case_with_underscores now", 80, false) != estimateRows("look at test case with underscores now", 80, false) {
			t.Fatal("rows differ")
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
