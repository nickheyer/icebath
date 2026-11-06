package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
)

func TestResolveModelThresholdRanksScopedAndLongestKeys(t *testing.T) {
	th := map[string]float64{"astra": 0.85, "codex:astra": 0.7, "gpt": 0.5, "gpt-5": 0.6}
	cases := []struct {
		model, provider string
		want            float64
	}{
		{"openai/astra-1", " Codex ", 0.7}, {"openai/astra-1", "openrouter", 0.85},
		{"gpt-5-mini", "", 0.6}, {"claude", "", 0.4}, {"", "codex", 0.4},
	}
	for _, c := range cases {
		if got := resolveModelThreshold(c.model, th, 0.4, c.provider); got != c.want {
			t.Errorf("%s/%s = %v want %v", c.model, c.provider, got, c.want)
		}
	}
	if got := resolveModelThreshold("x", nil, 0.3, ""); got != 0.3 {
		t.Fatalf("empty thresholds = %v", got)
	}
}

func TestLeafHelpersMatchUpstreamText(t *testing.T) {
	if got := skillPrunedMarker("deploy"); got != "[SKILL_PRUNED: content lost in compression; reload with skill_view(name='deploy')]" {
		t.Fatal(got)
	}
	for in, want := range map[string]string{
		`{"error": "boom\n  bad   thing"}`: " FAILED: boom bad thing", `{"success": false}`: " FAILED",
		`{"success": 0}`: "", `not json`: "", `[1]`: "", ``: "",
		`{"error": "` + strings.Repeat("x", 90) + `"}`: " FAILED: " + strings.Repeat("x", 80),
	} {
		if got := resultFailureSuffix(in); got != want {
			t.Errorf("%q = %q want %q", in, got, want)
		}
	}
	if !isSummaryLine("[terminal] ran ls (1,234 chars)") || isSummaryLine("[terminal] ran ls") || isSummaryLine("[x] "+strings.Repeat("y", 400)+" chars)") {
		t.Fatal("summary line detection")
	}
	parts := []any{"a", map[string]any{"type": "text", "text": "b"}, map[string]any{"type": "image_url"}, 3}
	if got := contentTextForContains(parts); got != "a\nb" {
		t.Fatalf("parts text %q", got)
	}
	if contentTextForContains(nil) != "" || contentTextForContains(7) != "7" {
		t.Fatal("scalar content text")
	}
	msg := map[string]any{"role": "tool", "content": "old", "api_content": "stale"}
	out := rewritten(msg, "new")
	if _, ok := out["api_content"]; ok || out["content"] != "new" || msg["content"] != "old" {
		t.Fatalf("rewritten %v", out)
	}
	if tcGet(map[string]any{"id": "c1"}, "missing", "d") != "d" || tcGet(map[string]any{"id": "c1"}, "id", "") != "c1" {
		t.Fatal("tcGet")
	}
}

func TestSummaryRoutePinIsSingleUseAndScoped(t *testing.T) {
	ctx := context.Background()
	if takeDeterministicSummaryPin(ctx) {
		t.Fatal("no pin consumed")
	}
	_ = PinSummaryRoute(ctx, map[string]any{"provider": "p", "model": "m", "base_url": "", "timeout": 30.0, "label": "x"}, func() error {
		if takeDeterministicSummaryPin(ctx) {
			t.Fatal("a real route is not the deterministic sentinel")
		}
		got := pinnedSummaryCallKwargs(ctx)
		if len(got) != 3 || got["provider"] != "p" || got["timeout"] != 30.0 {
			t.Fatalf("pinned kwargs %v", got)
		}
		if takePinnedSummaryRoute(ctx) != nil {
			t.Fatal("pin is single use")
		}
		return nil
	})
	_ = PinSummaryRoute(ctx, DeterministicSummaryRoute(), func() error {
		if !takeDeterministicSummaryPin(ctx) || takeDeterministicSummaryPin(ctx) {
			t.Fatal("deterministic pin consumed exactly once")
		}
		return nil
	})
}

func TestLeanTailBudgetHoldsTheTriggerShare(t *testing.T) {
	for _, c := range [][3]int{{1_000_000, 0, 25_000}, {1_000_000, 12_000, 6_000}, {100_000, 0, 10_000}, {1_000_000, 1_500, 25_000}, {400_000, 100_000, 10_000}} {
		if got := LeanTailBudget(c[0], c[1]); got != c[2] {
			t.Errorf("LeanTailBudget(%d, %d) = %d want %d", c[0], c[1], got, c[2])
		}
	}
}

func tailRound(id, name, args, content string) []map[string]any {
	return []map[string]any{
		{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": id, "function": map[string]any{"name": name, "arguments": args}}}},
		{"role": "tool", "tool_call_id": id, "tool_name": name, "content": content, "api_content": "sidecar"},
	}
}

func TestDemoteStaleTailToolsKeepsTheNewestSixRounds(t *testing.T) {
	big := strings.Repeat("z", 2_000)
	messages := []map[string]any{{"role": "user", "content": "go"}}
	messages = append(messages, tailRound("s1", "skill_view", `{"name": "deploy"}`, big)...)
	messages = append(messages, tailRound("f1", "terminal", "{}", `{"error": "nope", "pad": "`+big+`"}`)...)
	for i := 0; i < 6; i++ {
		messages = append(messages, tailRound("k"+string(rune('a'+i)), "terminal", "{}", big)...)
	}
	out, demoted := DemoteStaleTailTools(messages, 1, "sess-1")
	if demoted != 2 {
		t.Fatalf("demoted %d", demoted)
	}
	want := "[skill_view output demoted at compaction — 2,000 chars preserved in session history. Recover with " +
		"session_search(query=..., session_id='sess-1')] " + skillPrunedMarker("deploy")
	if out[2]["content"] != want {
		t.Fatalf("skill row %q", out[2]["content"])
	}
	if _, ok := out[2]["api_content"]; ok {
		t.Fatal("demoted row keeps the stale sidecar")
	}
	if s := out[4]["content"].(string); strings.Contains(s, "SKILL_PRUNED") || !strings.HasPrefix(s, "[terminal output demoted") {
		t.Fatalf("failed call row %q", s)
	}
	if out[6]["content"] != big || messages[2]["content"] != big {
		t.Fatal("protected rows and the input stay untouched")
	}
	if !IsDemotedRow(want) || IsDemotedRow("x"+want) {
		t.Fatal("demoted row pattern anchors at the start")
	}
	if _, n := DemoteStaleTailTools(out, 1, ""); n != 0 {
		t.Fatal("a demoted row is never demoted again")
	}
}

type tailCutFake struct{ summaries map[int]bool }

func (f tailCutFake) TailRowPricer(messages []map[string]any) func(int) int {
	return func(i int) int { return 10 }
}
func (f tailCutFake) IsContextSummaryMessage(m map[string]any) bool {
	return m["summary"] == true
}
func (f tailCutFake) AlignBoundaryForward(messages []map[string]any, idx int) int {
	for idx < len(messages) && messages[idx]["role"] == "tool" {
		idx++
	}
	return idx
}
func (f tailCutFake) FindLastUserMessageIdx(messages []map[string]any, headEnd int) int {
	for i := len(messages) - 1; i >= headEnd; i-- {
		if messages[i]["role"] == "user" {
			return i
		}
	}
	return -1
}

func TestTailCutPricerAndProgressFloorSkipTheHandoff(t *testing.T) {
	msgs := []map[string]any{{"role": "system"}, {"role": "user", "summary": true}, {"role": "assistant"}, {"role": "tool"}, {"role": "tool"}, {"role": "assistant"}, {"role": "user"}, {"role": "assistant"}}
	price := TailCutPricer(tailCutFake{}, msgs, 1)
	if price(1) != 0 || price(0) != 0 || price(2) != 10 {
		t.Fatal("rows at or before the handoff are free")
	}
	if got := TailCutProgressFloor(tailCutFake{}, msgs, 1, 2); got != 5 {
		t.Fatalf("floor raised past the group with the active user still in the tail = %d", got)
	}
	userInGroup := []map[string]any{{"role": "system"}, {"role": "user", "summary": true}, {"role": "user"}, {"role": "tool"}, {"role": "tool"}, {"role": "assistant"}}
	if got := TailCutProgressFloor(tailCutFake{}, userInGroup, 1, 2); got != 2 {
		t.Fatalf("raising would drop the active request, got %d", got)
	}
	noUser := []map[string]any{{"role": "system"}, {"role": "user", "summary": true}, {"role": "assistant"}, {"role": "tool"}, {"role": "assistant"}, {"role": "assistant"}}
	if got := TailCutProgressFloor(tailCutFake{}, noUser, 1, 2); got != 4 {
		t.Fatalf("raised floor = %d", got)
	}
	if got := TailCutProgressFloor(tailCutFake{}, noUser, 1, 4); got != 2 {
		t.Fatalf("a kept row before the cut keeps the old floor, got %d", got)
	}
	plain := []map[string]any{{"role": "user"}, {"role": "assistant"}}
	if TailCutProgressFloor(tailCutFake{}, plain, 0, 1) != 1 || TailCutPricer(tailCutFake{}, plain, 0)(0) != 10 {
		t.Fatal("no handoff keeps the plain floor and price")
	}
}

func TestSkillSafetyRuleRidesTheHandoffExactlyOnce(t *testing.T) {
	marker := skillPrunedMarker("deploy")
	summary, head, tail := WithSkillSafetyRule("sum", nil, []map[string]any{{"role": "tool", "content": "[skill_view] x " + marker + skillSafetySuffix}})
	if summary != "sum"+skillSafetySuffix || tail[0]["content"] != "[skill_view] x "+marker || len(head) != 0 {
		t.Fatalf("rule moved onto the handoff: %q %v", summary, tail)
	}
	if s, _, _ := WithSkillSafetyRule(summary, nil, nil); s != summary {
		t.Fatal("a ruled summary is not ruled twice")
	}
	if s, _, _ := WithSkillSafetyRule("plain", []map[string]any{{"role": "user", "content": "hi"}}, nil); s != "plain" {
		t.Fatal("no marker no rule")
	}
	sys := []map[string]any{{"role": "system", "content": "p" + skillSafetySuffix}}
	if _, h, _ := WithSkillSafetyRule("sum "+marker, sys, nil); h[0]["content"] != "p"+skillSafetySuffix {
		t.Fatal("system rows keep their text")
	}
	long := strings.Repeat("a", 5_000) + skillSafetySuffix
	got := ElideKeepingSkillSafetyRule(long, 3_000)
	if !strings.HasSuffix(got, skillSafetySuffix) || len([]rune(got)) > 3_000 {
		t.Fatalf("elided length %d", len([]rune(got)))
	}
	original := []map[string]any{{"role": "tool", "content": "body"}, {"role": "tool", "content": "body2"}}
	pruned := []map[string]any{{"role": "tool", "content": "body"}, {"role": "tool", "content": "[skill_view] " + marker}}
	ruled := WithPrunedRowSkillSafetyRule(original, pruned)
	if ruled[1]["content"] != "[skill_view] "+marker+skillSafetySuffix || pruned[1]["content"] != "[skill_view] "+marker {
		t.Fatalf("pruned row rule %v", ruled[1])
	}
	if again := WithPrunedRowSkillSafetyRule(original, ruled); again[1]["content"] != ruled[1]["content"] {
		t.Fatal("already ruled")
	}
	if WithoutSkillSafetyRule("x"+skillSafetySuffix) != "x" {
		t.Fatal("strip rule")
	}
}

func TestCompressionTelemetryRecordsEachPhase(t *testing.T) {
	ctx := context.Background()
	tel := &CompressionTelemetry{Seed: &pyjson.Object{}}
	tel.Seed.Set("session_id", "seeded")
	tel.Seed.Set("overflow_reason", "context_overflow")
	cur := 900
	rec := tel.Begin(TelemetryAttempt{CurrentTokens: cur, AttemptID: "a1", TriggerSource: "preflight", Provider: "p", Model: "m", ContextLength: 1000, ThresholdTokens: "800"})
	if tel.Active != rec || tel.Last != rec || len(rec.Keys) != 51 || rec.Keys[0] != "event" || rec.Keys[50] != "prepared_summary" {
		t.Fatalf("seeded keys %d %v", len(rec.Keys), rec.Keys[:3])
	}
	for k, want := range map[string]any{"attempt_id": "a1", "session_id": "seeded", "trigger_source": "preflight", "overflow_reason": "context_overflow", "effective_threshold": 800, "current_estimated_tokens": 900, "main_context_limit": 1000} {
		if got, _ := rec.Get(k); got != want {
			t.Errorf("%s = %v want %v", k, got, want)
		}
	}
	if id, _ := (&CompressionTelemetry{}).Begin(TelemetryAttempt{}).Get("attempt_id"); len(id.(string)) != 32 {
		t.Fatalf("generated attempt id %v", id)
	}
	prompt := []map[string]any{{"role": "user", "content": strings.Repeat("w", 400)}}
	tel.RecordAuxCall(ctx, AuxCompressionCall{PromptMessages: prompt, MaxTokens: 50, DurationMs: 120, AuxModel: "aux", EffectiveAuxContext: 1000,
		PhaseTimings: map[string]any{"queue_wait_ms": 5, "commit_ms": "7", "prompt_build_ms": nil}})
	tel.RecordAuxCall(ctx, AuxCompressionCall{PromptMessages: prompt, DurationMs: -3, PhaseTimings: map[string]any{"queue_wait_ms": 6}})
	promptTokens, _ := rec.Get("aux_prompt_tokens")
	fit, _ := rec.Get("fit_margin")
	if fit != 1000-promptTokens.(int) {
		t.Fatalf("fit margin %v with prompt %v", fit, promptTokens)
	}
	for k, want := range map[string]any{"aux_call_duration_ms": 120, "queue_wait_ms": 11, "commit_ms": 7, "prompt_build_ms": nil, "aux_model": "aux", "aux_provider": "", "aux_output_reservation": nil} {
		if got, _ := rec.Get(k); got != want {
			t.Errorf("%s = %v want %v", k, got, want)
		}
	}
	tel.NoteAuxRouteFallback(map[string]string{"failed_model": "cheap", "fallback_label": "main-agent(m)", "failure": strings.Repeat("e", 216) + "    tail"})
	if fc, _ := rec.Get("failure_class"); fc != "aux_model_fallback" || tel.LastAuxModelFailureModel != "cheap" || tel.LastAuxModelFailureError != strings.Repeat("e", 216)+"..." {
		t.Fatalf("fallback note %v %q", fc, tel.LastAuxModelFailureError)
	}
	if tel.Method() != "aux_fallback_main" {
		t.Fatal(tel.Method())
	}
	tel.LastSummaryDroppedCount = 4
	tel.RecordEffect(ctx, 10, prompt, 500, 2, 1)
	after, _ := rec.Get("tokens_after")
	if v, _ := rec.Get("tokens_reclaimed"); v != 500-after.(int) {
		t.Fatal("reclaimed")
	}
	if v, _ := rec.Get("items_dropped"); v != 4 {
		t.Fatal("items dropped")
	}
	tel.RecordCommittedEffect(ctx, append(prompt, prompt...), prompt)
	if v, _ := rec.Get("messages_before"); v != 2 {
		t.Fatal("committed shape")
	}
	tel.LastSummaryFallbackUsed = true
	if tel.Method() != "deterministic_fallback" || (&CompressionTelemetry{}).Method() != "llm_summary" {
		t.Fatal("method precedence")
	}
}

type summaryFake struct {
	summaryModel string
	fallenBack   bool
	lastError    string
	telemetry    *pyjson.Object
	gotFocus     *string
	restored     bool
	err          error
}

func (f *summaryFake) SummaryModel() string         { return f.summaryModel }
func (f *summaryFake) SummaryModelFallenBack() bool { return f.fallenBack }
func (f *summaryFake) MainRoute() (string, string, string, string, string) {
	return "main-prov", "main-model", "", "key", ""
}
func (f *summaryFake) SetLastSummaryError(text string)            { f.lastError = text }
func (f *summaryFake) ActiveCompressionTelemetry() *pyjson.Object { return f.telemetry }
func (f *summaryFake) DeriveAutoFocusTopic(messages []map[string]any) *string {
	s := "derived"
	return &s
}
func (f *summaryFake) GenerateWindowSummary(ctx context.Context, turns []map[string]any, focus *string, memoryContext string) (*string, error) {
	f.gotFocus = focus
	s := "summary:" + memoryContext
	return &s, f.err
}
func (f *summaryFake) RestoreSummaryState(previous *string, hasUser *bool) { f.restored = true }

type bypassSummaryFake struct {
	summaryFake
	bypass bool
}

func (f *bypassSummaryFake) GenerateWindowSummaryBypassingCooldown(ctx context.Context, turns []map[string]any, focus *string, memoryContext string, bypass bool) (*string, error) {
	f.bypass = bypass
	return f.GenerateWindowSummary(ctx, turns, focus, memoryContext)
}

func TestApplySummaryRouteNamesTheMainRuntimeAfterFallback(t *testing.T) {
	kw := map[string]any{"task": "compression", "provider": "aux", "api_key": "aux-key"}
	ApplySummaryRoute(&summaryFake{}, kw, map[string]any{"provider": "fb", "model": "fbm"})
	if _, ok := kw["api_key"]; ok || kw["provider"] != "fb" || kw["model"] != "fbm" {
		t.Fatalf("pinned route %v", kw)
	}
	kw = map[string]any{}
	ApplySummaryRoute(&summaryFake{summaryModel: "sm", fallenBack: true}, kw, nil)
	if len(kw) != 1 || kw["model"] != "sm" {
		t.Fatalf("summary model %v", kw)
	}
	kw = map[string]any{}
	ApplySummaryRoute(&summaryFake{}, kw, nil)
	if len(kw) != 0 {
		t.Fatal("no fallback leaves the route")
	}
	ApplySummaryRoute(&summaryFake{fallenBack: true}, kw, nil)
	if kw["task"] != provider.AuthoritativeAuxiliaryTask("compression") || kw["provider"] != "main-prov" || kw["api_key"] != "key" {
		t.Fatalf("main route %v", kw)
	}
	if _, ok := kw["base_url"]; ok {
		t.Fatal("empty main fields stay unset")
	}
}

func TestSummarizeWindowDerivesFocusAndPassesBypass(t *testing.T) {
	ctx := context.Background()
	f := &summaryFake{}
	got, err := SummarizeWindow(ctx, f, nil, nil, HandoffScan{}, nil, "mem", true)
	if err != nil || *got != "summary:mem" || *f.gotFocus != "derived" {
		t.Fatalf("summary %v %v", got, err)
	}
	topic := "focus"
	b := &bypassSummaryFake{}
	if _, err := SummarizeWindow(ctx, b, nil, nil, HandoffScan{}, &topic, "", true); err != nil || !b.bypass || *b.gotFocus != "focus" {
		t.Fatal("bypass keyword reaches a generator that accepts it")
	}
	f.err = &provider.AuxiliaryExplicitCancellation{}
	_ = PinSummaryRoute(ctx, map[string]any{"provider": "p"}, func() error {
		f.err = nil
		_, err := SummarizeWindow(ctx, f, nil, nil, HandoffScan{}, nil, "", false)
		if err != nil || f.restored || summaryRoutePin.Get(ctx) == nil {
			t.Fatal("a real pinned route stays for the summary call")
		}
		return nil
	})
}
