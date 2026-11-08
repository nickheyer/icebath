package agent

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func TestCloseTranscriptTailAppendsAnAssistantRowForADeliveredFinal(t *testing.T) {
	a := &AIAgent{}
	msgs := []*state.Message{{Role: "user", Content: "q"}, {Role: "tool", ToolCallID: "c1", Content: "out"}}
	closeTranscriptTail(a, &msgs, "answer", false, false)
	if len(msgs) != 3 || msgs[2].Role != "assistant" || msgs[2].Content != "answer" {
		t.Fatalf("tail = %+v", msgs[len(msgs)-1])
	}
}

func TestCloseTranscriptTailFillsABlankToolCallTailInPlace(t *testing.T) {
	a := &AIAgent{dbFlushScanPrefix: 3}
	tail := &state.Message{Role: "assistant", Content: "", ToolCalls: []state.ToolCall{{ID: "c1"}}}
	msgs := []*state.Message{{Role: "user", Content: "q"}, tail}
	closeTranscriptTail(a, &msgs, "answer", false, false)
	if len(msgs) != 2 || msgs[1] != tail || tail.Content != "answer" || tail.Timestamp == 0 || a.dbFlushScanPrefix != nil {
		t.Fatalf("msgs = %d, tail = %+v, cursor = %v", len(msgs), tail, a.dbFlushScanPrefix)
	}
}

func TestCloseTranscriptTailLeavesAMatchingAssistantTailAlone(t *testing.T) {
	a := &AIAgent{}
	msgs := []*state.Message{{Role: "user", Content: "q"}, {Role: "assistant", Content: "answer"}}
	closeTranscriptTail(a, &msgs, "answer", false, false)
	if len(msgs) != 2 {
		t.Fatalf("matching tail duplicated, %d rows", len(msgs))
	}
}

func TestStoreRewriteOnTailPublishesTheHeldRewrite(t *testing.T) {
	a := &AIAgent{holdStream: true}
	tail := &state.Message{Role: "assistant", Content: "raw", DisplayKind: "hidden"}
	storeRewriteOnTail(a, []*state.Message{tail}, "raw", "REWRITE")
	if tail.Content != "REWRITE" || tail.DisplayKind != "" || tail.APIContent != "raw" {
		t.Fatalf("tail = %+v", tail)
	}
	other := &state.Message{Role: "assistant", Content: "different"}
	storeRewriteOnTail(a, []*state.Message{other}, "raw", "REWRITE")
	if other.Content != "different" {
		t.Fatalf("a tail without the raw text was rewritten: %+v", other)
	}
}

func TestRecoverFinalFromStreamUsesDeliveredText(t *testing.T) {
	a := &AIAgent{}
	a.setCurrentStreamedAssistantText("  streamed reply  ")
	if got, ok := recoverFinalFromStream(a, "", false, false); got != "streamed reply" || !ok {
		t.Fatalf("recovered = %v %v", got, ok)
	}
	if got, ok := recoverFinalFromStream(a, nil, false, true); got != nil || ok {
		t.Fatalf("failed turn recovered %v", got)
	}
	if got, ok := recoverFinalFromStream(a, "model text", false, false); got != "model text" || ok {
		t.Fatalf("visible final replaced by %v", got)
	}
}

func TestLastTurnReasoningStopsAtTheTurnBoundary(t *testing.T) {
	msgs := []*state.Message{{Role: "assistant", Reasoning: "old"}, {Role: "user"}, {Role: "assistant", Reasoning: "tool step"}, {Role: "tool"}, {Role: "assistant"}}
	if got := lastTurnReasoning(msgs); got != "tool step" {
		t.Fatalf("reasoning = %v", got)
	}
	if got := lastTurnReasoning(msgs[:2]); got != nil {
		t.Fatalf("crossed the turn boundary, got %v", got)
	}
}

func TestDropVerificationContinuationScaffoldingKeepsTheCandidate(t *testing.T) {
	msgs := []*state.Message{
		{Role: "assistant", Content: "candidate"},
		{Role: "user", Content: "verify", Extra: map[string]any{"_verification_stop_synthetic": true}},
		{Role: "user", Content: "pre", Extra: map[string]any{"_pre_verify_synthetic": true}},
	}
	dropVerificationContinuationScaffolding(&msgs)
	if len(msgs) != 1 || msgs[0].Content != "candidate" {
		t.Fatalf("msgs = %+v", msgs)
	}
}

func TestAbsorbContinuationFragmentsJoinsAndUnflags(t *testing.T) {
	frag := &state.Message{Role: "assistant", Extra: map[string]any{"_length_continuation_fragment": true}}
	got, joined := absorbContinuationFragments("tail", []TruncatedPart{{Text: "head "}}, []*state.Message{frag})
	if !joined || got != joinTruncatedParts([]TruncatedPart{{Text: "head "}, {Text: "tail"}}) {
		t.Fatalf("joined = %v %q", joined, got)
	}
	if _, ok := frag.Extra["_length_continuation_fragment"]; ok {
		t.Fatal("fragment row still flagged")
	}
	if got, joined := absorbContinuationFragments("only", nil, nil); joined || got != "only" {
		t.Fatalf("no parts changed the final to %q", got)
	}
}
