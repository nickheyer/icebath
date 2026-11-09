package agent

import (
	"fmt"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func newCLIFlagsAgent(t *testing.T) (*AIAgent, *state.SessionDB) {
	t.Helper()
	db, err := state.NewSessionDB(t.Context(), t.TempDir()+"/state.db", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a := &AIAgent{sessionDB: db, sessionID: "sess-flags", model: "picked-model", platform: "cli", env: &tools.Env{},
		sessionInitModelConfig: map[string]any{"max_iterations": 10, "reasoning_config": map[string]any{"effort": "high"}, "max_tokens": nil}}
	a.reasoningConfig = map[string]any{"effort": "high"}
	return a, db
}

func sessionReasoningConfig(t *testing.T, db *state.SessionDB, sessionID string) string {
	t.Helper()
	v, err := db.GetSessionModelConfigValue(t.Context(), sessionID, "reasoning_config", "missing")
	if err != nil {
		t.Fatal(err)
	}
	text, err := pyjson.Dumps(v, pyjson.SortKeys(true))
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func TestSetPushReasoningPinsTheSessionReasoningConfig(t *testing.T) {
	a, db := newCLIFlagsAgent(t)
	a.reasoningConfig = map[string]any{"effort": "low"}
	a.SetPushReasoning(true)
	if !a.ReasoningPushed() {
		t.Fatal("a user picked level was not pushed")
	}
	if got := sessionReasoningConfig(t, db, "sess-flags"); got != `{"effort": "low"}` {
		t.Fatalf("session reasoning_config %s", got)
	}
	a.reasoningConfig = map[string]any{"enabled": false}
	a.SetPushReasoning(true)
	if !a.ReasoningPushed() || sessionReasoningConfig(t, db, "sess-flags") != `{"enabled": false}` {
		t.Fatalf("disabled reasoning not pinned as none: %v %v", a.ReasoningPushed(), sessionReasoningConfig(t, db, "sess-flags"))
	}
}

func TestSetPushReasoningWithoutALevelOrStoreLeavesNothingPushed(t *testing.T) {
	a, _ := newCLIFlagsAgent(t)
	a.reasoningConfig = nil
	a.SetPushReasoning(true)
	if a.ReasoningPushed() {
		t.Fatal("pushed without a level")
	}
	a.reasoningConfig = map[string]any{"effort": "high"}
	a.SetPushReasoning(false)
	if a.ReasoningPushed() {
		t.Fatal("pushed when the CLI did not ask")
	}
	a.sessionDB = nil
	a.SetPushReasoning(true)
	if a.ReasoningPushed() {
		t.Fatal("pushed without a session store")
	}
}

func TestSetOneTurnKeepsTheSessionRouteAndAppliesTheTurnLevel(t *testing.T) {
	a, db := newCLIFlagsAgent(t)
	a.SetOneTurn(true, map[string]any{"model": "session-model", "reasoning_config": map[string]any{"effort": "medium"},
		"one_turn_reasoning": "low", "one_turn_alias": "fast"})
	if a.OneTurnAlias() != "fast" || a.reasoningConfig["effort"] != "low" || a.reasoningConfig["enabled"] != true {
		t.Fatalf("alias %q reasoning %v", a.OneTurnAlias(), a.reasoningConfig)
	}
	if err := a.ensureDBSession(); err != nil || !a.sessionDBCreated {
		t.Fatalf("row not created %v", err)
	}
	row, err := db.GetSession(t.Context(), "sess-flags")
	if err != nil || row == nil {
		t.Fatalf("row %v %v", row, err)
	}
	if model, _ := row.Get("model"); fmt.Sprint(model) != "session-model" {
		t.Fatalf("session model %v", model)
	}
	if got := sessionReasoningConfig(t, db, "sess-flags"); got != `{"effort": "medium"}` {
		t.Fatalf("session reasoning_config %s", got)
	}
	a.SetOneTurn(false, nil)
	if a.oneTurn || a.OneTurnAlias() != "" || a.sessionRowModel() != "picked-model" {
		t.Fatal("one turn state survived SetOneTurn(false)")
	}
}

func TestSetOneTurnWithoutAPendingLevelKeepsTheAgentReasoning(t *testing.T) {
	a, _ := newCLIFlagsAgent(t)
	a.SetOneTurn(true, map[string]any{"model": "session-model"})
	if fmt.Sprint(a.reasoningConfig) != "map[effort:high]" || a.oneTurnSessionReasoningSet {
		t.Fatalf("reasoning %v set %v", a.reasoningConfig, a.oneTurnSessionReasoningSet)
	}
	if cfg := a.sessionRowModelConfig(); fmt.Sprint(cfg["reasoning_config"]) != "map[effort:high]" {
		t.Fatalf("row config %v", cfg)
	}
}

func TestControlUpdateCallbackFiresWhenAutomationStateMoves(t *testing.T) {
	a, db := newCLIFlagsAgent(t)
	calls := 0
	a.SetControlUpdateCallback(func() { calls++ })
	a.NoteToolResult("read_file", "c1", "ok", "ok")
	if calls != 0 {
		t.Fatalf("published %d times with no automation state", calls)
	}
	goal, err := NewGoalState("ship it").ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta(t.Context(), goalMetaKey("sess-flags"), goal); err != nil {
		t.Fatal(err)
	}
	a.NoteToolResult("goal_tool", "c2", "ok", "ok")
	a.NoteToolResult("read_file", "c3", "ok", "ok")
	if calls != 1 {
		t.Fatalf("published %d times for one goal change", calls)
	}
	state := NewGoalState("ship it")
	state.TurnsUsed = 2
	moved, _ := state.ToJSON()
	if err := db.SetMeta(t.Context(), goalMetaKey("sess-flags"), moved); err != nil {
		t.Fatal(err)
	}
	a.NoteToolResult("goal_tool", "c4", "ok", "ok")
	if calls != 2 {
		t.Fatalf("published %d times after the goal advanced", calls)
	}
	cleared := NewGoalState("ship it")
	cleared.Status = "cleared"
	gone, _ := cleared.ToJSON()
	if err := db.SetMeta(t.Context(), goalMetaKey("sess-flags"), gone); err != nil {
		t.Fatal(err)
	}
	a.NoteToolResult("goal_tool", "c5", "ok", "ok")
	if calls != 3 || a.controlRevision != "" {
		t.Fatalf("calls %d revision %q after the goal cleared", calls, a.controlRevision)
	}
}

func TestControlSnapshotRevisionCoversLoopAndHeartbeat(t *testing.T) {
	a, db := newCLIFlagsAgent(t)
	empty, err := a.controlSnapshotRevision()
	if err != nil || empty != "" {
		t.Fatalf("revision %q %v with no state", empty, err)
	}
	loop, _ := NewSessionLoopState("poll").ToJSON()
	if err := db.SetMeta(t.Context(), loopMetaPrefix+"sess-flags", loop); err != nil {
		t.Fatal(err)
	}
	withLoop, err := a.controlSnapshotRevision()
	if err != nil || len(withLoop) != 64 {
		t.Fatalf("revision %q %v with a loop", withLoop, err)
	}
	hb, _ := NewHeartbeatState("beat", 60).ToJSON()
	if err := db.SetMeta(t.Context(), heartbeatMetaPrefix+"sess-flags", hb); err != nil {
		t.Fatal(err)
	}
	withBoth, err := a.controlSnapshotRevision()
	if err != nil || withBoth == withLoop {
		t.Fatalf("revision %q %v unchanged by a heartbeat", withBoth, err)
	}
	again, _ := a.controlSnapshotRevision()
	if again != withBoth {
		t.Fatal("equal reads gave different revisions")
	}
}

func TestSetFiniteMarksTheEnvForFiniteTurnRequired(t *testing.T) {
	a, _ := newCLIFlagsAgent(t)
	if _, known := tools.FiniteTurnRequired(a.env); known {
		t.Fatal("fresh env already carries a finite flag")
	}
	a.SetFinite(true)
	if finite, known := tools.FiniteTurnRequired(a.env); !finite || !known || !a.finite {
		t.Fatalf("finite=%v known=%v", finite, known)
	}
	a.SetFinite(false)
	if finite, known := tools.FiniteTurnRequired(a.env); finite || !known {
		t.Fatalf("finite=%v known=%v after SetFinite(false)", finite, known)
	}
	bare := &AIAgent{}
	bare.SetFinite(true)
	if !bare.finite {
		t.Fatal("finite flag lost without an env")
	}
}

func TestSetAutoLoadSkillsResultSeedsThePromptCache(t *testing.T) {
	a := &AIAgent{}
	a.SetAutoLoadSkillsResult("<skills/>", []string{"git"}, []string{"nope"})
	if !a.autoLoadSkillsResolved || a.autoLoadSkillsResult.prompt != "<skills/>" ||
		fmt.Sprint(a.autoLoadSkillsResult.loaded) != "[git]" || fmt.Sprint(a.autoLoadSkillsResult.missing) != "[nope]" {
		t.Fatalf("result %+v resolved %v", a.autoLoadSkillsResult, a.autoLoadSkillsResolved)
	}
}
