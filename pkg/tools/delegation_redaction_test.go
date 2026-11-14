package tools

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Structured summaries keep valid JSON, plain ones get the text pass and parent copies yield their projection
func TestRedactSummaryProjectsStructuredPlainAndParentCopySummaries(t *testing.T) {
	answer := `{"env": "OPENAI_API_KEY=sk-proj-` + strings.Repeat("a", 40) + `", "n": 2}`
	structured, ok := RedactSummary(nil, "Here:\n"+answer+"\nDone.", true, true).(string)
	if !ok || strings.Contains(structured, strings.Repeat("a", 40)) {
		t.Fatalf("structured %v", structured)
	}
	parsed, err := pyjson.Loads(ExtractJSONCandidate(structured))
	if err != nil {
		t.Fatalf("structured summary no longer parses %v", err)
	}
	if n, _ := parsed.(*pyjson.Object).Get("n"); !pyrepr.Equal(n, 2) {
		t.Fatalf("n = %v", n)
	}
	if plain := RedactSummary(nil, delegationSecret, false, true); plain == delegationSecret {
		t.Fatal("plain summary kept the secret")
	}
	carrier := NewParentCopy("parent text", "forced text")
	if RedactSummary(nil, carrier, true, true) != "forced text" || RedactSummary(nil, carrier, true, false) != carrier {
		t.Fatal("parent copy projections")
	}
	if RedactSummary(nil, nil, true, true) != nil {
		t.Fatal("absent summary changed")
	}
}

func TestRedactChildValueRedactsStringMapsPerValue(t *testing.T) {
	secret := "sk-proj-" + strings.Repeat("a", 40)
	got := RedactChildValue(nil, map[string]string{"0": "tail with OPENAI_API_KEY=" + secret + " inside", "1": "plain tail"}, true)
	m, ok := got.(map[string]any)
	if !ok || len(m) != 2 || m["1"] != "plain tail" {
		t.Fatalf("got %#v", got)
	}
	if first, _ := m["0"].(string); strings.Contains(first, secret) || !strings.Contains(first, "tail with") {
		t.Fatalf("value not redacted %q", first)
	}
}
