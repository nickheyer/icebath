package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Assistant row carrying one terminal tool call
func pruneCall(id string) map[string]any {
	return map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{
		"id": id, "type": "function", "function": map[string]any{"name": "terminal", "arguments": `{"cmd":"ls"}`}}}}
}

// Tool result row answering the call id
func pruneResult(id, content string) map[string]any {
	return map[string]any{"role": "tool", "tool_call_id": id, "content": content}
}

// System row plus call and result pairs, big payloads at the given pair indices
func pruneBuild(pairs int, big map[int]bool, bigChars int) []map[string]any {
	msgs := []map[string]any{{"role": "system", "content": "sys"}}
	for i := range pairs {
		id := fmt.Sprintf("call_%d", i)
		content := "ok"
		if big[i] {
			content = strings.Repeat(string(rune(65+i%26)), bigChars)
		}
		msgs = append(msgs, pruneCall(id), pruneResult(id, content))
	}
	return msgs
}

func pruneIdx(ids ...int) map[int]bool {
	out := map[int]bool{}
	for _, i := range ids {
		out[i] = true
	}
	return out
}

// Compressor over a one million token window with the prune test defaults and edits
func pruneCompressor(t *testing.T, edit func(*ContextCompressorOptions)) *ContextCompressor {
	return newCompressorWith(t, 1_000_000, func(o *ContextCompressorOptions) {
		o.Model, o.ThresholdPercent, o.ProtectFirstN, o.ProtectLastN = "test", 0.50, 2, 4
		if edit != nil {
			edit(o)
		}
	})
}

func pruneArmed(o *ContextCompressorOptions) {
	o.ProactivePruneTokens, o.ProactivePruneMinResultChars = 48_000, 8_000
}

func pruneToolByID(msgs []map[string]any, id string) map[string]any {
	for _, m := range msgs {
		if m["role"] == "tool" && m["tool_call_id"] == id {
			return m
		}
	}
	return nil
}

func pruneBudget(c *ContextCompressor, msgs []map[string]any) int { return c.messageBudgetTokens(msgs) }

func sameSlice(a, b []map[string]any) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

// Captures warning and above records of the default logger
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logs
}

func patchPruneScan(t *testing.T, fn func(c *ContextCompressor, msgs []map[string]any, tailCount int, tailTokens *int, minChars int) ([]map[string]any, int)) {
	prev := compressorPruneScan
	compressorPruneScan = fn
	t.Cleanup(func() { compressorPruneScan = prev })
}

// Rearm mark parked 913 tokens above the message only estimate
func parkRearmAboveMessages(t *testing.T, c *ContextCompressor, msgs []map[string]any) {
	before := pruneBudget(c, msgs)
	c.proactivePruneRearmTokens = before + 913
	if before >= c.proactivePruneRearmTokens {
		t.Fatal("rearm not parked above the estimate")
	}
}

func rearmCompressor(t *testing.T, edit func(*ContextCompressorOptions)) *ContextCompressor {
	return pruneCompressor(t, func(o *ContextCompressorOptions) {
		pruneArmed(o)
		if edit != nil {
			edit(o)
		}
	})
}

func TestBilledBasisOverThresholdDefeatsMessageOnlyRearmLockout(t *testing.T) {
	c := rearmCompressor(t, nil)
	msgs := pruneBuild(8, pruneIdx(0, 1, 2), 9000)
	parkRearmAboveMessages(t, c, msgs)
	reclaimed := make([]map[string]any, len(msgs))
	for i, m := range msgs {
		reclaimed[i] = withoutKeys(m, nil)
		if i < len(msgs)-2 && m["role"] == "tool" {
			reclaimed[i]["content"] = "[pruned]"
		}
	}
	scans := 0
	patchPruneScan(t, func(*ContextCompressor, []map[string]any, int, *int, int) ([]map[string]any, int) {
		scans++
		return reclaimed, 3
	})
	result, pruned := c.pruneToolResultsOnly(msgs, iptr(c.ThresholdTokens()+1))
	if scans == 0 || pruned != 3 || sameSlice(result, msgs) {
		t.Fatalf("rearm gate short-circuited: scans=%d pruned=%d", scans, pruned)
	}
}

func TestMessageOnlyRearmStillHoldsBelowThreshold(t *testing.T) {
	c := rearmCompressor(t, nil)
	msgs := pruneBuild(8, pruneIdx(0, 1, 2), 9000)
	parkRearmAboveMessages(t, c, msgs)
	under := c.ThresholdTokens() - 1
	if under < c.proactivePruneTokens {
		t.Fatal("threshold below the prune trigger")
	}
	patchPruneScan(t, func(*ContextCompressor, []map[string]any, int, *int, int) ([]map[string]any, int) {
		panic("scan must not run below threshold")
	})
	result, pruned := c.pruneToolResultsOnly(msgs, iptr(under))
	if !sameSlice(result, msgs) || pruned != 0 {
		t.Fatalf("prune ran below threshold: %d", pruned)
	}
}

func TestPruneNoOpBelowTheTrigger(t *testing.T) {
	c := rearmCompressor(t, nil)
	msgs := pruneBuild(8, pruneIdx(0, 1, 2), 9000)
	c.OnSessionReset()
	patchPruneScan(t, func(*ContextCompressor, []map[string]any, int, *int, int) ([]map[string]any, int) {
		panic("scan must not run below the trigger")
	})
	result, pruned := c.pruneToolResultsOnly(msgs, iptr(c.proactivePruneTokens-1))
	if !sameSlice(result, msgs) || pruned != 0 {
		t.Fatalf("prune ran below the trigger: %d", pruned)
	}
}

func TestOverThresholdReclamationNoOpWarnsOnce(t *testing.T) {
	c := rearmCompressor(t, func(o *ContextCompressorOptions) { o.ProactivePruneMinReclaimTokens = 10_000_000 })
	msgs := pruneBuild(8, pruneIdx(0, 1, 2), 9000)
	billed := iptr(c.ThresholdTokens() + 5_000)
	logs := captureWarnings(t)
	result, pruned := c.pruneToolResultsOnly(msgs, billed)
	if !sameSlice(result, msgs) || pruned != 0 {
		t.Fatalf("prune committed: %d", pruned)
	}
	first := strings.Count(logs.String(), "over the compression threshold")
	if first == 0 {
		t.Fatal("over-threshold reclamation no-op was silent")
	}
	c.pruneToolResultsOnly(msgs, billed)
	if got := strings.Count(logs.String(), "over the compression threshold"); got != first {
		t.Fatalf("warning repeated: %d then %d", first, got)
	}
}

func TestUnderThresholdNoOpIsNotWarned(t *testing.T) {
	c := rearmCompressor(t, func(o *ContextCompressorOptions) { o.ProactivePruneMinReclaimTokens = 10_000_000 })
	msgs := pruneBuild(8, pruneIdx(0, 1, 2), 9000)
	logs := captureWarnings(t)
	result, pruned := c.pruneToolResultsOnly(msgs, iptr(c.ThresholdTokens()-1))
	if !sameSlice(result, msgs) || pruned != 0 || strings.Contains(logs.String(), "over the compression threshold") {
		t.Fatalf("under threshold no-op warned or committed: %d %q", pruned, logs.String())
	}
}

func TestProactivePruneBelowCompressionThreshold(t *testing.T) {
	c := pruneCompressor(t, pruneArmed)
	if c.ShouldCompress(iptr(120_000)) {
		t.Fatal("compression would run")
	}
	msgs := pruneBuild(8, pruneIdx(0, 1, 2), 9000)
	result, pruned := c.pruneToolResultsOnly(msgs, iptr(120_000))
	if pruned < 3 || len(result) != len(msgs) {
		t.Fatalf("pruned %d rows %d", pruned, len(result))
	}
	for _, id := range []string{"call_0", "call_1", "call_2"} {
		content := pruneToolByID(result, id)["content"].(string)
		if len(content) >= 9000 || content == prunedToolPlaceholder {
			t.Fatalf("%s not summarized: %q", id, content)
		}
	}
}

func TestProactivePruneIdempotent(t *testing.T) {
	c := pruneCompressor(t, pruneArmed)
	first, n1 := c.pruneToolResultsOnly(pruneBuild(8, pruneIdx(0, 1, 2), 9000), iptr(120_000))
	second, n2 := c.pruneToolResultsOnly(first, nil)
	if n1 < 3 || n2 != 0 || !sameRows(first, second) {
		t.Fatalf("not idempotent: %d %d", n1, n2)
	}
}

func TestProactivePruneRearmsOnlyAfterReclaimedTokenRunway(t *testing.T) {
	c := pruneCompressor(t, pruneArmed)
	first, n1 := c.pruneToolResultsOnly(pruneBuild(8, pruneIdx(0, 1, 2, 6, 7), 9000), iptr(120_000))
	if n1 < 3 {
		t.Fatalf("first prune %d", n1)
	}
	rearm := pruneBudget(c, first) + 48_000
	grown := append(append([]map[string]any{}, first...), pruneCall("call_8"), pruneResult("call_8", "ok"), pruneCall("call_9"), pruneResult("call_9", "ok"))
	if pruneBudget(c, grown) >= rearm {
		t.Fatal("grown already past the runway")
	}
	under := iptr(c.ThresholdTokens() - 1)
	blocked, n2 := c.pruneToolResultsOnly(grown, under)
	if n2 != 0 || !sameSlice(blocked, grown) {
		t.Fatalf("pruned before the runway: %d", n2)
	}
	for _, id := range []string{"call_6", "call_7"} {
		if len(pruneToolByID(blocked, id)["content"].(string)) != 9000 {
			t.Fatalf("%s touched", id)
		}
	}
	missing := rearm - pruneBudget(c, grown)
	regrown := append(append([]map[string]any{}, grown...), map[string]any{"role": "user", "content": strings.Repeat("x", missing*4)})
	if pruneBudget(c, regrown) < rearm {
		t.Fatal("regrown short of the runway")
	}
	rearmed, n3 := c.pruneToolResultsOnly(regrown, under)
	if n3 < 2 || sameSlice(rearmed, regrown) {
		t.Fatalf("did not rearm: %d", n3)
	}
}

func TestSuccessfulFullCompressionResetsProactiveRunway(t *testing.T) {
	c := pruneCompressor(t, pruneArmed)
	if _, n1 := c.pruneToolResultsOnly(pruneBuild(8, pruneIdx(0, 1, 2), 9000), iptr(120_000)); n1 < 3 {
		t.Fatalf("first prune %d", n1)
	}
	history := []map[string]any{{"role": "system", "content": "sys"}}
	for i := range 12 {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		history = append(history, map[string]any{"role": role, "content": fmt.Sprintf("turn %d ", i) + strings.Repeat("x", 1000)})
	}
	c.SetTailTokenBudget(50)
	patchGenerateSummary(t, "summary")
	compressed, err := c.Compress(history, iptr(500_000), nil, true, "")
	if err != nil || !c.lastCompressionMadeProgress || len(compressed) >= len(history) {
		t.Fatalf("compress: %v progress=%v rows=%d", err, c.lastCompressionMadeProgress, len(compressed))
	}
	fresh := pruneBuild(8, pruneIdx(0, 1, 2), 9000)
	result, pruned := c.pruneToolResultsOnly(fresh, iptr(48_000))
	if pruned < 3 || sameSlice(result, fresh) {
		t.Fatalf("runway not reset: %d", pruned)
	}
}

// Replaces the summary generator with a fixed answer
func patchGenerateSummary(t *testing.T, summary string) {
	prev := compressorGenerateSummary
	compressorGenerateSummary = func(*ContextCompressor, context.Context, []map[string]any, *string, string, bool) (*string, error) {
		s := summary
		return &s, nil
	}
	t.Cleanup(func() { compressorGenerateSummary = prev })
}

func TestProactivePruneMinReclaimGateClamp(t *testing.T) {
	for _, v := range []int{0, -5} {
		if got := pruneCompressor(t, func(o *ContextCompressorOptions) { o.ProactivePruneMinReclaimTokens = v }).proactivePruneMinReclaimTokens; got != 0 {
			t.Fatalf("%d clamped to %d", v, got)
		}
	}
}

func TestProactivePruneNoOrphansBothDirections(t *testing.T) {
	c := pruneCompressor(t, func(o *ContextCompressorOptions) { pruneArmed(o); o.ProactivePruneMinReclaimTokens = 0 })
	result, pruned := c.pruneToolResultsOnly(pruneBuild(10, pruneIdx(0, 1, 2, 3, 4), 9000), iptr(120_000))
	if pruned < 1 {
		t.Fatal("nothing pruned")
	}
	calls, results := map[string]bool{}, map[string]bool{}
	for _, m := range result {
		if m["role"] == "assistant" {
			for _, id := range callIDs(m) {
				calls[id] = true
			}
		}
		if m["role"] == "tool" {
			results[m["tool_call_id"].(string)] = true
		}
	}
	if fmt.Sprint(calls) != fmt.Sprint(results) {
		t.Fatalf("orphans: calls %v results %v", calls, results)
	}
}

func TestProactivePruneUnsetConfigZeroBehaviorChange(t *testing.T) {
	c := pruneCompressor(t, nil)
	if c.proactivePruneTokens != 0 {
		t.Fatal("prune armed by default")
	}
	msgs := pruneBuild(8, pruneIdx(0, 1, 2), 9000)
	snapshot := pyrepr.Repr(msgs)
	result, pruned := c.pruneToolResultsOnly(msgs, iptr(10_000_000))
	if pruned != 0 || !sameSlice(result, msgs) || pyrepr.Repr(msgs) != snapshot {
		t.Fatalf("unset config changed behavior: %d", pruned)
	}
}

// Prune only marks a skill row with the rule once and salvage keeps the rule past the cut
func TestSkillSafetyRuleSurvivesPruneOnlyAndSalvageRewrites(t *testing.T) {
	c := pruneCompressor(t, pruneArmed)
	type call struct{ tool, args, content string }
	calls := []call{{"skill_view", `{"name":"pdf"}`, strings.Repeat("P", 9000)}, {"terminal", `{"cmd":"ls"}`, strings.Repeat("A", 9000)}, {"terminal", `{"cmd":"ls"}`, strings.Repeat("B", 9000)}}
	for range 5 {
		calls = append(calls, call{"terminal", `{"cmd":"ls"}`, "ok"})
	}
	msgs := []map[string]any{{"role": "system", "content": "sys"}}
	for i, cl := range calls {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs, map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{
			"id": id, "type": "function", "function": map[string]any{"name": cl.tool, "arguments": cl.args}}}}, pruneResult(id, cl.content))
	}
	texts := func(rows []map[string]any) string {
		var b []string
		for _, m := range rows {
			s, _ := m["content"].(string)
			b = append(b, s)
		}
		return strings.Join(b, "\n")
	}
	pruned, count := c.pruneToolResultsOnly(msgs, iptr(120_000))
	if count == 0 || !strings.Contains(texts(pruned), skillPrunedMarkerPrefix) || strings.Count(texts(pruned), skillSafetyRule) != 1 {
		t.Fatalf("prune only rule: count=%d", count)
	}
	if _, again := c.pruneOldToolResults(pruned, 4, nil, pruneMinChars); again != 0 {
		t.Fatalf("ruled stub pruned again: %d", again)
	}
	summary := "## Goal\n" + strings.Repeat("x", 20_000) + "\n\n" + skillSafetyRule + "\n\n" + state.SummaryEndMarker
	grown := []map[string]any{
		{"role": "user", "content": summary, state.CompressedSummaryMetadataKey: true},
		{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "s", "type": "function", "function": map[string]any{"name": "skill_view", "arguments": `{"name":"pdf"}`}}}},
		pruneResult("s", "[skill_view] name=pdf (9,000 chars) "+skillPrunedMarker("pdf")),
		{"role": "user", "content": "latest ask"},
	}
	salvaged := SalvageGrownTranscript(context.Background(), []map[string]any{{"role": "user", "content": strings.Repeat("y", 40_000)}}, grown, nil)
	if salvaged == nil || len(salvaged[0]["content"].(string)) >= len(summary) || !strings.Contains(salvaged[0]["content"].(string), skillSafetyRule) {
		t.Fatal("salvage dropped the rule or kept the full handoff")
	}
}

var errDiskFull = errors.New("disk full")
