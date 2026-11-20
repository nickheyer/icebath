package discord

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func editMessage(t *testing.T, a *DiscordAdapter, content string, finalize bool) (string, []any, bool) {
	t.Helper()
	r, _ := a.EditMessage(context.Background(), "555", "42", content, finalize)
	id, _ := r.MessageID.(string)
	return id, r.ContinuationMessageIDs, r.Success
}

func TestEditMessageHappyPathShortEditInPlace(t *testing.T) {
	a, api := sendAdapter(t)
	id, cont, ok := editMessage(t, a, "short reply", true)
	if !ok || id != "42" || len(cont) != 0 || !slices.Equal(api.editContents("555", "42"), []string{"short reply"}) || len(api.sends("555")) != 0 {
		t.Fatalf("id %s cont %v", id, cont)
	}
}

func TestMidStreamOverflowTruncatesOversizedStreamingEditTruncatesInPlace(t *testing.T) {
	a, api := sendAdapter(t)
	id, cont, ok := editMessage(t, a, strings.Repeat("p", 6000), false)
	edits := api.editContents("555", "42")
	if !ok || id != "42" || len(cont) != 0 || len(api.sends("555")) != 0 || len(edits) != 1 ||
		utf8.RuneCountInString(edits[0]) > discordMaxMessageLength || strings.HasSuffix(edits[0], "...") {
		t.Fatalf("edits %d", len(edits))
	}
}

func TestSaturatedPreviewDedupSaturatedPreviewDedupsRepeatOversizedEdits(t *testing.T) {
	a, api := sendAdapter(t)
	count := func() int { return len(api.editContents("555", "42")) }
	if _, _, ok := editMessage(t, a, strings.Repeat("x", 2500), false); !ok || count() != 1 {
		t.Fatal("first saturated edit")
	}
	for _, grow := range []int{3000, 3500} {
		if id, _, ok := editMessage(t, a, strings.Repeat("x", grow), false); !ok || id != "42" {
			t.Fatal("dedup result")
		}
	}
	if count() != 1 {
		t.Fatal("identical saturated previews must not be re-sent")
	}
	editMessage(t, a, strings.Repeat("x", 4000), false)
	if count() != 2 {
		t.Fatalf("edits %d", count())
	}
	editMessage(t, a, strings.Repeat("x", 4500), false)
	if count() != 2 {
		t.Fatalf("edits %d", count())
	}
	if _, _, ok := editMessage(t, a, strings.Repeat("x", 4500), true); !ok || count() != 3 {
		t.Fatalf("final edits %d", count())
	}
}

func TestFinalOverflowSplitsOversizedFinalEditSplitsAndDelivers(t *testing.T) {
	a, api := sendAdapter(t)
	id, cont, ok := editMessage(t, a, strings.Repeat("q", 6000), true)
	edits, sends := api.editContents("555", "42"), api.sentContents("555")
	if !ok || len(cont) == 0 || id != cont[len(cont)-1] || len(edits) != 1 || len(sends) != len(cont) {
		t.Fatalf("id %s cont %v edits %d sends %d", id, cont, len(edits), len(sends))
	}
	for _, c := range append(edits, sends...) {
		if utf8.RuneCountInString(c) > discordMaxMessageLength || strings.HasSuffix(c, "...") {
			t.Fatal("chunk over cap or ellipsized")
		}
	}
}

func TestFinalOverflowSplitsByteCoveragePreserved(t *testing.T) {
	a, api := sendAdapter(t)
	if _, _, ok := editMessage(t, a, strings.Repeat("a", 5000)+"END_MARKER_XYZ", true); !ok {
		t.Fatal("edit failed")
	}
	delivered := strings.Join(append(api.editContents("555", "42"), api.sentContents("555")...), "")
	if !strings.Contains(delivered, "END_MARKER_XYZ") {
		t.Fatal("tail lost")
	}
}

func TestReactiveOverflowDetection50035OnFinalEditTriggersSplit(t *testing.T) {
	a, api := sendAdapter(t)
	api.mu.Lock()
	api.fail = func(c fakeCall, n int) (int, string) {
		if c.Method == http.MethodPatch && len(api.matching(http.MethodPatch, "/messages/42")) == 1 {
			return 400, `{"message":"Invalid Form Body","code":50035,"errors":{"content":{"_errors":[{"code":"BASE_TYPE_MAX_LENGTH","message":"Must be 2000 or fewer in length."}]}}}`
		}
		return 0, ""
	}
	api.mu.Unlock()
	if _, _, ok := editMessage(t, a, strings.Repeat("u", 1500), true); !ok || len(api.editContents("555", "42")) < 1 {
		t.Fatal("reactive split failed")
	}
}

func TestLengthOverflowDetectorIgnoresNonLength50035(t *testing.T) {
	if isLengthOverflowError(errors.New("error code: 50035: Cannot reply to a system message")) {
		t.Fatal("non length 50035 treated as overflow")
	}
}

func TestPartialMessageContinuationReferencesContinuationsThreadedWithIdsBuiltReference(t *testing.T) {
	a, api := sendAdapter(t)
	if _, _, ok := editMessage(t, a, strings.Repeat("chunk alpha ", 600), true); !ok {
		t.Fatal("edit failed")
	}
	sends := api.sends("555")
	if len(sends) < 1 {
		t.Fatal("overflow should send continuations")
	}
	for _, s := range sends {
		if s["message_reference"] == nil {
			t.Fatal("continuation lost its reply reference")
		}
	}
}
