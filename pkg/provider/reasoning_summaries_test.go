package provider

import (
	"reflect"
	"strings"
	"testing"
)

// Accumulates deltas the way the chat completions stream loop does
func streamReasoningSummaries(deltas []string) string {
	var parts []string
	for _, delta := range deltas {
		previous := ""
		if len(parts) > 0 {
			previous = parts[len(parts)-1]
		}
		parts = append(parts, separateGluedReasoningBlocks(previous, delta))
	}
	return strings.Join(parts, "")
}

func TestHeadingOnlyPartsDoNotGlueIntoOneRun(t *testing.T) {
	text := streamReasoningSummaries([]string{
		"**Investigating likely culprit PRs**",
		"**Inspecting message schema and tool_calls content**",
		"**Analyzing interrupted tool call impact**",
	})
	if strings.Contains(text, "****") {
		t.Fatalf("bold run glued: %q", text)
	}
	want := []string{"**Investigating likely culprit PRs**", "", "**Inspecting message schema and tool_calls content**", "", "**Analyzing interrupted tool call impact**"}
	if got := strings.Split(text, "\n"); !reflect.DeepEqual(got, want) {
		t.Fatalf("lines %q", got)
	}
}

func TestProseBodyDoesNotGlueOntoTheNextHeading(t *testing.T) {
	text := streamReasoningSummaries([]string{
		"**Simulating a greeting stream**\n\nIt feels like a streaming interaction!",
		"**Simulating a greeting stream**\n\nI want to meet the request.",
	})
	if strings.Contains(text, "interaction!**") || !strings.Contains(text, "interaction!\n\n**Simulating") {
		t.Fatalf("prose glued onto heading: %q", text)
	}
}

func TestTokenStreamedReasoningIsUntouched(t *testing.T) {
	deltas := []string{"Looking at", " the session", " logs, I see", " one bold word."}
	if got := streamReasoningSummaries(deltas); got != strings.Join(deltas, "") {
		t.Fatalf("token stream rewritten: %q", got)
	}
}

func TestBoldWordMidSentenceIsNotABoundary(t *testing.T) {
	if got := separateGluedReasoningBlocks("I see the ", "**signature**"); got != "**signature**" {
		t.Fatalf("%q", got)
	}
}

func TestUnclosedEmphasisFragmentIsNotABoundary(t *testing.T) {
	if got := separateGluedReasoningBlocks("weighing", "**"); got != "**" {
		t.Fatalf("%q", got)
	}
}

func TestBoundaryNeedsABoldOpener(t *testing.T) {
	if got := separateGluedReasoningBlocks("**Closing**", "plain head"); got != "plain head" {
		t.Fatalf("%q", got)
	}
}

func TestEmptyOperandsPassThrough(t *testing.T) {
	if got := separateGluedReasoningBlocks("", "**first**"); got != "**first**" {
		t.Fatalf("%q", got)
	}
	if got := separateGluedReasoningBlocks("**first**", ""); got != "" {
		t.Fatalf("%q", got)
	}
}
