package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestCompressionMadeProgress(t *testing.T) {
	caseHome(t)
	t.Run("test_rows_reduced_counts_as_progress", func(t *testing.T) {
		if !compressionMadeProgress(10, 5, 1000, 1000) {
			t.Fatal("fewer rows is progress")
		}
	})
	t.Run("test_neither_moved_means_no_progress", func(t *testing.T) {
		if compressionMadeProgress(10, 10, 1000, 1000) {
			t.Fatal("same rows and tokens is no progress")
		}
	})
	t.Run("test_sub_5pct_token_drop_is_not_progress", func(t *testing.T) {
		if compressionMadeProgress(10, 10, 1000, 970) || !compressionMadeProgress(10, 10, 1000, 940) {
			t.Fatal("only a token drop past five percent is progress")
		}
	})
}

func TestCompressionWarrantsAnotherPreflightPass(t *testing.T) {
	caseHome(t)
	t.Run("test_material_reduction_above_threshold_allows_another_pass", func(t *testing.T) {
		if !compressionWarrantsAnotherPreflightPass(400_000, 350_000, 272_000) {
			t.Fatal("a material cut still over threshold allows another pass")
		}
	})
	t.Run("test_marginal_reduction_above_threshold_stops", func(t *testing.T) {
		if compressionWarrantsAnotherPreflightPass(350_000, 345_000, 272_000) {
			t.Fatal("a marginal cut stops the passes")
		}
	})
}

func TestBoundaryIsExportedOnlyForTheVerbatimCurrentRow(t *testing.T) {
	caseHome(t)
	multimodal := func() []any {
		return []any{map[string]any{"type": "text", "text": "look"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:x"}}}
	}
	for _, tc := range []struct {
		userMessage any
		messages    []map[string]any
		want        int
	}{
		{"same question", []map[string]any{{"role": "user", "content": "same question"}, {"role": "assistant", "content": "old answer"},
			{"role": "user", "content": "same question"}, {"role": "assistant", "content": "new answer"}}, 2},
		{multimodal(), []map[string]any{{"role": "user", "content": multimodal()}, {"role": "assistant", "content": "ok"}}, 0},
		{"same question", []map[string]any{{"role": "user", "content": "summary\n\nsame question"}, {"role": "assistant", "content": "answer"}}, -1},
		{"another question", []map[string]any{{"role": "user", "content": "same question"}, {"role": "assistant", "content": "old answer"}}, -1},
	} {
		a := &AIAgent{currentTurnID: "session:task:abcd1234"}
		result := exportCurrentTurnBoundary(a, map[string]any{"messages": rowsToMessages(tc.messages)}, tc.userMessage)
		_, hasIdx := result["current_turn_user_idx"]
		_, hasTurn := result["turn_id"]
		if tc.want < 0 {
			if hasIdx || hasTurn {
				t.Fatalf("%v: an unproven boundary exported %v", tc.userMessage, result)
			}
		} else if result["current_turn_user_idx"] != tc.want || result["turn_id"] != a.currentTurnID {
			t.Fatalf("%v: exported %v %v", tc.userMessage, result["current_turn_user_idx"], result["turn_id"])
		}
		if a.persistUserMessageIdx != nil {
			t.Fatal("the persist anchor must not be re-anchored by the export")
		}
	}
}

func TestPreflightTimeoutEnvelopeExportsNothing(t *testing.T) {
	caseHome(t)
	a := &AIAgent{currentTurnID: "session:task:abcd1234"}
	result := map[string]any{"turn_exit_reason": "context_compression_timeout",
		"messages": rowsToMessages([]map[string]any{{"role": "user", "content": "continue"}, {"role": "assistant", "content": "old"}})}
	out := exportCurrentTurnBoundary(a, result, "continue")
	if _, ok := out["current_turn_user_idx"]; ok {
		t.Fatal("a preflight timeout envelope exports no index")
	}
	if _, ok := out["turn_id"]; ok {
		t.Fatal("a preflight timeout envelope exports no turn id")
	}
}

func TestRunConversationExportsThePairOnASuccessEnvelope(t *testing.T) {
	caseHome(t)
	script := &thinkingScript{responses: []map[string]any{thinkingMockResponse("new answer", "stop")}}
	a, ctx := thinkingLoopAgent(t, script)
	history := []map[string]any{{"role": "user", "content": "same question"}, {"role": "assistant", "content": "old answer"}}
	result, err := a.RunConversation(ctx, "same question", RunConversationOptions{ConversationHistory: history})
	if err != nil {
		t.Fatal(err)
	}
	messages := resultRows(t, result)
	idx, ok := result["current_turn_user_idx"].(int)
	if result["completed"] != true || !ok || idx <= 0 || idx >= len(messages) {
		t.Fatalf("completed %v idx %v of %d rows", result["completed"], result["current_turn_user_idx"], len(messages))
	}
	if messages[idx]["role"] != "user" || messages[idx]["content"] != "same question" || result["turn_id"] != a.currentTurnID ||
		messages[len(messages)-1]["content"] != "new answer" {
		t.Fatalf("row %+v turn %v last %v", messages[idx], result["turn_id"], messages[len(messages)-1]["content"])
	}
}

// Message rows of a turn result, whichever of the two list shapes the envelope carries
func resultRows(t *testing.T, result map[string]any) []map[string]any {
	t.Helper()
	switch rows := result["messages"].(type) {
	case []map[string]any:
		return rows
	case []*state.Message:
		return messagesToRows(rows)
	}
	t.Fatalf("result messages of type %T", result["messages"])
	return nil
}

const (
	sidecarResetNote = "[System note: The user's previous session expired due to inactivity. This is a fresh conversation with no prior context.]"
	sidecarVCNote    = "[Voice channel now: dev-vc (2 members)]"
)

// Real agent trimmed to what the prologue touches, the stand in for the hermes fake agent
func prologueAgent(t *testing.T) *AIAgent {
	t.Helper()
	a := newFixtureAgent(t, AIAgentOptions{BaseURL: "https://openrouter.ai/api/v1", Model: "test/model", Platform: "cli", EnabledToolsets: []string{}})
	t.Cleanup(func() { a.Close() })
	a.sessionID, a.cachedSystemPrompt, a.skipMCPRefresh, a.compressionEnabled = "sess-1", "SYSTEM", true, false
	a.memoryManager, a.memoryStore, a.memoryNudgeInterval = nil, nil, 0
	return a
}

// Replaces the plugin hook dispatch for the case the way the hermes cases patch plugins.invoke_hook
func patchPluginHooks(t *testing.T, fn func(hook string, kwargs map[string]any) []any) {
	prev := lifecyclePluginInvokeHook
	lifecyclePluginInvokeHook = func(_ context.Context, hook string, kwargs map[string]any) []any { return fn(hook, kwargs) }
	t.Cleanup(func() { lifecyclePluginInvokeHook = prev })
}

func prologueBuildErr(a *AIAgent, edit func(*TurnContextRequest)) (*TurnContext, error) {
	r := TurnContextRequest{UserMessage: "hello"}
	if edit != nil {
		edit(&r)
	}
	return buildTurnContext(tools.WithToolEnv(context.Background(), a.env), a, r)
}

func prologueBuild(t *testing.T, a *AIAgent, edit func(*TurnContextRequest)) *TurnContext {
	t.Helper()
	tc, err := prologueBuildErr(a, edit)
	if err != nil {
		t.Fatal(err)
	}
	return tc
}

func TestConsumeIsOneShot(t *testing.T) {
	caseHome(t)
	t.Run("test_consume_clears_the_attribute", func(t *testing.T) {
		a := &AIAgent{}
		a.gatewayTurnContextNotes = sidecarResetNote
		if consumeGatewayTurnContextNotes(a) != sidecarResetNote || consumeGatewayTurnContextNotes(a) != "" {
			t.Fatal("the note is consumed once")
		}
	})
	t.Run("test_absent_attribute_is_empty", func(t *testing.T) {
		if consumeGatewayTurnContextNotes(&AIAgent{}) != "" {
			t.Fatal("no staged note reads empty")
		}
	})
	t.Run("test_non_string_value_is_empty", func(t *testing.T) {
		a := &AIAgent{}
		a.SetGatewayTurnContextNotes("")
		if consumeGatewayTurnContextNotes(a) != "" {
			t.Fatal("a slot holding no text reads empty")
		}
	})
}

func TestStringContentSidecarDelivery(t *testing.T) {
	caseHome(t)
	t.Run("test_notes_ride_the_api_content_sidecar", func(t *testing.T) {
		a := prologueAgent(t)
		a.platform, a.gatewayTurnContextNotes = "discord", sidecarResetNote
		patchPluginHooks(t, func(string, map[string]any) []any { return nil })
		tc := prologueBuild(t, a, nil)
		msg := tc.Messages[tc.CurrentTurnUserIdx]
		composed, _ := composeUserAPIContent("hello", tc.ExtPrefetchCache, tc.PluginUserContext)
		if msg.Content != "hello" || msg.APIContent != "hello\n\n"+sidecarResetNote || msg.APIContent != composed || a.gatewayTurnContextNotes != "" {
			t.Fatalf("content %q sidecar %q composed %q pending %q", msg.Content, msg.APIContent, composed, a.gatewayTurnContextNotes)
		}
	})
	t.Run("test_notes_append_after_plugin_context", func(t *testing.T) {
		a := prologueAgent(t)
		a.platform, a.gatewayTurnContextNotes = "discord", sidecarVCNote
		patchPluginHooks(t, func(hook string, _ map[string]any) []any {
			if hook == "pre_llm_call" {
				return []any{map[string]any{"context": "PLUGIN-CTX"}}
			}
			return nil
		})
		tc := prologueBuild(t, a, nil)
		if got := tc.Messages[tc.CurrentTurnUserIdx].APIContent; got != "hello\n\nPLUGIN-CTX\n\n"+sidecarVCNote {
			t.Fatalf("sidecar %q", got)
		}
	})
	t.Run("test_no_notes_means_no_stamp", func(t *testing.T) {
		a := prologueAgent(t)
		a.platform = "discord"
		patchPluginHooks(t, func(string, map[string]any) []any { return nil })
		tc := prologueBuild(t, a, nil)
		if got := tc.Messages[tc.CurrentTurnUserIdx].APIContent; got != "" {
			t.Fatalf("sidecar %q", got)
		}
	})
}

func TestMultimodalFallback(t *testing.T) {
	caseHome(t)
	t.Run("test_notes_appended_as_text_part_on_list_content", func(t *testing.T) {
		a := prologueAgent(t)
		a.platform, a.gatewayTurnContextNotes = "discord", sidecarResetNote
		patchPluginHooks(t, func(string, map[string]any) []any { return nil })
		content := []any{map[string]any{"type": "text", "text": "look at this"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://x/img.png"}}}
		tc := prologueBuild(t, a, func(r *TurnContextRequest) { r.UserMessage = content })
		msg := tc.Messages[tc.CurrentTurnUserIdx]
		parts := anyRows(msg.Content)
		if len(parts) == 0 || !reflect.DeepEqual(parts[len(parts)-1], map[string]any{"type": "text", "text": sidecarResetNote}) || msg.APIContent != "" {
			t.Fatalf("parts %v sidecar %q", parts, msg.APIContent)
		}
	})
	t.Run("test_helper_appends_only_to_lists", func(t *testing.T) {
		var content any = []any{map[string]any{"type": "text", "text": "hi"}}
		if !appendNotesToMultimodalContent(&content, "NOTE") {
			t.Fatal("a list takes the note")
		}
		parts := content.([]any)
		if !reflect.DeepEqual(parts[len(parts)-1], map[string]any{"type": "text", "text": "NOTE"}) {
			t.Fatalf("parts %v", parts)
		}
		var text any = "string"
		if appendNotesToMultimodalContent(&text, "NOTE") || appendNotesToMultimodalContent(&content, "") {
			t.Fatal("a string or an empty note appends nothing")
		}
	})
}

func idleDecide(edit func(o *idleDecision)) bool {
	o := idleDecision{enabled: true, idleAfter: 1800, idleGap: 3600.0, tokens: 100_000, floor: 40_000}
	if edit != nil {
		edit(&o)
	}
	return shouldIdleCompact(o.enabled, o.idleAfter, o.idleGap, o.tokens, o.floor, o.cooldown, o.last)
}

type idleDecision struct {
	enabled             bool
	idleAfter           int
	idleGap             float64
	tokens, floor, last int
	cooldown            bool
}

func TestShouldIdleCompact(t *testing.T) {
	caseHome(t)
	t.Run("test_disabled_when_idle_after_zero", func(t *testing.T) {
		if idleDecide(func(o *idleDecision) { o.idleAfter, o.idleGap = 0, 10_000.0 }) {
			t.Fatal("zero disables idle compaction")
		}
	})
	t.Run("test_disabled_when_compression_off", func(t *testing.T) {
		if idleDecide(func(o *idleDecision) { o.enabled = false }) {
			t.Fatal("compression off disables idle compaction")
		}
	})
	t.Run("test_fires_just_above_floor", func(t *testing.T) {
		if !idleDecide(func(o *idleDecision) { o.tokens, o.floor = 40_001, 40_000 }) {
			t.Fatal("one token above the floor fires")
		}
	})
}

func TestPostCompactionFloor(t *testing.T) {
	caseHome(t)
	at := func(tokens, floor, last int) func(*idleDecision) {
		return func(o *idleDecision) { o.tokens, o.floor, o.last = tokens, floor, last }
	}
	t.Run("test_unrecorded_last_compaction_keeps_original_floor", func(t *testing.T) {
		if !idleDecide(at(40_001, 40_000, 0)) {
			t.Fatal("no recorded pass keeps the original floor")
		}
	})
	t.Run("test_skips_when_transcript_has_not_grown_since_last_compaction", func(t *testing.T) {
		if idleDecide(at(44_100, 40_000, 44_000)) {
			t.Fatal("an unchanged transcript sits the round out")
		}
	})
	t.Run("test_fires_once_a_full_floor_of_new_content_accumulated", func(t *testing.T) {
		if !idleDecide(at(84_001, 40_000, 44_000)) {
			t.Fatal("a full floor of new content fires")
		}
	})
	t.Run("test_does_not_fire_at_exactly_the_raised_floor", func(t *testing.T) {
		if idleDecide(at(84_000, 40_000, 44_000)) {
			t.Fatal("exactly the raised floor does not fire")
		}
	})
	t.Run("test_reported_session_stops_recompacting_itself", func(t *testing.T) {
		reported := func(last int) func(*idleDecision) {
			return func(o *idleDecision) {
				o.idleAfter, o.idleGap, o.floor, o.tokens, o.last = 1, 747.0, 25_502, 44_579, last
			}
		}
		if !idleDecide(reported(0)) || idleDecide(reported(44_579)) {
			t.Fatal("the previous pass output raises the floor")
		}
	})
	t.Run("test_an_effective_pass_still_raises_the_floor", func(t *testing.T) {
		if idleDecide(at(30_000, 25_000, 10_000)) || !idleDecide(at(35_001, 25_000, 10_000)) {
			t.Fatal("another pass waits for a floor of new content")
		}
	})
	t.Run("test_other_gates_still_win_over_the_raised_floor", func(t *testing.T) {
		grown := at(200_000, 40_000, 44_000)
		for name, edit := range map[string]func(*idleDecision){
			"cooldown": func(o *idleDecision) { o.cooldown = true },
			"opt out":  func(o *idleDecision) { o.idleAfter = 0 },
			"gap":      func(o *idleDecision) { o.idleGap = 0.5 },
		} {
			if idleDecide(func(o *idleDecision) { grown(o); edit(o) }) {
				t.Fatalf("%s gate lost to growth", name)
			}
		}
	})
}

func TestFewMessagesHugeContentTriggersGate(t *testing.T) {
	caseHome(t)
	rows := make([]map[string]any, 0, 8)
	for range 7 {
		rows = append(rows, map[string]any{"role": "user", "content": "hi"})
	}
	rows = append(rows, map[string]any{"role": "user", "content": strings.Repeat("x", 280_000)})
	if len(rows) > 3+20+1 || !shouldRunPreflightEstimate(context.Background(), rowsToMessages(rows), 3, 20, 64_000) {
		t.Fatal("a few huge messages must trip the estimate gate")
	}
}

func TestContentBelowThresholdDoesNotTrigger(t *testing.T) {
	caseHome(t)
	rows := []map[string]any{{"role": "user", "content": strings.Repeat("x", (64_000-1000)*4)}}
	if shouldRunPreflightEstimate(context.Background(), rowsToMessages(rows), 3, 20, 64_000) {
		t.Fatal("content under the threshold must not trip the gate")
	}
}

func timedOutAgent(window int) *AIAgent {
	a := &AIAgent{}
	if window > 0 {
		a.contextCompressor = newStubEngine(window, 0.75)
	}
	markContextCompressionTimedOut(a)
	return a
}

func TestPreflightTimeoutBlocksUnchangedProviderPayload(t *testing.T) {
	caseHome(t)
	var timedOut *PreflightCompressionTimedOut
	err := failClosedAfterPreflightTimeout(timedOutAgent(0), 190_035)
	if !errors.As(err, &timedOut) || !strings.Contains(err.Error(), "provider call was not sent") {
		t.Fatalf("err %v", err)
	}
}

func TestStructuralNoopKeepsExistingPreflightBehavior(t *testing.T) {
	caseHome(t)
	if err := failClosedAfterPreflightTimeout(&AIAgent{}, 190_035); err != nil {
		t.Fatalf("err %v", err)
	}
}

func TestPreflightTimeoutSendsARequestThatFitsTheModelWindow(t *testing.T) {
	caseHome(t)
	if err := failClosedAfterPreflightTimeout(timedOutAgent(120_000), 99_347); err != nil {
		t.Fatalf("a request under the window is sent, got %v", err)
	}
	var timedOut *PreflightCompressionTimedOut
	if err := failClosedAfterPreflightTimeout(timedOutAgent(1_000_000), 1_313_423); !errors.As(err, &timedOut) {
		t.Fatalf("a request over the window fails closed, got %v", err)
	}
}

const syntheticNote = "[System note: Your previous turn was interrupted mid-run …]\n\nkeep going"

func syntheticAgent(t *testing.T) (*AIAgent, *state.SessionDB) {
	t.Helper()
	db := restoreRealDB(t, "sess-synthetic", "desktop")
	a := newFixtureAgent(t, AIAgentOptions{BaseURL: "https://openrouter.ai/api/v1", SessionDB: db, SessionID: "sess-synthetic", EnabledToolsets: []string{}})
	t.Cleanup(func() { a.Close() })
	a.sessionDBCreated, a.cachedSystemPrompt, a.skipMCPRefresh = true, "SYSTEM", true
	return a, db
}

func syntheticStoredUserRow(t *testing.T, db *state.SessionDB) map[string]any {
	t.Helper()
	var users []map[string]any
	for _, r := range persistConversation(t, db, "sess-synthetic", false) {
		if r["role"] == "user" {
			users = append(users, r)
		}
	}
	if len(users) != 1 {
		t.Fatalf("user rows %v", users)
	}
	return users[0]
}

func TestRowIsTypedByTheTurnStartPersist(t *testing.T) {
	caseHome(t)
	a, db := syntheticAgent(t)
	prologueBuild(t, a, func(r *TurnContextRequest) {
		r.UserMessage, r.PersistUserDisplayKind, r.PersistUserDisplayMetadata = syntheticNote, "auto_continue", map[string]any{"attempt": 2}
	})
	row := syntheticStoredUserRow(t, db)
	if metadata := mustDumps(t, row["display_metadata"]); row["display_kind"] != "auto_continue" || metadata != `{"attempt": 2}` || row["content"] != syntheticNote {
		t.Fatalf("row %v", row)
	}
}

func TestARealUserTurnStaysUntyped(t *testing.T) {
	caseHome(t)
	a, db := syntheticAgent(t)
	prologueBuild(t, a, func(r *TurnContextRequest) { r.UserMessage = "keep going" })
	if kind, ok := syntheticStoredUserRow(t, db)["display_kind"]; ok && kind != nil && kind != "" {
		t.Fatalf("display kind %v", kind)
	}
}

type prologueMemory struct {
	memory.BaseProvider
	mu       sync.Mutex
	prefetch string
	recall   *memory.RecallStatus
	queries  [][2]string
	turns    []memory.TurnOptions
	recalls  int
}

func (p *prologueMemory) Name() string                                                 { return "prologue-memory" }
func (p *prologueMemory) Available(context.Context) bool                               { return true }
func (p *prologueMemory) Initialize(context.Context, string, memory.InitOptions) error { return nil }
func (p *prologueMemory) ToolSchemas(context.Context) ([]map[string]any, error)        { return nil, nil }
func (p *prologueMemory) Prefetch(_ context.Context, query, sessionID string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queries = append(p.queries, [2]string{query, sessionID})
	return p.prefetch, nil
}
func (p *prologueMemory) OnTurnStart(_ context.Context, _ int, _ string, opts memory.TurnOptions) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.turns = append(p.turns, opts)
	return nil
}
func (p *prologueMemory) RecallStatus(context.Context) (*memory.RecallStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recalls++
	return p.recall, nil
}

func prologueMemoryAgent(t *testing.T, prefetch string) (*AIAgent, *prologueMemory) {
	t.Helper()
	a := prologueAgent(t)
	p := &prologueMemory{prefetch: prefetch}
	a.memoryManager = memory.NewMemoryManager(context.Background(), a.env, nil)
	if err := a.memoryManager.AddProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return a, p
}

const deployQuery = "what did we decide about the deploy pipeline?"

func TestTurnContext(t *testing.T) {
	caseHome(t)
	t.Run("test_returns_turn_context_with_user_message_appended", func(t *testing.T) {
		tc := prologueBuild(t, prologueAgent(t), nil)
		last := tc.Messages[len(tc.Messages)-1]
		if tc.UserMessage != "hello" || last.Role != "user" || last.Content != "hello" || last.Timestamp == 0 ||
			tc.CurrentTurnUserIdx != len(tc.Messages)-1 || tc.ActiveSystemPrompt != "SYSTEM" {
			t.Fatalf("context %+v last %+v", tc, last)
		}
	})
	t.Run("test_preflight_timeout_stops_turn_before_provider_boundary", func(t *testing.T) {
		a := prologueAgent(t)
		a.compressionEnabled, a.maxCompressionAttempts = true, 3
		engine := newStubEngine(1_500, 0)
		engine.ThresholdTokens, engine.ProtectFirstN, engine.ProtectLastN = 1_000, 2, 2
		a.contextCompressor = engine
		compressions := 0
		prev := facadeCompressContext
		facadeCompressContext = func(_ context.Context, agent *AIAgent, messages []map[string]any, _ *string, _ compressContextOptions) ([]map[string]any, string, error) {
			compressions++
			markContextCompressionTimedOut(agent)
			return messages, "SYSTEM", nil
		}
		t.Cleanup(func() { facadeCompressContext = prev })
		oversized := []map[string]any{{"role": "assistant", "content": strings.Repeat("x", 8_000)}}
		tc, err := prologueBuildErr(a, func(r *TurnContextRequest) { r.ConversationHistory = oversized })
		var timedOut *PreflightCompressionTimedOut
		if !errors.As(err, &timedOut) || !strings.Contains(err.Error(), "provider call was not sent") || tc != nil || compressions != 1 {
			t.Fatalf("err %v context %v compressions %d", err, tc, compressions)
		}
	})
	t.Run("test_user_message_preserves_platform_event_timestamp", func(t *testing.T) {
		stamp := 123.5
		tc := prologueBuild(t, prologueAgent(t), func(r *TurnContextRequest) { r.PersistUserTimestamp = &stamp })
		if got := tc.Messages[len(tc.Messages)-1].Timestamp; got != 123.5 {
			t.Fatalf("timestamp %v", got)
		}
	})
	t.Run("test_prefetch_skipped_for_trivial_user_message", func(t *testing.T) {
		a, mem := prologueMemoryAgent(t, "REMEMBERED CONTEXT")
		tc := prologueBuild(t, a, func(r *TurnContextRequest) { r.UserMessage = "hi!" })
		if len(mem.queries) != 0 || tc.ExtPrefetchCache != "" {
			t.Fatalf("queries %v cache %q", mem.queries, tc.ExtPrefetchCache)
		}
	})
	t.Run("test_prefetch_runs_for_substantive_user_message", func(t *testing.T) {
		a, mem := prologueMemoryAgent(t, "REMEMBERED CONTEXT")
		tc := prologueBuild(t, a, func(r *TurnContextRequest) { r.UserMessage = deployQuery })
		if !reflect.DeepEqual(mem.queries, [][2]string{{deployQuery, a.sessionID}}) || tc.ExtPrefetchCache != "REMEMBERED CONTEXT" {
			t.Fatalf("queries %v cache %q", mem.queries, tc.ExtPrefetchCache)
		}
	})
	t.Run("test_turn_author_is_normalized_then_reaches_on_turn_start_and_the_agent_stash", func(t *testing.T) {
		a, mem := prologueMemoryAgent(t, "REMEMBERED CONTEXT")
		prologueBuild(t, a, func(r *TurnContextRequest) {
			r.UserMessage, r.TurnAuthor = deployQuery, map[string]any{"id": " bot:al\x00pha ", "name": "Alpha", "is_bot": 1, "x": 1}
		})
		if len(mem.turns) != 1 || mem.turns[0] != (memory.TurnOptions{AuthorID: "bot:alpha", AuthorName: "Alpha", AuthorIsBot: true}) {
			t.Fatalf("turn options %+v", mem.turns)
		}
		if got := stashedTurnAuthor(a); !reflect.DeepEqual(got, map[string]any{"id": "bot:alpha", "name": "Alpha", "is_bot": true}) {
			t.Fatalf("stashed author %v", got)
		}
	})
	t.Run("test_turn_without_author_clears_previous_bot_author", func(t *testing.T) {
		a, mem := prologueMemoryAgent(t, "REMEMBERED CONTEXT")
		prologueBuild(t, a, func(r *TurnContextRequest) {
			r.UserMessage, r.TurnAuthor = "first turn", map[string]any{"id": "bot:alpha", "name": "Alpha", "is_bot": true}
		})
		if stashedTurnAuthor(a)["id"] != "bot:alpha" {
			t.Fatalf("stashed author %v", stashedTurnAuthor(a))
		}
		prologueBuild(t, a, func(r *TurnContextRequest) { r.UserMessage = "second turn" })
		last := mem.turns[len(mem.turns)-1]
		if stashedTurnAuthor(a) != nil || last.AuthorID != "" || last.AuthorIsBot {
			t.Fatalf("stashed author %v last turn %+v", stashedTurnAuthor(a), last)
		}
	})
	t.Run("test_turn_author_reaches_the_agent_through_the_real_facade", func(t *testing.T) {
		script := &thinkingScript{responses: []map[string]any{thinkingMockResponse("ok", "stop"), thinkingMockResponse("ok", "stop")}}
		a, ctx := thinkingLoopAgent(t, script)
		author := map[string]any{"id": "bot:alpha", "name": "Alpha", "is_bot": true}
		if _, err := a.RunConversation(ctx, "hi", RunConversationOptions{TurnAuthor: author}); err != nil {
			t.Fatal(err)
		}
		if got := stashedTurnAuthor(a); !reflect.DeepEqual(got, author) {
			t.Fatalf("stashed author %v", got)
		}
		if _, err := a.RunConversation(ctx, "hi again", RunConversationOptions{}); err != nil {
			t.Fatal(err)
		}
		if stashedTurnAuthor(a) != nil {
			t.Fatalf("stashed author %v", stashedTurnAuthor(a))
		}
	})
	t.Run("test_turn_start_replaces_stale_parent_history_with_compression_child", func(t *testing.T) {
		a := prologueAgent(t)
		compacted := []map[string]any{{"role": "user", "content": "[CONTEXT COMPACTION] summary"}, {"role": "assistant", "content": "child tail"}}
		prev := turnRecoverRotatedSession
		turnRecoverRotatedSession = func(_ context.Context, agent *AIAgent) ([]map[string]any, bool) {
			agent.sessionID = "compression-child"
			return compacted, true
		}
		t.Cleanup(func() { turnRecoverRotatedSession = prev })
		tc := prologueBuild(t, a, func(r *TurnContextRequest) {
			r.ConversationHistory = []map[string]any{{"role": "user", "content": "stale parent"}}
		})
		if a.sessionID != "compression-child" || !strings.HasPrefix(a.currentTurnID, "compression-child:") || currentLogSessionTag(t) != " [compression-child]" {
			t.Fatalf("session %q turn %q log tag %q", a.sessionID, a.currentTurnID, currentLogSessionTag(t))
		}
		last := tc.Messages[len(tc.Messages)-1]
		if len(tc.ConversationHistory) != 2 || len(tc.Messages) != 3 || last.Role != "user" || last.Content != "hello" || last.Timestamp == 0 {
			t.Fatalf("history %d messages %d last %+v", len(tc.ConversationHistory), len(tc.Messages), last)
		}
		for i, want := range compacted {
			if tc.ConversationHistory[i].Content != want["content"] || tc.Messages[i].Content != want["content"] || tc.Messages[i].Role != want["role"] {
				t.Fatalf("row %d history %v message %v", i, tc.ConversationHistory[i].Content, tc.Messages[i].Content)
			}
		}
		for _, m := range tc.Messages {
			if m.Content == "stale parent" {
				t.Fatal("the stale parent history survived the recovery")
			}
		}
	})
	t.Run("test_applies_agent_side_effects", func(t *testing.T) {
		a := prologueAgent(t)
		a.invalidToolRetries = -1
		halted := a.toolGuardrails.HaltDecision()
		prologueBuild(t, a, nil)
		if a.invalidToolRetries != 0 || halted != nil || a.toolGuardrails.HaltDecision() != nil || a.userTurnCount != 1 {
			t.Fatalf("retries %d turns %d", a.invalidToolRetries, a.userTurnCount)
		}
		if a.inflightTurnID != "" || a.currentTaskID == "" || a.currentTurnID == "" {
			t.Fatalf("inflight %q task %q turn %q", a.inflightTurnID, a.currentTaskID, a.currentTurnID)
		}
	})
	t.Run("test_pending_cli_message_uses_clean_override_for_api_local_note", func(t *testing.T) {
		a := prologueAgent(t)
		a.pendingCLIUserMessage = map[string]any{"role": "user", "content": "clean prompt", "_db_persisted": true}
		tc := prologueBuild(t, a, func(r *TurnContextRequest) {
			r.UserMessage, r.PersistUserMessage = "[MODEL NOTE]\n\nclean prompt", "clean prompt"
		})
		last := tc.Messages[len(tc.Messages)-1]
		if last.Content != "[MODEL NOTE]\n\nclean prompt" || !last.Persisted || last.Timestamp == 0 || a.pendingCLIUserMessage != nil {
			t.Fatalf("last %+v pending %v", last, a.pendingCLIUserMessage)
		}
	})
	t.Run("test_recall_indicator_emitted_when_memory_injected", func(t *testing.T) {
		a, mem := prologueMemoryAgent(t, "- recalled fact")
		mem.recall = &memory.RecallStatus{ProviderLabel: "Hindsight", Count: 2, Glyph: "👁️"}
		statuses := captureStatuses(a)
		prologueBuild(t, a, func(r *TurnContextRequest) { r.UserMessage = deployQuery })
		found := false
		for _, s := range *statuses {
			found = found || s == "👁️ Hindsight — recalled 2 memories"
		}
		if !found {
			t.Fatalf("statuses %v", *statuses)
		}
	})
	t.Run("test_recall_indicator_skipped_when_nothing_injected", func(t *testing.T) {
		a, mem := prologueMemoryAgent(t, "")
		mem.recall = &memory.RecallStatus{ProviderLabel: "Hindsight", Count: 2, Glyph: "👁️"}
		statuses := captureStatuses(a)
		prologueBuild(t, a, func(r *TurnContextRequest) { r.UserMessage = deployQuery })
		if mem.recalls != 0 {
			t.Fatalf("recall consulted %d times with nothing injected", mem.recalls)
		}
		for _, s := range *statuses {
			if strings.Contains(s, "👁️") {
				t.Fatalf("indicator emitted %q", s)
			}
		}
	})
	t.Run("test_ensure_db_session_runs_after_system_prompt_restore", func(t *testing.T) {
		db := restoreRealDB(t, "other-session", "cli")
		a := newFixtureAgent(t, AIAgentOptions{BaseURL: "https://openrouter.ai/api/v1", Model: "test/model", Platform: "cli", SessionDB: db,
			SessionID: "fresh-session", EnabledToolsets: []string{}})
		t.Cleanup(func() { a.Close() })
		a.skipMCPRefresh, a.compressionEnabled, a.cachedSystemPrompt = true, false, ""
		prev := loopBuildSystemPrompt
		loopBuildSystemPrompt = func(context.Context, *AIAgent, *string) (string, error) { return "REBUILT-SYSTEM", nil }
		t.Cleanup(func() { loopBuildSystemPrompt = prev })
		prologueBuild(t, a, nil)
		row, err := db.GetSession(t.Context(), "fresh-session")
		if err != nil || row == nil {
			t.Fatalf("session row %v %v", row, err)
		}
		if prompt, _ := row.Get("system_prompt"); prompt != "REBUILT-SYSTEM" || a.cachedSystemPrompt != "REBUILT-SYSTEM" {
			t.Fatalf("stored prompt %v cached %q", prompt, a.cachedSystemPrompt)
		}
	})
}

// Plain dict of the author the prologue stashed on the agent, nil when the turn had none
func stashedTurnAuthor(a *AIAgent) map[string]any {
	author, _ := a.turnAuthor.(*pyjson.Object)
	if author == nil {
		return nil
	}
	return turnAuthorMap(author)
}

// Session tag the logging context stamps on a record emitted by the calling goroutine
func currentLogSessionTag(t *testing.T) string {
	t.Helper()
	h := &sessionTagHandler{}
	lg := logging.GetLogger("agent.test_turn_context_session_tag")
	lg.SetLevel(logging.LevelDebug)
	lg.SetPropagate(false)
	lg.AddHandler(h)
	defer func() { lg.RemoveHandler(h); lg.SetPropagate(true) }()
	lg.Info("probe")
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.tags) != 1 {
		t.Fatalf("probe records %v", h.tags)
	}
	return h.tags[0]
}
