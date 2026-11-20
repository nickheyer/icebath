package tools

import (
	"strings"
	"testing"
)

func TestStripLeakedBracketedPastePlainTextUnchanged(t *testing.T) {
	if got := StripLeakedBracketedPasteWrappers("hello world"); got != "hello world" {
		t.Fatalf("got %q", got)
	}
}

func TestStripLeakedBracketedPasteKeepsNonWrapperBracketForms(t *testing.T) {
	text := "literal[200~tag and literal[201~tag should stay"
	if got := StripLeakedBracketedPasteWrappers(text); got != text {
		t.Fatalf("got %q", got)
	}
}

func TestStripLeakedBracketedPasteBoundaryForms(t *testing.T) {
	cases := map[string]string{
		"\x1b[200~pasted\x1b[201~":     "pasted",
		"^[[200~pasted^[[201~":         "pasted",
		"[200~pasted[201~":             "pasted",
		"say: [200~hi[201~.":           "say: hi.",
		"00~body01~":                   "body",
		"x　[200~y":                     "x　y",
		"keep00~mid and 01~tail stays": "keep00~mid and 01~tail stays",
	}
	for in, want := range cases {
		if got := StripLeakedBracketedPasteWrappers(in); got != want {
			t.Fatalf("%q -> %q want %q", in, got, want)
		}
	}
}

func TestCollapseRepeatedInputArtifactsIssue62557CorruptionTail(t *testing.T) {
	prefix := "需要时随时叫我。"
	tail := "[e~[[e" + strings.Repeat("~[[e", 20)
	if got := CollapseRepeatedInputArtifacts(prefix+tail, 4); got != prefix {
		t.Fatalf("got %q", got)
	}
}

func TestCollapseRepeatedInputArtifactsTrailingPunctuationPreserved(t *testing.T) {
	if got := CollapseRepeatedInputArtifacts("wait....", 4); got != "wait...." {
		t.Fatalf("got %q", got)
	}
	if got := CollapseRepeatedInputArtifacts("x"+strings.Repeat("~[[e", 3), 4); got != "x"+strings.Repeat("~[[e", 3) {
		t.Fatalf("short run collapsed %q", got)
	}
}

func TestSanitizeUserPromptTextCombinesWrapperStripAndTailCollapse(t *testing.T) {
	if got := SanitizeUserPromptText("hello[" + strings.Repeat("~[[e", 8)); got != "hello" {
		t.Fatalf("got %q", got)
	}
}
