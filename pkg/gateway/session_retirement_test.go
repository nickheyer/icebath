package gateway

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestClosedLocalConversationLeavesMemoryAndComesBackWithItsSource(t *testing.T) {
	ctx, a, runner := newRecoverAuthority(t)
	ref, physical := seedRecoverSession(t, ctx, a, recoverActor(), recoverTUIParams(t.TempDir()))
	live, _ := a.live(ref.SessionID)
	source, route := live.Source, live.Route
	stale := pyjson.NewObject()
	if retired, err := retireClosed(ctx, a, ref.SessionID, live, physical, stale); err != nil || retired {
		t.Fatalf("a close a later turn superseded retired the conversation: %v %v", retired, err)
	}
	if retired, err := retireAtIdle(ctx, a, ref, live); err != nil || retired {
		t.Fatalf("the idle exit retired a conversation reopened since its close: %v %v", retired, err)
	}
	a.mu.Lock()
	live.Subscribers = map[string]Principal{"viewer": recoverActor()}
	a.mu.Unlock()
	if retired, err := retireClosed(ctx, a, ref.SessionID, live, physical, live.EventStream.Execution); err != nil || retired {
		t.Fatalf("a watched conversation retired: %v %v", retired, err)
	}
	a.mu.Lock()
	live.Subscribers = nil
	a.mu.Unlock()
	if retired, err := retireAtIdle(ctx, a, ref, live); err != nil || !retired {
		t.Fatalf("the idle exit kept a closed conversation nobody views: %v %v", retired, err)
	}
	if recoverLive(a, ref.SessionID) {
		t.Fatal("the retired conversation is still in memory")
	}
	a.mu.Lock()
	local, held := a.retiredLocal[ref.SessionID], a.retiredRoutes[ref.SessionID]
	a.mu.Unlock()
	if !local || held != route {
		t.Fatalf("retired local %v route %q", local, held)
	}
	adapter, err := localAdapter(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if adapter.(*LocalSessionAdapter).policy(ref.SessionID) == nil {
		t.Fatal("the retired conversation lost its frozen policy")
	}
	if err := restoreRetired(ctx, a, ref.SessionID); err != nil {
		t.Fatal(err)
	}
	restored, ok := a.live(ref.SessionID)
	if !ok || restored == live || restored.Source != source || restored.Route != route {
		t.Fatalf("restored %+v", restored)
	}
	if !adapter.(*LocalSessionAdapter).authorizeSource(source) {
		t.Fatal("the source the conversation was admitted with is no longer authorized")
	}
	a.mu.Lock()
	local, timer := a.retiredLocal[ref.SessionID], restored.RetireTimer
	a.mu.Unlock()
	if local || timer == nil {
		t.Fatalf("back in memory yet retired local %v, timer %v", local, timer)
	}
	timer.Stop()
	retireRestored(ctx, a, ref.SessionID, restored)
	if recoverLive(a, ref.SessionID) {
		t.Fatal("an unwatched restored conversation did not retire again")
	}
	if err := forgetRetired(ctx, a, ref.SessionID); err != nil {
		t.Fatal(err)
	}
	if adapter.(*LocalSessionAdapter).policy(ref.SessionID) != nil || adapter.(*LocalSessionAdapter).savedSource(ref.SessionID) != nil {
		t.Fatal("a deleted retired conversation kept its source or policy")
	}
	if len(runner.evicted) != 1 || runner.evicted[0] != route {
		t.Fatalf("evicted %v", runner.evicted)
	}
}

func TestApiConversationRetiresBetweenRequestsUnlessInputIsPending(t *testing.T) {
	ctx, a, _ := newAPIOwner(t)
	apiCreate(t, a, "api-1", "api_server")
	ref := apiBind(t, ctx, a, "api-1")
	live, _ := a.live("api-1")
	if _, err := a.DB.AdmitSessionInput(ctx, state.SessionInput{Epoch: a.Epoch, PrincipalID: "api", SessionID: "api-1",
		RequestID: "r1", Payload: pyjson.ObjectOf("text", "hello")}); err != nil {
		t.Fatal(err)
	}
	if busy, err := viewerBusy(ctx, a, "api-1", "api-1"); err != nil || !busy {
		t.Fatalf("queued input does not read busy: %v %v", busy, err)
	}
	if retired, err := retireAtIdle(ctx, a, ref, live); err != nil || retired {
		t.Fatalf("a conversation with queued input retired: %v %v", retired, err)
	}
	if _, _, err := a.DB.ClaimSessionInput(ctx, state.ClaimInput{Epoch: a.Epoch, SessionID: "api-1"}); err != nil {
		t.Fatal(err)
	}
	if retired, err := retireAtIdle(ctx, a, ref, live); err != nil || retired {
		t.Fatalf("a conversation with a started turn retired: %v %v", retired, err)
	}
	if pinned(ctx, a, "api-1", live) {
		t.Fatal("ledger rows alone pinned the live session")
	}
	a.mu.Lock()
	a.executing = map[string]int64{"api-1": 1}
	a.mu.Unlock()
	if !pinned(ctx, a, "api-1", live) {
		t.Fatal("an executing conversation is not pinned")
	}
	if target, err := viewerPhysicalTarget(ctx, a, "api-1"); err != nil || target != "api-1" {
		t.Fatalf("physical target %q %v", target, err)
	}
	if policy, err := policyForSource(ctx, a.Runner, live.Source); err != nil || policy != nil {
		t.Fatalf("an API source resolved a local policy: %v %v", policy, err)
	}
}

func TestApiBindingOffTheLoopReopensAndContinuesItsChain(t *testing.T) {
	ctx, a, runner := newAPIOwner(t)
	result, err := runStepsOffLoop(ctx, a, bindAPISteps(ctx, a, "api-off", apiBindOptions{}), nil)
	if err != nil || result.(SessionRef).SessionID != "api-off" || !recoverLive(a, "api-off") {
		t.Fatalf("off loop bind %v %v", result, err)
	}
	_, err = runStepsOffLoop(ctx, a, bindAPISteps(ctx, a, "../escape", apiBindOptions{}), nil)
	recoverRefused(t, err, "invalid_params")
	ref := SessionRef{ProfileID: "default", SessionID: "api-off"}
	if err := a.DB.EndSession("api-off", "cli_close"); err != nil {
		t.Fatal(err)
	}
	if err := reopenAPISession(ctx, a, ref); err != nil {
		t.Fatal(err)
	}
	row, err := a.DB.GetSession(ctx, "api-off")
	if err != nil {
		t.Fatal(err)
	}
	if reason, _ := row.Get("end_reason"); reason != nil {
		t.Fatalf("the admitted turn left end_reason %v", reason)
	}
	if next, err := continuedAPISession(ctx, runner, "api-off", ""); err != nil || next != "api-off" {
		t.Fatalf("a retained chain moved to %q, %v", next, err)
	}
	if next, err := continuedAPISession(ctx, runner, "", ""); err != nil || next != "" {
		t.Fatalf("an empty chain resolved %q, %v", next, err)
	}
	runner.draining = true
	_, err = bindAPISession(ctx, a, "api-late", apiBindOptions{})
	recoverRefused(t, err, "runtime_draining")
}

func TestBootRecoveryRestoresOpenReceiptsAndReopensOnTheFirstTurn(t *testing.T) {
	ctx, first, _ := newRecoverAuthority(t)
	actor := recoverActor()
	open, _ := seedRecoverSession(t, ctx, first, actor, recoverGUIParams("open", t.TempDir()))
	ended, endedPhysical := seedRecoverSession(t, ctx, first, actor, recoverGUIParams("ended", t.TempDir()))
	if err := first.DB.EndSession(endedPhysical, "tui_shutdown"); err != nil {
		t.Fatal(err)
	}
	if err := releaseHandedOffReceipts(ctx, first.DB, first.Epoch); err != nil {
		t.Fatal(err)
	}
	ids, err := localReceiptIDs(ctx, first)
	if err != nil || len(ids) != 1 || ids[0] != open.SessionID {
		t.Fatalf("boot receipts %v %v", ids, err)
	}
	second := recoverSecondOwner(t, ctx, first.DB)
	if !recoverLive(second, open.SessionID) || recoverLive(second, ended.SessionID) {
		t.Fatal("boot restored the wrong receipts")
	}
	if err := second.Authorize(ctx, actor, ended, "session:read"); err != nil {
		t.Fatal(err)
	}
	if !recoverLive(second, ended.SessionID) {
		t.Fatal("the first touch did not restore the ended session")
	}
	if err := reopenLocalSession(ctx, second, ended); err != nil {
		t.Fatal(err)
	}
	row, err := second.DB.GetSession(ctx, endedPhysical)
	if err != nil {
		t.Fatal(err)
	}
	if reason, _ := row.Get("end_reason"); reason != nil {
		t.Fatalf("the first admitted turn left end_reason %v", reason)
	}
	owner, err := messagingOwnerRef(ctx, second, open)
	if err != nil || owner != open {
		t.Fatalf("a local row resumed on %v, %v", owner, err)
	}
	if handed, err := handedOffRef(ctx, second, open.SessionID); err != nil || handed != nil {
		t.Fatalf("a local conversation no lane holds read as handed off: %v %v", handed, err)
	}
	authority, localOwner, head, err := conversationHead(ctx, second.Runner, open.SessionID)
	if err != nil || authority != second || localOwner == nil || *localOwner != open.SessionID || head == "" {
		t.Fatalf("conversation head %v %v %q %v", authority, localOwner, head, err)
	}
	live, _ := second.live(open.SessionID)
	route := live.Route
	second.mu.Lock()
	delete(second.Sessions, open.SessionID)
	second.mu.Unlock()
	if err := restoreCompletionRoute(ctx, second.Runner, route); err != nil {
		t.Fatal(err)
	}
	if !recoverLive(second, open.SessionID) {
		t.Fatal("a completion did not restore the session its route names")
	}
}

// Built agent stand in of the session info cases
type infoTestAgent struct {
	tools int
}

func (infoTestAgent) Model() string       { return "m" }
func (infoTestAgent) Provider() string    { return "custom" }
func (infoTestAgent) BaseURL() string     { return "" }
func (infoTestAgent) APIMode() string     { return "chat_completions" }
func (infoTestAgent) APIKey() string      { return "sk-abcdefghijklmnop1234" }
func (infoTestAgent) ServiceTier() string { return "" }
func (infoTestAgent) ReasoningConfig() map[string]any {
	return map[string]any{"enabled": true, "effort": "high"}
}
func (a infoTestAgent) SessionRuntime() SessionRuntimeFacts { return a }
func (a infoTestAgent) ToolDefinitions() []map[string]any {
	return make([]map[string]any, a.tools)
}

func TestSessionInfoFieldsOfABuiltAndALazySession(t *testing.T) {
	ctx, a, _ := newRecoverAuthority(t)
	ref, physical := seedRecoverSession(t, ctx, a, recoverActor(), recoverTUIParams(t.TempDir()))
	if _, err := a.DB.SetSessionTitle(ctx, physical, "Some Title"); err != nil {
		t.Fatal(err)
	}
	live, _ := a.live(ref.SessionID)
	policy, err := policyForSource(ctx, a.Runner, live.Source)
	if err != nil || policy == nil {
		t.Fatalf("policy %v %v", policy, err)
	}
	plain := func(o *pyjson.Object) map[string]any { return pyjson.Plain(o).(map[string]any) }
	built := plain(classicCLIFields(ctx, a, ref, live, infoTestAgent{tools: 3}, policy))
	if built["title"] != "Some Title" || built["yolo"] != false || built["api_mode"] != "chat_completions" ||
		built["tool_count"] != 3 || built["credential_hint"] != "sk-a...1234" {
		t.Fatalf("built fields %v", built)
	}
	if _, isFloat := built["started_at"].(float64); !isFloat {
		t.Fatalf("started_at %T", built["started_at"])
	}
	if _, isFloat := built["last_active_at"].(float64); !isFloat {
		t.Fatalf("last_active_at %T", built["last_active_at"])
	}
	lazy := plain(classicCLIFields(ctx, a, ref, live, nil, policy))
	if lazy["title"] != "Some Title" || lazy["tool_count"] != nil || lazy["credential_hint"] != nil || lazy["api_mode"] != nil {
		t.Fatalf("lazy fields %v", lazy)
	}
	effort, err := reasoningFields(ctx, infoTestAgent{}, policy, "m", a, nil)
	if err != nil || plain(effort)["reasoning_effort"] != "high" {
		t.Fatalf("built effort %v %v", effort, err)
	}
	none, err := reasoningFields(ctx, nil, nil, "m", a, nil)
	if err != nil || none.Len() != 0 {
		t.Fatalf("no agent and no policy answered %v %v", none, err)
	}
	next, err := reasoningFields(ctx, nil, policy, "m", a, map[string]any{"enabled": false})
	if err != nil || plain(next)["reasoning_effort"] != "none" {
		t.Fatalf("a typed reasoning override answered %v %v", next, err)
	}
	model, err := lazyModel(ctx, a, policy)
	if err != nil || model != "m" {
		t.Fatalf("lazy model %v %v", model, err)
	}
}
