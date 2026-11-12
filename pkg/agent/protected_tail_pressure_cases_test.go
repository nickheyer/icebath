package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

// One read_file call and its unique tool output of chars characters
func uniqueToolPair(i, chars int) []map[string]any {
	payload := strings.Repeat(fmt.Sprintf("line %d unique payload ", i), chars/22)
	body := fmt.Sprintf("FILE_%d_START\n", i) + payload[:min(len(payload), chars)]
	return []map[string]any{
		{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": fmt.Sprintf("call_%d", i), "type": "function",
			"function": map[string]any{"name": "read_file", "arguments": fmt.Sprintf(`{"path":"f%d.py"}`, i)}}}},
		{"role": "tool", "tool_call_id": fmt.Sprintf("call_%d", i), "content": body},
	}
}

// Session already compacted once whose tail is nPairs huge tool rounds and a huge user report
func alreadyCompactedSession(nPairs, toolChars, userChars int) []map[string]any {
	msgs := []map[string]any{
		{"role": "system", "content": "You are Hermes."},
		{"role": "user", "content": "Investigate thoroughly"},
		{"role": "assistant", "content": "OK"},
		{"role": "user", "content": "[CONTEXT COMPACTION — REFERENCE ONLY]\n" + strings.Repeat("Prior findings. ", 200)},
		{"role": "assistant", "content": "Continuing from compacted context."},
	}
	for i := range nPairs {
		msgs = append(msgs, uniqueToolPair(i, toolChars)...)
	}
	return append(msgs, map[string]any{"role": "user", "content": "Full structured report:\n" + strings.Repeat("U", userChars)})
}

func roughTokens(rows []map[string]any) int {
	return provider.EstimateMessagesTokens(rowsToMessages(rows), provider.DefaultImageTokenCost, true)
}

func TestProtectedTailPressure61932(t *testing.T) {
	caseHome(t)
	t.Run("test_compress_escapes_cannot_compress_further_dead_end", func(t *testing.T) {
		patchCallLLM(t, func(map[string]any) (any, error) {
			return contentResponse("compact summary of earlier investigation"), nil
		})
		window := 128_000
		c := newCompressorWith(t, 128_000, func(o *ContextCompressorOptions) {
			o.Model, o.ThresholdPercent, o.SummaryTargetRatio, o.ProtectFirstN, o.ProtectLastN, o.ConfigContextLength = "openai-codex/gpt-test", 0.50, 0.20, 3, 20, &window
		})
		msgs := alreadyCompactedSession(4, 200_000, 80_000)
		rough0 := roughTokens(msgs)
		if rough0 <= c.ContextLength() {
			t.Fatalf("session %d not over the %d window", rough0, c.ContextLength())
		}
		cur, tok, lastProgress := msgs, rough0, false
		for range 3 {
			oLen, oTok := len(cur), tok
			out, err := c.Compress(append([]map[string]any{}, cur...), &tok, nil, false, "")
			if err != nil {
				t.Fatal(err)
			}
			nTok := roughTokens(out)
			lastProgress = compressionMadeProgress(oLen, len(out), oTok, nTok)
			cur, tok = out, nTok
			if nTok < c.Base().ThresholdTokens && nTok < c.ContextLength() {
				break
			}
		}
		if tok >= c.ContextLength() {
			t.Fatalf("still over context after compression: %d >= %d", tok, c.ContextLength())
		}
		if float64(tok) >= float64(rough0)*0.5 {
			t.Fatalf("compression did not reclaim enough headroom: %d -> %d", rough0, tok)
		}
		if !(tok < c.Base().ThresholdTokens || lastProgress) {
			t.Fatalf("tokens %d threshold %d progress %v", tok, c.Base().ThresholdTokens, lastProgress)
		}
	})
}
