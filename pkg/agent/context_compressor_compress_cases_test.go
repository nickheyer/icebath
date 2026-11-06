package agent

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func roleRows(roles ...string) []map[string]any {
	out := make([]map[string]any, 0, len(roles))
	for i, r := range roles {
		out = append(out, map[string]any{"role": r, "content": "msg " + strconv.Itoa(i)})
	}
	return out
}

func compressWithSummary(t *testing.T, c *ContextCompressor, msgs []map[string]any, summary string) []map[string]any {
	t.Helper()
	patchCallLLM(t, func(map[string]any) (any, error) { return contentResponse(summary), nil })
	out, err := c.Compress(msgs, nil, nil, false, "")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func testModelCompressor(t *testing.T, first, last int) *ContextCompressor {
	return newCompressorWith(t, 100000, func(o *ContextCompressorOptions) { o.Model, o.ProtectFirstN, o.ProtectLastN = "test", first, last })
}

func strContent(m map[string]any) string {
	s, _ := m["content"].(string)
	return s
}

func summaryRow(t *testing.T, result []map[string]any) map[string]any {
	t.Helper()
	for _, m := range result {
		if strings.HasPrefix(strContent(m), state.SummaryPrefix) {
			return m
		}
	}
	t.Fatalf("no summary row in %v", result)
	return nil
}

func TestUserRoleSummaryCarriesEndMarker(t *testing.T) {
	c := testModelCompressor(t, 2, 2)
	result := compressWithSummary(t, c, roleRows("user", "assistant", "user", "assistant", "user", "assistant", "user", "assistant"), "summary text")
	m := summaryRow(t, result)
	if m["role"] != "user" || !strings.HasSuffix(strings.TrimRight(strContent(m), " \t\n\r"), strings.TrimSpace(state.SummaryEndMarker)) {
		t.Fatalf("got %v", m)
	}
}

func TestAssistantRoleSummaryCarriesEndMarker(t *testing.T) {
	c := testModelCompressor(t, 2, 2)
	msgs := roleRows("system", "user", "user", "assistant", "user", "user", "assistant", "user")
	m := summaryRow(t, compressWithSummary(t, c, msgs, "[CONTEXT SUMMARY]: stuff happened"))
	if m["role"] != "assistant" || !strings.HasSuffix(strings.TrimRight(strContent(m), " \t\n\r"), strings.TrimSpace(state.SummaryEndMarker)) {
		t.Fatalf("got %v", m)
	}
}

func assertAlternates(t *testing.T, result []map[string]any) {
	t.Helper()
	for i := 1; i < len(result); i++ {
		a, b := result[i-1]["role"], result[i]["role"]
		if (a == "user" || a == "assistant") && (b == "user" || b == "assistant") && a == b {
			t.Fatalf("consecutive %v at %d,%d", a, i-1, i)
		}
	}
}

func TestDoubleCollisionMergesSummaryIntoTail(t *testing.T) {
	c := testModelCompressor(t, 2, 3)
	msgs := roleRows("system", "user", "assistant", "user", "assistant", "user", "user", "assistant", "user")
	result := compressWithSummary(t, c, msgs, "summary text")
	assertAlternates(t, result)
	var tail []map[string]any
	for _, m := range result {
		if strings.Contains(strContent(m), "msg 6") {
			tail = append(tail, m)
		}
	}
	if len(tail) != 1 || !strings.Contains(strContent(tail[0]), "summary text") {
		t.Fatalf("got %v", tail)
	}
}

func TestMergeIntoTailEndMarkerIsLast(t *testing.T) {
	c := testModelCompressor(t, 2, 3)
	msgs := roleRows("system", "user", "assistant", "user", "assistant", "user", "user", "assistant", "user")
	msgs[6]["content"] = []any{map[string]any{"type": "text", "text": "PRESERVED_TAIL_CONTENT"}}
	result := compressWithSummary(t, c, msgs, "SUMMARY_BODY")
	var text string
	found := false
	for _, m := range result {
		if flag, _ := m[state.CompressedSummaryMetadataKey].(bool); !flag {
			continue
		}
		found = true
		switch v := m["content"].(type) {
		case string:
			text = v
		case []any:
			var parts []string
			for _, b := range v {
				if d, ok := b.(map[string]any); ok {
					s, _ := d["text"].(string)
					parts = append(parts, s)
				}
			}
			text = strings.Join(parts, " ")
		}
		break
	}
	end := strings.TrimSpace(state.SummaryEndMarker)
	if !found || !strings.Contains(text, "PRESERVED_TAIL_CONTENT") || !strings.Contains(text, "SUMMARY_BODY") || !strings.Contains(text, end) {
		t.Fatalf("got %q", text)
	}
	if strings.Index(text, "PRESERVED_TAIL_CONTENT") >= strings.Index(text, "SUMMARY_BODY") || strings.Index(text, "SUMMARY_BODY") >= strings.Index(text, end) ||
		!strings.HasSuffix(strings.TrimRight(text, " \t\n\r"), end) {
		t.Fatalf("order broken %q", text)
	}
}

func TestSmallConversationStillCompresses(t *testing.T) {
	c := newCompressorWith(t, 200_000, func(o *ContextCompressorOptions) {
		o.ThresholdPercent, o.ProtectFirstN, o.ProtectLastN = 0.50, 2, 20
	})
	var messages []map[string]any
	for i := 0; i < 9; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		messages = append(messages, map[string]any{"role": role, "content": "Message " + strconv.Itoa(i)})
	}
	prev := compressorGenerateSummary
	compressorGenerateSummary = func(*ContextCompressor, context.Context, []map[string]any, *string, string, bool) (*string, error) {
		s := "Summary of conversation"
		return &s, nil
	}
	t.Cleanup(func() { compressorGenerateSummary = prev })
	tokens := 90_000
	result, err := c.Compress(messages, &tokens, nil, false, "")
	if err != nil || len(result) >= len(messages) {
		t.Fatalf("got %d rows err %v", len(result), err)
	}
}

func nonSystem(result []map[string]any) []map[string]any {
	var out []map[string]any
	for _, m := range result {
		if m["role"] != "system" {
			out = append(out, m)
		}
	}
	return out
}

func TestDoubleCompactionSummaryRole(t *testing.T) {
	t.Run("summary_must_be_user_when_only_system_protected", func(t *testing.T) {
		c := testModelCompressor(t, 2, 2)
		c.SetCompressionCount(1)
		msgs := roleRows("system", "user", "assistant", "user", "assistant", "user", "assistant")
		msgs[0]["content"] = "You are a helpful assistant."
		result := compressWithSummary(t, c, msgs, "summary of earlier turns")
		ns := nonSystem(result)
		if result[0]["role"] != "system" || len(ns) == 0 || ns[0]["role"] != "user" {
			t.Fatalf("got %v", result)
		}
	})
	t.Run("restart_handoff_without_system_still_starts_with_user", func(t *testing.T) {
		c := testModelCompressor(t, 3, 2)
		msgs := []map[string]any{
			{"role": "user", "content": state.SummaryPrefix + "\nold persisted summary"},
			{"role": "assistant", "content": "handoff acknowledged"},
			{"role": "user", "content": "new work after restart"},
			{"role": "assistant", "content": "new answer after restart"},
			{"role": "user", "content": "more new work after restart"},
			{"role": "assistant", "content": "more new answer after restart"},
			{"role": "user", "content": "tail request"},
			{"role": "assistant", "content": "tail answer"},
		}
		result := compressWithSummary(t, c, msgs, "summary of resumed turns")
		if result[0]["role"] != "user" || !strings.Contains(strContent(result[0]), "summary of resumed turns") {
			t.Fatalf("got %v", result[0])
		}
	})
	t.Run("user_tail_merges_into_tail", func(t *testing.T) {
		c := testModelCompressor(t, 2, 2)
		c.SetCompressionCount(1)
		msgs := roleRows("system", "user", "assistant", "user", "assistant", "user", "assistant", "user")
		msgs[0]["content"] = "You are a helpful assistant."
		result := compressWithSummary(t, c, msgs, "summary of earlier turns")
		carried := false
		for _, m := range result {
			if flag, _ := m[state.CompressedSummaryMetadataKey].(bool); flag && !strings.Contains(strContent(m), "msg 5") {
				t.Fatalf("standalone summary %v", m)
			}
			carried = carried || strings.Contains(strContent(m), "summary of earlier turns")
		}
		if ns := nonSystem(result); ns[0]["role"] != "user" || !carried {
			t.Fatalf("got %v", result)
		}
	})
}

func lastPrompt(t *testing.T, calls []map[string]any) string {
	t.Helper()
	msgs, _ := calls[len(calls)-1]["messages"].([]map[string]any)
	if len(msgs) == 0 {
		if anyMsgs, ok := calls[len(calls)-1]["messages"].([]any); ok && len(anyMsgs) > 0 {
			m, _ := anyMsgs[0].(map[string]any)
			return strContent(m)
		}
		t.Fatalf("no prompt in %v", calls[len(calls)-1])
	}
	return strContent(msgs[0])
}

func TestLeanSamplingKeepsRecordBoundaries(t *testing.T) {
	c := newCompressorWith(t, 100000, func(o *ContextCompressorOptions) {
		o.ThresholdPercent, o.ProtectFirstN, o.ProtectLastN, o.TailMode = 0.85, 2, 2, "lean"
	})
	body := "para1\n\npara2 " + strings.Repeat("x", 1200)
	var messages []map[string]any
	for i := 0; i < 1200; i++ {
		messages = append(messages, map[string]any{"role": "user", "content": "record-" + leftPad4(i) + " " + body})
	}
	var calls []map[string]any
	patchCallLLM(t, func(kw map[string]any) (any, error) {
		calls = append(calls, kw)
		return contentResponse("## Historical Task Snapshot\nDone."), nil
	})
	telemetry := c.CompressionTelemetry.Begin(TelemetryAttempt{CurrentTokens: 1_000_000})
	if s, err := c.generateSummary(context.Background(), messages, nil, "", false); err != nil || s == nil {
		t.Fatalf("no summary %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("calls %d", len(calls))
	}
	prompt := lastPrompt(t, calls)
	if !strings.Contains(prompt, "chars elided") {
		t.Fatal("no elision")
	}
	transcript := prompt[strings.Index(prompt, "[USER]: record-"):]
	for _, sec := range elisionMarkerRE.Split(transcript, -1) {
		if strings.Contains(sec, "record-") && !strings.HasPrefix(sec, "[USER]: record-") {
			t.Fatalf("section starts mid record %q", sec[:60])
		}
	}
	whole := regexp.MustCompile(`\[USER\]: record-\d{4} `+regexp.QuoteMeta(body)+`(\n\n|\n*$)`).FindAllStringSubmatch(transcript, -1)
	var kept []string
	sampled := 0
	for _, w := range whole {
		r := strings.TrimSuffix(w[0], w[1])
		kept = append(kept, r)
		sampled += len([]rune(r))
	}
	if len(kept) != strings.Count(transcript, "[USER]: record-") || len(kept) <= 8 ||
		!strings.HasPrefix(kept[len(kept)-1], "[USER]: record-1199 ") || !strings.HasPrefix(kept[0], "[USER]: record-0000 ") {
		t.Fatalf("records %d of %d", len(kept), strings.Count(transcript, "[USER]: record-"))
	}
	get := func(k string) int64 {
		v, _ := telemetry.Get(k)
		n, _ := v.(int64)
		if i, ok := v.(int); ok {
			n = int64(i)
		}
		return n
	}
	if get("summary_input_record_count") != 1200 || get("summary_input_sampled_record_count") != int64(len(kept)) ||
		get("summary_input_elided_record_count") != int64(1200-len(kept)) || get("summary_input_sampled_chars") != int64(sampled) ||
		get("summary_input_chars") != get("summary_input_sampled_chars")+get("summary_input_omitted_chars") {
		t.Fatalf("telemetry %v", telemetry)
	}
}

func leftPad4(i int) string {
	s := strconv.Itoa(i)
	return strings.Repeat("0", 4-len(s)) + s
}

func TestIterativeUpdatePathIsBounded(t *testing.T) {
	c := newCompressorWith(t, 272000, func(o *ContextCompressorOptions) { o.Model = "test" })
	prev := "PREV_HEAD " + strings.Repeat("p", summaryInputMaxChars*2) + " PREV_TAIL"
	c.previousSummary = &prev
	var messages []map[string]any
	for i := 0; i < 80; i++ {
		messages = append(messages, map[string]any{"role": "user", "content": "turn-" + strconv.Itoa(i) + "-" + strings.Repeat("x", 6000)})
	}
	var calls []map[string]any
	patchCallLLM(t, func(kw map[string]any) (any, error) {
		calls = append(calls, kw)
		return contentResponse("updated summary"), nil
	})
	summary, err := c.generateSummary(context.Background(), messages, nil, "", false)
	if err != nil || summary == nil || !strings.HasPrefix(*summary, state.SummaryPrefix) {
		t.Fatalf("summary %v %v", summary, err)
	}
	prompt := lastPrompt(t, calls)
	if len([]rune(prompt)) >= 2*summaryInputMaxChars+30_000 || !strings.Contains(prompt, "PREV_HEAD") || !strings.Contains(prompt, "PREV_TAIL") {
		t.Fatalf("prompt %d chars", len([]rune(prompt)))
	}
}
