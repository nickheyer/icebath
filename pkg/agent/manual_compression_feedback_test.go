package agent

import (
	"strconv"
	"strings"
	"testing"
)

func feedbackMessages(count int) []map[string]any {
	out := make([]map[string]any, count)
	for i := range out {
		role := "assistant"
		if i%2 == 0 {
			role = "user"
		}
		out[i] = map[string]any{"role": role, "content": strconv.Itoa(i)}
	}
	return out
}

func TestFailureReasonRedactionIsForcedAtUIBoundary(t *testing.T) {
	t.Setenv("ICEBATH_REDACT_SECRETS", "false")
	messages := feedbackMessages(12)
	secret := "sk-proj-" + strings.Repeat("X", 40)
	fb := SummarizeManualCompression(messages, append([]map[string]any(nil), messages...), 120_000, 120_000,
		&ManualCompressionState{LastCompressAborted: true, LastSummaryError: "provider rejected OPENAI_API_KEY=" + secret})
	if strings.Contains(*fb.Note, secret) || !strings.Contains(*fb.Note, "OPENAI_API_KEY=") {
		t.Fatalf("note %q", *fb.Note)
	}
	if fb.Headline != "Compression aborted: 12 messages preserved" || fb.TokenLine != "Approx request size: ~120,000 tokens (unchanged)" || !fb.Noop {
		t.Fatalf("aborted feedback %+v", fb)
	}
}

func TestFallbackCompressionReportsDroppedMessageCount(t *testing.T) {
	before := feedbackMessages(12)
	after := append(append([]map[string]any(nil), before[:2]...), before[10:]...)
	dropped := 8
	fb := SummarizeManualCompression(before, after, 120_000, 40_000, &ManualCompressionState{LastSummaryFallbackUsed: true,
		LastSummaryDroppedCount: &dropped, LastSummaryError: "summary provider returned an invalid response"})
	if fb.Aborted || !fb.FallbackUsed || fb.Headline != "Compressed with fallback: 12 → 4 messages" {
		t.Fatalf("fallback feedback %+v", fb)
	}
	if *fb.Note != "Summary generation failed; Icebath used limited fallback context and removed 8 message(s). Reason: summary provider returned an invalid response" {
		t.Fatalf("note %q", *fb.Note)
	}
	if fb.TokenLine != "Approx request size: ~120,000 → ~40,000 tokens" {
		t.Fatal(fb.TokenLine)
	}
	if fb := SummarizeManualCompression(before, after, 1, 1, &ManualCompressionState{LastSummaryFallbackUsed: true}); !strings.Contains(*fb.Note, "removed 8 message(s).") {
		t.Fatal("dropped count falls back to the message delta")
	}
}

func refusalMessages(n int) []map[string]any {
	out := make([]map[string]any, n)
	for i := range out {
		out[i] = map[string]any{"role": "user", "content": "m" + strconv.Itoa(i)}
	}
	return out
}

func TestWouldGrowRefusalFeedback(t *testing.T) {
	fb := SummarizeManualCompression(refusalMessages(8), refusalMessages(14), 22025, 22453, &ManualCompressionState{LastCompressRefusedWouldGrow: true})
	if !fb.RefusedWouldGrow || !strings.Contains(strings.ToLower(fb.Headline), "refused") || strings.Contains(fb.Headline, "→") ||
		!strings.Contains(fb.TokenLine, "unchanged") || !strings.Contains(*fb.Note, "no messages were removed") {
		t.Fatalf("refusal %+v", fb)
	}
	fb = SummarizeManualCompression(refusalMessages(10), refusalMessages(4), 30000, 12000, &ManualCompressionState{})
	if fb.RefusedWouldGrow || !strings.HasPrefix(fb.Headline, "Compressed: 10 → 4") || fb.Note != nil {
		t.Fatalf("normal %+v", fb)
	}
	fb = SummarizeManualCompression(refusalMessages(6), refusalMessages(6), 9000, 9000, &ManualCompressionState{LastCompressAborted: true})
	if !fb.Aborted || !strings.HasPrefix(fb.Headline, "Compression aborted") {
		t.Fatalf("abort %+v", fb)
	}
	fb = SummarizeManualCompression(refusalMessages(6), refusalMessages(4), 100, 200, nil)
	if fb.Note == nil || !strings.HasPrefix(*fb.Note, "Note: fewer messages") {
		t.Fatal("denser summary note")
	}
	if fb := SummarizeManualCompression(refusalMessages(3), refusalMessages(3), 5, 5, nil); fb.Headline != "No changes from compression: 3 messages" {
		t.Fatal(fb.Headline)
	}
}

func TestDescribeCompressionLockSkip(t *testing.T) {
	if got := DescribeCompressionLockSkip("pid 42"); got != "⏳ Compression already in progress for this session (holder: pid 42). Please wait for it to finish." {
		t.Fatal(got)
	}
	for _, sig := range []any{true, nil, "  "} {
		if !strings.HasPrefix(DescribeCompressionLockSkip(sig), "⏳ Compression skipped: could not acquire") {
			t.Fatal(sig)
		}
	}
}
