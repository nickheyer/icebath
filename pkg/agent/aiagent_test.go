package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

var _ observability.MetricsAgent = (*AIAgent)(nil)

func TestStreamWriterFenceSupersedesEarlierClaims(t *testing.T) {
	a := &AIAgent{}
	first := a.ClaimStreamWriter()
	if !a.StreamWriterIsCurrent(first) {
		t.Fatal("fresh claim is not current")
	}
	second := a.ClaimStreamWriter()
	if second != first+1 || a.StreamWriterIsCurrent(first) || !a.StreamWriterIsCurrent(second) {
		t.Fatalf("claims %d then %d", first, second)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() { defer wg.Done(); a.StreamWriterIsCurrent(a.ClaimStreamWriter()) }()
	}
	wg.Wait()
	if a.streamWriterToken != second+16 {
		t.Fatalf("token %d after concurrent claims", a.streamWriterToken)
	}
}

func TestToolSnapshotAccessors(t *testing.T) {
	a := &AIAgent{tools: []any{map[string]any{"function": map[string]any{"name": "x"}}, "junk"}}
	if defs := a.ToolDefs(); len(defs) != 1 || snapshotToolName(defs[0]) != "x" {
		t.Fatalf("defs %v", defs)
	}
	if _, ok := a.SnapshotGeneration(); ok {
		t.Fatal("generation set before any record")
	}
	if _, ok := a.SnapshotSelection(); ok {
		t.Fatal("selection set before any record")
	}
	a.RecordSnapshot(7, "sel")
	if g, ok := a.SnapshotGeneration(); !ok || g != 7 {
		t.Fatalf("generation %d %v", g, ok)
	}
	if s, ok := a.SnapshotSelection(); !ok || s != "sel" {
		t.Fatalf("selection %q %v", s, ok)
	}
	a.SetToolSnapshot([]map[string]any{{"function": map[string]any{"name": "y"}}}, map[string]bool{"y": true})
	if len(a.tools) != 1 || !a.ValidToolNames()["y"] || a.ToolDefinitions()[0]["function"].(map[string]any)["name"] != "y" {
		t.Fatalf("snapshot %v %v", a.tools, a.validToolNames)
	}
	a.PublishContextEngineToolNames(map[string]bool{"e": true})
	if a.contextEngineToolNames != nil {
		t.Fatal("publish created a set the agent never had")
	}
	set := map[string]bool{"old": true}
	a.contextEngineToolNames = set
	a.PublishContextEngineToolNames(map[string]bool{"e": true})
	if len(set) != 1 || !set["e"] {
		t.Fatalf("set not replaced in place %v", set)
	}
	if a.ToolPinStore() != nil || a.MemoryToolSchemas() != nil || a.ContextEngineToolSchemas() != nil {
		t.Fatal("absent stores must read as nil")
	}
	a.SetToolsets([]string{"web"}, nil)
	a.SetConfiguredDisabledToolsets([]string{"terminal"})
	if a.EnabledToolsets()[0] != "web" || a.DisabledToolsets() != nil || a.ConfiguredDisabledToolsets()[0] != "terminal" {
		t.Fatal("toolsets not stored")
	}
}

func TestAdoptBuiltBackendOnlyForTheCarriedDelegateDef(t *testing.T) {
	def := map[string]any{"function": map[string]any{"name": "delegate_task", "description": "v1"}}
	a := &AIAgent{tools: []any{def}, delegationBackend: "old"}
	a.AdoptBuiltBackend([]any{map[string]any{"function": map[string]any{"name": "delegate_task", "description": "v2"}}}, "new")
	if a.DelegationBackend() != "old" {
		t.Fatal("a differing definition adopted its backend")
	}
	a.AdoptBuiltBackend([]any{map[string]any{"function": map[string]any{"name": "delegate_task", "description": "v1"}}}, "new")
	if a.DelegationBackend() != "new" {
		t.Fatal("the carried definition did not adopt its backend")
	}
	b := &AIAgent{delegationBackend: "keep"}
	b.AdoptBuiltBackend(nil, "none")
	if b.delegationBackend != "none" {
		t.Fatal("two absent definitions compare equal in hermes")
	}
}

func TestTouchActivityStampsUnderTheLock(t *testing.T) {
	a := &AIAgent{turnLivenessAbortClaim: "claim"}
	prevWall := activityWallNow
	activityWallNow = func() float64 { return 42.5 }
	t.Cleanup(func() { activityWallNow = prevWall })
	a.touchActivity(strings.Repeat("d", 400), "compression", false)
	if a.turnLivenessActivityGeneration != 1 || a.lastActivityTS != 42.5 || a.turnLivenessAbortClaim != nil {
		t.Fatalf("stamp %d %v %v", a.turnLivenessActivityGeneration, a.lastActivityTS, a.turnLivenessAbortClaim)
	}
	if a.lastActivityDesc != state.BoundActivityDescription(strings.Repeat("d", 400)) {
		t.Fatalf("desc %q", a.lastActivityDesc)
	}
	a.TouchActivity("next")
	if a.turnLivenessActivityGeneration != 2 || a.lastActivityProvenance != state.ActivityProvenanceUnknown {
		t.Fatalf("second stamp %d %q", a.turnLivenessActivityGeneration, a.lastActivityProvenance)
	}
	a.resetActivityLabelsAfterTurn()
	if a.lastActivityDesc != "" || a.lastActivityTS != 42.5 {
		t.Fatal("labels reset must keep the timestamp")
	}
}

func TestGetActivitySummaryCarriesBudgetAndTool(t *testing.T) {
	a := &AIAgent{iterationBudget: NewIterationBudget(9), maxIterations: 9, apiCallCount: 3, currentTool: "terminal",
		lastActivityTS: 10, lastActivityDesc: "x", lastActivityProvenance: state.ActivityProvenanceUnknown}
	a.iterationBudget.Consume()
	got := a.GetActivitySummary()
	for k, want := range map[string]any{"current_tool": "terminal", "api_call_count": 3, "max_iterations": 9, "budget_used": 1, "budget_max": 9} {
		if fmt.Sprint(got[k]) != fmt.Sprint(want) {
			t.Fatalf("%s = %v want %v in %v", k, got[k], want, got)
		}
	}
}

func TestGatewayOriginJSON(t *testing.T) {
	if got := gatewayOriginJSON(t.Context(), &AIAgent{platform: "telegram"}); got != "" {
		t.Fatalf("no identity gave %q", got)
	}
	a := &AIAgent{platform: "telegram", chatID: "c1", userID: "u1", userIDAlt: "alt", profileName: "work"}
	want := `{"platform": "telegram", "chat_id": "c1", "chat_name": null, "chat_type": "dm", "user_id": "u1", ` +
		`"user_name": null, "thread_id": null, "user_id_alt": "alt", "profile": "work"}`
	if got := gatewayOriginJSON(t.Context(), a); got != want {
		t.Fatalf("got %s", got)
	}
}

func TestLaunchCwdForSession(t *testing.T) {
	prevGetwd := launchGetwd
	launchGetwd = func() (string, error) { return "/work", nil }
	t.Cleanup(func() { launchGetwd = prevGetwd })
	t.Setenv("TERMINAL_ENV", "")
	if launchCwdForSession("cli") != "/work" || launchCwdForSession("telegram") != "" {
		t.Fatal("local cli family source must stamp the cwd and nothing else")
	}
	t.Setenv("TERMINAL_ENV", " Docker ")
	if launchCwdForSession("cli") != "" {
		t.Fatal("remote backend stamped a cwd")
	}
	t.Setenv("TERMINAL_ENV", " LOCAL ")
	if launchCwdForSession("cli") != "/work" {
		t.Fatal("local backend in any case must stamp the cwd")
	}
}

func TestSmallRunAgentHelpers(t *testing.T) {
	if positiveInt(3) != 3 || positiveInt(0) != 0 || positiveInt(true) != 0 || positiveInt(2.0) != 0 {
		t.Fatal("positive int")
	}
	if poolMayRecoverFromRateLimit(nil) {
		t.Fatal("nil pool cannot rotate")
	}
	if reviewQueueKey(&AIAgent{sessionID: "s"}) != "s" || reviewQueueKey(&AIAgent{}) == "" {
		t.Fatal("review queue key")
	}
}

type recEngine struct {
	ContextEngine
	calls []string
	bound string
}

func (e *recEngine) Base() *ContextEngineBase { return &ContextEngineBase{ContextLength: 1000} }
func (e *recEngine) OnSessionEnd(id string, m []map[string]any) {
	e.calls = append(e.calls, fmt.Sprintf("end %s %d", id, len(m)))
}
func (e *recEngine) OnSessionReset() { e.calls = append(e.calls, "reset") }
func (e *recEngine) OnSessionStart(id string, kw map[string]any) {
	e.calls = append(e.calls, fmt.Sprintf("start %s %v", id, kw))
}
func (e *recEngine) CarryOverNewSessionContext(o, n string) {
	e.calls = append(e.calls, "carry "+o+" "+n)
}
func (e *recEngine) BindSessionState(db *state.DB, id string) error {
	e.bound = id
	return nil
}
func (e *recEngine) BoundSessionID() string { return e.bound }

func TestTransitionContextEngineSessionOrder(t *testing.T) {
	e := &recEngine{}
	a := &AIAgent{contextCompressor: e, sessionID: "new", model: "m", platform: "cli"}
	a.transitionContextEngineSession("old", "", []map[string]any{{}}, true, true, nil)
	want := []string{"end old 1", "reset",
		"start new map[carry_over_context:true context_length:1000 model:m old_session_id:old platform:cli]", "carry old new"}
	if fmt.Sprint(e.calls) != fmt.Sprint(want) {
		t.Fatalf("calls %q", e.calls)
	}
	e.calls = nil
	a.transitionContextEngineSession("", "", nil, false, true, nil)
	if fmt.Sprint(e.calls) != "[reset]" {
		t.Fatalf("bare reset %q", e.calls)
	}
}

func TestResetSessionStateZeroesCountersAndRebinds(t *testing.T) {
	e := &recEngine{}
	a := &AIAgent{contextCompressor: e, sessionID: "s2", sessionTotalTokens: 5, sessionAPICalls: 2,
		sessionEstimatedCostUSD: 1.5, sessionCostStatus: "estimated", userTurnCount: 4, isUserInitiatedTurn: true}
	a.resetSessionState(nil, "", false)
	if a.sessionTotalTokens != 0 || a.sessionAPICalls != 0 || a.sessionEstimatedCostUSD != 0 ||
		a.sessionCostStatus != "unknown" || a.sessionCostSource != "none" || a.userTurnCount != 0 || a.isUserInitiatedTurn {
		t.Fatal("counters not reset")
	}
	if e.bound != "s2" || fmt.Sprint(e.calls) != "[reset]" {
		t.Fatalf("engine %q %q", e.bound, e.calls)
	}
}

func TestEnsureDBSessionCreatesTheRowOnce(t *testing.T) {
	for _, a := range []*AIAgent{{}, {persistDisabled: true}} {
		if err := a.ensureDBSession(); err != nil || a.sessionDBCreated {
			t.Fatal("skip path created a row")
		}
	}
	db, err := state.NewSessionDB(t.Context(), t.TempDir()+"/state.db", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	t.Setenv("TERMINAL_ENV", "docker")
	a := &AIAgent{sessionDB: db, sessionID: "sess-1", model: "m1", platform: "telegram", chatID: "c", userID: "u",
		profileName: "work", parentSessionID: "", env: nil}
	if err := a.ensureDBSession(); err != nil || !a.sessionDBCreated {
		t.Fatalf("row not created %v", err)
	}
	row, err := db.GetSession(t.Context(), "sess-1")
	if err != nil || row == nil {
		t.Fatalf("row %v %v", row, err)
	}
	for k, want := range map[string]any{"source": "telegram", "model": "m1", "chat_id": "c", "user_id": "u"} {
		if got, _ := row.Get(k); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s = %v want %v", k, got, want)
		}
	}
	origin, _ := row.Get("origin_json")
	if !strings.Contains(fmt.Sprint(origin), `"profile": "work"`) {
		t.Fatalf("origin %v", origin)
	}
}

func TestSetStreamDeltaAndToolProgressCallbacksInstallTheSlots(t *testing.T) {
	a := &AIAgent{}
	var got []string
	a.SetStreamDeltaCallback(func(text string) { got = append(got, text) })
	a.SetToolProgressCallback(func(event, name, preview string, args map[string]any, extra map[string]any) {
		got = append(got, event+":"+name)
	})
	if !callQuietly(a.streamDeltaCallback, "hi") {
		t.Fatal("stream delta callback not installed")
	}
	callToolProgress(a, "tool.started", "read_file", "p")
	if strings.Join(got, ",") != "hi,tool.started:read_file" {
		t.Fatalf("got %v", got)
	}
}

func TestSessionMessagesTaskIDAndPersistSessionAccessorsReadTheAgentSlots(t *testing.T) {
	a := &AIAgent{}
	a.currentTaskID = "task-1"
	a.sessionMessages = []*state.Message{{Role: "user", Content: "hi"}}
	if a.TaskID() != "task-1" || len(a.SessionMessages()) != 1 || a.SessionMessages()[0].Role != "user" {
		t.Fatalf("accessors %q %v", a.TaskID(), a.SessionMessages())
	}
	msgs := a.SessionMessages()
	if err := a.PersistSession(&msgs, nil); err != nil {
		t.Fatalf("persist without a store: %v", err)
	}
}

func TestQuietAgentSettersReplaceTheStdoutCallbacks(t *testing.T) {
	a := &AIAgent{toolProgressMode: "all", printFn: func(string) {}}
	a.SetQuietMode(true)
	a.SetSuppressStatusOutput(true)
	a.SetToolGenCallback(nil)
	a.SetReasoningCallback(nil)
	a.SetToolStartCallback(nil)
	a.SetToolCompleteCallback(nil)
	a.SetToolProgressMode("off")
	a.SetPrintFn(nil)
	if !a.quietMode || !a.suppressStatusOutput || a.toolProgressMode != "off" || a.printFn != nil {
		t.Fatalf("quiet setters did not land: %+v", a)
	}
	if a.toolGenCallback != nil || a.reasoningCallback != nil || a.toolStartCallback != nil || a.toolCompleteCallback != nil {
		t.Fatal("callbacks must be cleared")
	}
}

func TestNotificationAccessorsReadTheBoundTurnState(t *testing.T) {
	a := &AIAgent{}
	if a.MuteNotificationReply() || a.NotificationConfig() != nil {
		t.Fatal("a fresh agent mutes nothing and binds no config")
	}
	restore := a.NotificationPolicySnapshot("cli", map[string]any{"display": map[string]any{}})
	if a.NotificationConfig() == nil {
		t.Fatal("the bound config must read back")
	}
	restore()
	_, unmute := a.NotificationTurn(context.Background(), true, "s1")
	if !a.MuteNotificationReply() {
		t.Fatal("a muted turn must read as muted")
	}
	unmute()
	if a.MuteNotificationReply() || a.NotificationConfig() != nil {
		t.Fatal("restores must clear the turn state")
	}
}

func TestEmitNoticeReachesTheNoticeCallback(t *testing.T) {
	var got []provider.AgentNotice
	a := &AIAgent{noticeCallback: func(n provider.AgentNotice) { got = append(got, n) }}
	a.EmitNotice(provider.AgentNotice{Level: "warn", Text: "careful"})
	if len(got) != 1 || got[0].Text != "careful" || got[0].Level != "warn" {
		t.Fatalf("notice callback saw %+v", got)
	}
}

func TestApplyFallbackChainSkipsWhileCooldownHoldsFallback(t *testing.T) {
	live := []map[string]any{{"provider": "deepseek", "model": "deepseek-v4-flash"}}
	a := &AIAgent{fallbackChain: live, fallbackIndex: 1, fallbackActivated: true}
	a.rateLimitedUntil = pythread.Monotonic() + 30
	a.ApplyFallbackChain([]map[string]any{{"provider": "openrouter", "model": "anthropic/claude-sonnet-4.6"}})
	if len(a.fallbackChain) != 1 || a.fallbackChain[0]["provider"] != "deepseek" || a.fallbackIndex != 1 || !a.fallbackActivated {
		t.Fatalf("a live fallback activation was clobbered: %v index %d", a.fallbackChain, a.fallbackIndex)
	}
}

func TestApplyFallbackChainReplacesTheChainOnThePrimary(t *testing.T) {
	a := &AIAgent{fallbackChain: []map[string]any{{"provider": "deepseek", "model": "m"}}, fallbackIndex: 1}
	a.unavailableFallbackKeys = map[[3]string]bool{{"deepseek", "m", ""}: true}
	next := []map[string]any{{"provider": "openrouter", "model": "anthropic/claude-sonnet-4.6"}}
	a.ApplyFallbackChain(next)
	if len(a.fallbackChain) != 1 || a.fallbackChain[0]["provider"] != "openrouter" || a.fallbackIndex != 0 || len(a.unavailableFallbackKeys) != 0 {
		t.Fatalf("chain %v index %d memo %v", a.fallbackChain, a.fallbackIndex, a.unavailableFallbackKeys)
	}
	if got := a.FallbackModel(); got == nil || got["provider"] != "openrouter" {
		t.Fatalf("first entry %v", got)
	}
	a.unavailableFallbackKeys[[3]string{"openrouter", "x", ""}] = true
	a.ApplyFallbackChain(next)
	if len(a.unavailableFallbackKeys) != 1 {
		t.Fatal("an unchanged chain must keep the unavailability memo")
	}
	a.ApplyFallbackChain(nil)
	if len(a.fallbackChain) != 0 || a.FallbackModel() != nil {
		t.Fatalf("a removed chain left %v", a.fallbackChain)
	}
}

func TestStagePendingCLIUserMessageClearsThePreviousTurnOverrides(t *testing.T) {
	idx := 3
	a := &AIAgent{persistUserMessageIdx: &idx, persistUserMessageOverride: "old", persistUserMessageTimestamp: 1.5}
	staged := map[string]any{"role": "user", "content": "hi"}
	appended := false
	a.StagePendingCLIUserMessage(staged, func() { appended = true })
	if a.persistUserMessageIdx != nil || a.persistUserMessageOverride != nil || a.persistUserMessageTimestamp != nil {
		t.Fatal("the previous turn overrides must be cleared")
	}
	if got, _ := a.pendingCLIUserMessage.(map[string]any); got == nil || got["content"] != "hi" || !appended {
		t.Fatalf("staged %v appended %v", a.pendingCLIUserMessage, appended)
	}
	if got := a.PendingCLIUserMessage(); got == nil || got["role"] != "user" {
		t.Fatalf("pending accessor %v", got)
	}
	if i, o, ts := a.PersistUserMessageStaging(); i != nil || o != nil || ts != nil {
		t.Fatalf("staging accessor %v %v %v", i, o, ts)
	}
}

func TestActiveChildInterruptsReportsEachChildFlag(t *testing.T) {
	quiet, cut := &AIAgent{}, &AIAgent{}
	cut.interruptRequested.Store(true)
	a := &AIAgent{activeChildren: []*AIAgent{quiet, cut}, activeChildrenLock: &sync.Mutex{}}
	if got := a.ActiveChildInterrupts(); len(got) != 2 || got[0] || !got[1] {
		t.Fatalf("flags %v", got)
	}
}

func TestStatusBarUsageReportsCountersAndContextOccupancy(t *testing.T) {
	a := &AIAgent{sessionInputTokens: 7, sessionOutputTokens: 3, sessionPromptTokens: 12, sessionCacheReadTokens: 5, sessionAPICalls: 2,
		apiLatencyHistory: []float64{1.5, 2.5}, apiOutputHistory: []int{30, 90}}
	usage := a.StatusBarUsage(context.Background())
	if usage.HasCompressor || usage.Counters["session_prompt_tokens"] != 12 || usage.Counters["session_api_calls"] != 2 || len(usage.LatencyHistory) != 2 {
		t.Fatalf("usage without an engine %+v", usage)
	}
	pin := 200000
	compressor := &ContextCompressor{configContextLength: &pin}
	compressor.LastPromptTokens, compressor.lastRealPromptTokens = 50000, 50000
	compressor.ContextEngineBase.ContextLength, compressor.ContextEngineBase.CompressionCount = 200000, 3
	a.contextCompressor = compressor
	usage = a.StatusBarUsage(context.Background())
	if !usage.HasCompressor || usage.ContextTokens != 50000 || usage.ContextLength != 200000 || usage.ContextPercent == nil || *usage.ContextPercent != 25 ||
		usage.ContextEstimated || !usage.ContextPinned || usage.Compressions != 3 {
		t.Fatalf("usage %+v", usage)
	}
	compressor.LastPromptTokens = -1
	if got := a.StatusBarUsage(context.Background()); got.ContextTokens != 0 {
		t.Fatalf("the post compression sentinel leaked as %d tokens", got.ContextTokens)
	}
	compressor.LastPromptTokens, compressor.lastRealPromptTokens = 60000, 50000
	if got := a.StatusBarUsage(context.Background()); !got.ContextEstimated {
		t.Fatal("a local seed that differs from the provider reading must read as estimated")
	}
	if _, ok := a.TodoItems(); ok {
		t.Fatal("an agent without a todo store reports none")
	}
}
