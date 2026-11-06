package agent

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/observability"
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
	activityWallNow = func() float64 { return 42.5 }
	t.Cleanup(func() { activityWallNow = func() float64 { return 0 } })
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
