package gateway

import (
	"reflect"
	"testing"
)

func TestSubmitCLITurnFieldsForwardsOnlyThePresentCLIFields(t *testing.T) {
	params := map[string]any{"session_id": "s", "text": "hi", "submission_id": "x1", "api_text": "voice: hi",
		"turn_author": nil, "clarify_batch": true, "surface": "cli"}
	want := map[string]any{"api_text": "voice: hi", "turn_author": nil, "clarify_batch": true}
	if got := submitCLITurnFields(params); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if got := submitCLITurnFields(map[string]any{"text": "hi"}); len(got) != 0 {
		t.Fatalf("a request without CLI fields gave %v", got)
	}
}

func TestSubmitSurfaceFieldsForwardsOnlyThePresentSurfaceFields(t *testing.T) {
	params := map[string]any{"session_id": "s", "text": "hi", "surface": "hud", "interrupted": false, "api_text": "x"}
	want := map[string]any{"surface": "hud", "interrupted": false}
	if got := submitSurfaceFields(params); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if got := submitSurfaceFields(map[string]any{"text": "hi"}); len(got) != 0 {
		t.Fatalf("a request without surface fields gave %v", got)
	}
}

func TestSessionStorePinnedReportsOnlyAnExplicitPin(t *testing.T) {
	store := &SessionStore{}
	if db, ok := store.pinned(); ok || db != nil {
		t.Fatalf("a store without a pin reported %v", db)
	}
	store.dbPinned = true
	if _, ok := store.pinned(); !ok {
		t.Fatal("an explicit pin of no database was not reported")
	}
}
