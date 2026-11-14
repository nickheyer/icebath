package state

import (
	"reflect"
	"testing"
)

// Merged string, delimiter part list, force user leading list and non list content each project as hermes does
func TestHandoffOnlyContentProjectsSummaryToTheHandoff(t *testing.T) {
	summary := "summary body\n" + SummaryEndMarker
	if got := HandoffOnlyContent("older turn " + MergedSummaryDelimiter + "  " + summary + "\nlive ask"); got != summary {
		t.Fatalf("merged string projected to %q", got)
	}
	if got := HandoffOnlyContent(summary + "\nlive ask"); got != summary {
		t.Fatalf("plain string projected to %q", got)
	}
	merged := []any{
		map[string]any{"type": "text", "text": "prior " + MergedSummaryDelimiter + "\n" + summary + " tail"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA"}},
	}
	want := []any{map[string]any{"type": "text", "text": summary}}
	if got := HandoffOnlyContent(merged); !reflect.DeepEqual(got, want) {
		t.Fatalf("delimiter part list projected to %#v", got)
	}
	empty := []any{map[string]any{"type": "text", "text": "prior " + MergedSummaryDelimiter + "   "}}
	if got := HandoffOnlyContent(empty); !reflect.DeepEqual(got, []any{}) {
		t.Fatalf("empty suffix projected to %#v", got)
	}
	leading := []any{
		map[string]any{"type": "text", "text": "head"},
		map[string]any{"type": "text", "text": "rest " + SummaryEndMarker + " live ask"},
		map[string]any{"type": "text", "text": "after"},
	}
	wantLeading := []any{map[string]any{"type": "text", "text": "head"}, map[string]any{"type": "text", "text": "rest " + SummaryEndMarker}}
	if got := HandoffOnlyContent(leading); !reflect.DeepEqual(got, wantLeading) {
		t.Fatalf("force user leading list projected to %#v", got)
	}
	if got := HandoffOnlyContent(7); got != 7 {
		t.Fatalf("non list content changed to %#v", got)
	}
}
