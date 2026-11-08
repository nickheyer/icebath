package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"weak"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Weakly referencing stand in for a live child agent
type fakeChild struct {
	mu          sync.Mutex
	steered     []string
	acceptSteer bool
	parent      func() any
}

func newFakeChild(parent any) *fakeChild {
	c := &fakeChild{acceptSteer: true}
	if parent != nil {
		c.parent = func() any { return parent }
	}
	return c
}

func (c *fakeChild) Steer(text string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.acceptSteer {
		return false
	}
	c.steered = append(c.steered, text)
	return true
}

func (c *fakeChild) steeredTexts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.steered...)
}

func (c *fakeChild) delegateParentAgent() any {
	if c.parent == nil {
		return nil
	}
	return c.parent()
}

func (c *fakeChild) liveTranscript() any { return "/tmp/live/task-0.log" }

func registerFake(sid string, child any, extra map[string]any) {
	record := map[string]any{
		"subagent_id": sid, "parent_id": nil, "depth": 0, "goal": "test goal", "model": "test-model",
		"started_at": 1000.0, "status": "running", "tool_count": 0, "agent": child,
	}
	for k, v := range extra {
		record[k] = v
	}
	registerSubagent(record)
}

func controlJSON(t *testing.T, action string, sid, message any, parent *AIAgent) map[string]any {
	t.Helper()
	out, err := handleDelegateControlAction(action, sid, message, parent)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("not json %q", out)
	}
	return m
}

func controlOut(t *testing.T, action string, sid, message any, parent *AIAgent) string {
	t.Helper()
	out, err := handleDelegateControlAction(action, sid, message, parent)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGrandchildIsDescendant(t *testing.T) {
	parent := &AIAgent{}
	mid := newFakeChild(parent)
	grandchild := newFakeChild(mid)
	if !isDescendantOf(grandchild, parent, 8) {
		t.Fatal("grandchild not a descendant")
	}
}

func TestMissingRefIsNotDescendant(t *testing.T) {
	parent := &AIAgent{}
	if isDescendantOf(newFakeChild(nil), parent, 8) || isDescendantOf(nil, parent, 8) {
		t.Fatal("orphan counted as descendant")
	}
}

func TestDeadParentRefIsNotDescendant(t *testing.T) {
	ref := weak.Make(&AIAgent{sessionID: "gone"})
	child := &fakeChild{acceptSteer: true, parent: func() any {
		if p := ref.Value(); p != nil {
			return p
		}
		return nil
	}}
	for range 5 {
		runtime.GC()
		if ref.Value() == nil {
			break
		}
	}
	if isDescendantOf(child, &AIAgent{}, 8) {
		t.Fatal("dead parent counted")
	}
}

func TestListShowsOnlyOwnChildren(t *testing.T) {
	parent := &AIAgent{}
	mine := newFakeChild(parent)
	foreign := newFakeChild(&AIAgent{})
	registerFake("sid-ctl-list-1", mine, nil)
	registerFake("sid-ctl-list-2", foreign, nil)
	defer unregisterSubagent("sid-ctl-list-1", nil)
	defer unregisterSubagent("sid-ctl-list-2", nil)
	out := controlJSON(t, "list", nil, nil, parent)
	if out["count"] != 1.0 {
		t.Fatalf("count %v", out["count"])
	}
	entry := out["subagents"].([]any)[0].(map[string]any)
	if entry["subagent_id"] != "sid-ctl-list-1" || entry["goal"] != "test goal" || entry["accepting_steer"] != true ||
		entry["live_transcript"] != "/tmp/live/task-0.log" {
		t.Fatalf("entry %v", entry)
	}
	if _, ok := entry["agent"]; ok {
		t.Fatal("agent leaked")
	}
	if _, ok := entry["owner_transport"]; ok {
		t.Fatal("owner_transport leaked")
	}
}

func TestSteerReachesOwnedChild(t *testing.T) {
	parent := &AIAgent{}
	child := newFakeChild(parent)
	registerFake("sid-ctl-steer-1", child, nil)
	defer unregisterSubagent("sid-ctl-steer-1", nil)
	out := controlJSON(t, "steer", "sid-ctl-steer-1", "focus on X", parent)
	if out["status"] != "queued" || fmt.Sprint(child.steeredTexts()) != "[focus on X]" {
		t.Fatalf("out %v steered %v", out, child.steeredTexts())
	}
}

func TestSteerForeignChildIsRefused(t *testing.T) {
	parent := &AIAgent{}
	foreign := newFakeChild(&AIAgent{})
	registerFake("sid-ctl-steer-2", foreign, nil)
	defer unregisterSubagent("sid-ctl-steer-2", nil)
	out := controlOut(t, "steer", "sid-ctl-steer-2", "hijack", parent)
	if !strings.Contains(out, "No live subagent") || len(foreign.steeredTexts()) != 0 {
		t.Fatalf("out %q", out)
	}
}

func TestSteerClosedAcceptanceIsRefused(t *testing.T) {
	parent := &AIAgent{}
	child := newFakeChild(parent)
	registerFake("sid-ctl-steer-4", child, map[string]any{"accepting_steer": false})
	defer unregisterSubagent("sid-ctl-steer-4", nil)
	out := controlOut(t, "steer", "sid-ctl-steer-4", "late", parent)
	if !strings.Contains(out, "no longer accepting") || len(child.steeredTexts()) != 0 {
		t.Fatalf("out %q", out)
	}
}

func swapHardInterrupt(t *testing.T) *[]any {
	var interrupted []any
	prev := subagentHardInterrupt
	subagentHardInterrupt = func(agent any, reason string) bool {
		interrupted = append(interrupted, agent)
		return true
	}
	t.Cleanup(func() { subagentHardInterrupt = prev })
	return &interrupted
}

func TestStopInterruptsOwnedChild(t *testing.T) {
	parent := &AIAgent{}
	child := newFakeChild(parent)
	registerFake("sid-ctl-stop-1", child, nil)
	defer unregisterSubagent("sid-ctl-stop-1", nil)
	interrupted := swapHardInterrupt(t)
	out := controlJSON(t, "stop", "sid-ctl-stop-1", nil, parent)
	if out["status"] != "interrupt_requested" || len(*interrupted) != 1 || (*interrupted)[0] != any(child) {
		t.Fatalf("out %v interrupted %v", out, *interrupted)
	}
}

func TestStopForeignChildIsRefused(t *testing.T) {
	parent := &AIAgent{}
	foreign := newFakeChild(&AIAgent{})
	registerFake("sid-ctl-stop-2", foreign, nil)
	defer unregisterSubagent("sid-ctl-stop-2", nil)
	interrupted := swapHardInterrupt(t)
	out := controlOut(t, "stop", "sid-ctl-stop-2", nil, parent)
	if !strings.Contains(out, "No live subagent") || len(*interrupted) != 0 {
		t.Fatalf("out %q", out)
	}
}

func TestDelegateTaskControlActionBypassesSpawnPause(t *testing.T) {
	SetSpawnPaused(true)
	defer SetSpawnPaused(false)
	out, err := delegateTask(context.Background(), &AIAgent{}, map[string]any{"action": "list"})
	var m map[string]any
	if err != nil || json.Unmarshal([]byte(out), &m) != nil || m["action"] != "list" {
		t.Fatalf("out %q err %v", out, err)
	}
}

func TestDelegateTaskUnknownActionIsAnError(t *testing.T) {
	out, err := delegateTask(context.Background(), &AIAgent{}, map[string]any{"action": "pause", "goal": "g"})
	if err != nil || !strings.Contains(out, "Unknown action") {
		t.Fatalf("out %q err %v", out, err)
	}
}

func TestDelegateTaskRequiresParentAgentForControl(t *testing.T) {
	out, err := delegateTask(context.Background(), nil, map[string]any{"action": "list"})
	if err != nil || !strings.Contains(out, "requires a parent agent") {
		t.Fatalf("out %q err %v", out, err)
	}
}

func TestEmptyTasksArrayWithGoalIsSingleTaskNotBatchError(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out, err := delegateTask(context.Background(), &AIAgent{}, map[string]any{"tasks": []any{}, "goal": ""})
	if err != nil || !strings.Contains(out, "No tasks provided") || strings.Contains(out, "at least 2 tasks") {
		t.Fatalf("out %q err %v", out, err)
	}
}

func TestListFindsChildAfterParentAgentRebuild(t *testing.T) {
	oldParent := &AIAgent{sessionID: "sess-durable-1"}
	child := newFakeChild(oldParent)
	registerFake("sid-durable-list-1", child, map[string]any{"owner_agent_session_id": "sess-durable-1"})
	defer unregisterSubagent("sid-durable-list-1", nil)
	rebuilt := &AIAgent{sessionID: "sess-durable-1"}
	if isDescendantOf(child, rebuilt, 8) {
		t.Fatal("identity chain reached the rebuilt parent")
	}
	out := controlJSON(t, "list", nil, nil, rebuilt)
	if out["count"] != 1.0 || out["subagents"].([]any)[0].(map[string]any)["subagent_id"] != "sid-durable-list-1" {
		t.Fatalf("out %v", out)
	}
}

func TestSteerResolvesAfterParentAgentRebuild(t *testing.T) {
	child := newFakeChild(&AIAgent{sessionID: "sess-durable-2"})
	registerFake("sid-durable-steer-1", child, map[string]any{"owner_agent_session_id": "sess-durable-2"})
	defer unregisterSubagent("sid-durable-steer-1", nil)
	out := controlJSON(t, "steer", "sid-durable-steer-1", "keep going", &AIAgent{sessionID: "sess-durable-2"})
	if out["status"] != "queued" || fmt.Sprint(child.steeredTexts()) != "[keep going]" {
		t.Fatalf("out %v", out)
	}
}

func TestDurableOwnershipDoesNotLeakToForeignSession(t *testing.T) {
	child := newFakeChild(&AIAgent{sessionID: "sess-durable-3"})
	registerFake("sid-durable-foreign-1", child, map[string]any{"owner_agent_session_id": "sess-durable-3"})
	defer unregisterSubagent("sid-durable-foreign-1", nil)
	intruder := &AIAgent{sessionID: "sess-other-99"}
	out := controlOut(t, "steer", "sid-durable-foreign-1", "hijack", intruder)
	if !strings.Contains(out, "No live subagent") || len(child.steeredTexts()) != 0 {
		t.Fatalf("out %q", out)
	}
	if out2 := controlJSON(t, "list", nil, nil, intruder); out2["count"] != 0.0 {
		t.Fatalf("list %v", out2)
	}
}

func TestDurableOwnershipResolvesCompressionLineage(t *testing.T) {
	db, err := state.NewSessionDB(context.Background(), filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, n := range []state.NewSession{{ID: "sess-old-tip", Source: "cli"}, {ID: "sess-new-tip", Source: "cli", ParentSessionID: "sess-old-tip"}} {
		if err := db.CreateSession(n); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.EndSession("sess-old-tip", "compression"); err != nil {
		t.Fatal(err)
	}
	registerFake("sid-durable-lineage-1", newFakeChild(nil), map[string]any{"owner_agent_session_id": "sess-old-tip"})
	defer unregisterSubagent("sid-durable-lineage-1", nil)
	out := controlJSON(t, "list", nil, nil, &AIAgent{sessionID: "sess-new-tip", sessionDB: db})
	if out["count"] != 1.0 {
		t.Fatalf("out %v", out)
	}
}

func TestOwnsSubagentRecordRequiresSomeSpine(t *testing.T) {
	child := newFakeChild(nil)
	if ownsSubagentRecord(map[string]any{"subagent_id": "x", "agent": child}, &AIAgent{sessionID: "sess-a"}) {
		t.Fatal("record without spine owned")
	}
	if ownsSubagentRecord(map[string]any{"subagent_id": "y", "agent": child, "owner_agent_session_id": "s1"}, &AIAgent{}) {
		t.Fatal("parent without session owned")
	}
}

func TestAttributionResolvesLiveChild(t *testing.T) {
	child := newFakeChild(&AIAgent{sessionID: "sess-attr-1"})
	registerFake("sa-0-attr0001", child, map[string]any{"delegation_id": "deleg_attr_1", "owner_agent_session_id": "sess-attr-1"})
	defer unregisterSubagent("sa-0-attr0001", nil)
	info := GetSubagentAttribution("sa-0-attr0001")
	if info == nil || info["subagent_id"] != "sa-0-attr0001" || info["goal"] != "test goal" || info["delegation_id"] != "deleg_attr_1" {
		t.Fatalf("info %v", info)
	}
}

func TestAttributionSurvivesChildCompletion(t *testing.T) {
	child := newFakeChild(&AIAgent{sessionID: "sess-attr-2"})
	registerFake("sa-0-attr0002", child, map[string]any{"delegation_id": "deleg_attr_2", "owner_agent_session_id": "sess-attr-2"})
	unregisterSubagent("sa-0-attr0002", nil)
	info := GetSubagentAttribution("sa-0-attr0002")
	if info == nil || info["delegation_id"] != "deleg_attr_2" || info["goal"] != "test goal" {
		t.Fatalf("info %v", info)
	}
}

func TestAttributionUnknownTaskIDIsNone(t *testing.T) {
	if GetSubagentAttribution("proc-not-a-subagent") != nil || GetSubagentAttribution("") != nil {
		t.Fatal("unknown task attributed")
	}
}

func childCompletionEvt(taskID, sid string) map[string]any {
	return map[string]any{
		"type": "completion", "session_id": sid, "task_id": taskID, "command": "npm ci", "exit_code": 0,
		"output": "added 1500 packages",
	}
}

func surfaceEnv(t *testing.T, surface bool) *tools.Env {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cfg := config.Defaults()
	if surface {
		if err := cfg.Set("delegation.surface_child_process_notifications", true); err != nil {
			t.Fatal(err)
		}
	}
	return &tools.Env{Config: cfg}
}

func TestCompletionNotificationCarriesDelegationAttribution(t *testing.T) {
	child := newFakeChild(&AIAgent{sessionID: "sess-attr-3"})
	registerFake("sa-1-attr0003", child, map[string]any{"delegation_id": "deleg_attr_3", "goal": "run the npm ci for the desktop app"})
	defer unregisterSubagent("sa-1-attr0003", nil)
	reg := tools.NewProcessRegistry()
	reg.CompletionQueue.Put(childCompletionEvt("sa-1-attr0003", "proc_deadbeef0001"))
	results := reg.DrainNotifications(surfaceEnv(t, true), "", nil, false)
	if len(results) != 1 {
		t.Fatalf("results %v", results)
	}
	text := results[0].Text
	for _, want := range []string{"Started by subagent sa-1-attr0003", "deleg_attr_3", "run the npm ci for the desktop app"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}
}

func TestChildCompletionNotificationSuppressedByDefault(t *testing.T) {
	reg := tools.NewProcessRegistry()
	reg.CompletionQueue.Put(childCompletionEvt("sa-9-supp0001", "proc_childnoise01"))
	if results := reg.DrainNotifications(surfaceEnv(t, false), "", nil, false); len(results) != 0 {
		t.Fatalf("results %v", results)
	}
	if reg.CompletionQueue.Len() != 0 {
		t.Fatal("child event requeued")
	}
}

func TestAsyncDelegationEventFromChildNeverSuppressed(t *testing.T) {
	reg := tools.NewProcessRegistry()
	reg.CompletionQueue.Put(childCompletionEvt("sa-9-supp0002", "proc_childnoise01"))
	reg.CompletionQueue.Put(map[string]any{
		"type": "async_delegation", "delegation_id": "deleg_supp_2", "task_id": "sa-9-supp0002", "goal": "port the widget",
		"status": "completed", "summary": "Widget ported successfully.",
	})
	results := reg.DrainNotifications(surfaceEnv(t, false), "", nil, false)
	if len(results) != 1 || results[0].Event["type"] != "async_delegation" || !strings.Contains(results[0].Text, "ASYNC DELEGATION COMPLETE") {
		t.Fatalf("results %v", results)
	}
	if reg.CompletionQueue.Len() != 0 {
		t.Fatal("queue not drained")
	}
}

func TestParentOwnedCompletionUnaffectedBySuppression(t *testing.T) {
	reg := tools.NewProcessRegistry()
	reg.CompletionQueue.Put(map[string]any{
		"type": "completion", "session_id": "proc_parent01", "task_id": "20260817_154314_30d98f", "command": "make build",
		"exit_code": 0, "output": "ok",
	})
	results := reg.DrainNotifications(surfaceEnv(t, false), "", nil, false)
	if len(results) != 1 || !strings.Contains(results[0].Text, "Background process proc_parent01") {
		t.Fatalf("results %v", results)
	}
}

func TestSurfaceFlagTrueRestoresChildNotificationDelivery(t *testing.T) {
	reg := tools.NewProcessRegistry()
	reg.CompletionQueue.Put(childCompletionEvt("sa-9-supp0003", "proc_childnoise01"))
	results := reg.DrainNotifications(surfaceEnv(t, true), "", nil, false)
	if len(results) != 1 || !strings.Contains(results[0].Text, "Background process proc_childnoise01") ||
		!strings.Contains(results[0].Text, "Started by subagent sa-9-supp0003") {
		t.Fatalf("results %v", results)
	}
}

func TestChildWatchMatchSuppressedByDefault(t *testing.T) {
	reg := tools.NewProcessRegistry()
	reg.CompletionQueue.Put(map[string]any{
		"type": "watch_match", "session_id": "proc_childwatch01", "task_id": "sa-9-supp0004", "command": "vitest --watch",
		"pattern": "FAIL", "output": "FAIL src/x.test.ts", "suppressed": 0,
	})
	if results := reg.DrainNotifications(surfaceEnv(t, false), "", nil, false); len(results) != 0 || reg.CompletionQueue.Len() != 0 {
		t.Fatalf("results %v", results)
	}
}

func TestChildCompletionWithCollapsedContainerTaskIDSuppressed(t *testing.T) {
	reg := tools.NewProcessRegistry()
	evt := childCompletionEvt("default", "proc_childnoise01")
	evt["owner_task_id"] = "sa-9-supp0005"
	reg.CompletionQueue.Put(evt)
	if results := reg.DrainNotifications(surfaceEnv(t, false), "", nil, false); len(results) != 0 || reg.CompletionQueue.Len() != 0 {
		t.Fatalf("results %v", results)
	}
}

// Shell child that waits for a gate file and prints a finish marker
func notificationChild(t *testing.T) (string, string) {
	dir := t.TempDir()
	gate := filepath.Join(dir, "release")
	t.Cleanup(func() { _ = os.WriteFile(gate, nil, 0o644) })
	cmd := fmt.Sprintf(`i=0; while [ ! -e '%s' ]; do i=$((i+1)); [ $i -gt 1500 ] && exit 3; sleep 0.01; done; echo notification-child-finished`, gate)
	return cmd, gate
}

func waitCompletion(t *testing.T, reg *tools.ProcessRegistry) map[string]any {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if evt, ok := reg.CompletionQueue.GetNowait(); ok {
			return evt
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no completion event")
	return nil
}

func TestSpawnLocalStampsOwnerTaskIDAndEventCarriesIt(t *testing.T) {
	env := surfaceEnv(t, false)
	reg := tools.NewProcessRegistry()
	command, gate := notificationChild(t)
	session, err := reg.SpawnLocal(env, command, tools.SpawnLocalOptions{TaskID: "default", OwnerTaskID: "sa-9-supp0006"})
	if err != nil {
		t.Fatal(err)
	}
	session.NotifyOnComplete = true
	if session.OwnerTaskID != "sa-9-supp0006" {
		t.Fatalf("owner %q", session.OwnerTaskID)
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	evt := waitCompletion(t, reg)
	if fmt.Sprint(evt["exit_code"]) != "0" || !strings.Contains(fmt.Sprint(evt["output"]), "notification-child-finished") {
		t.Fatalf("evt %v", evt)
	}
	reg.CompletionQueue.Put(evt)
	if results := reg.DrainNotifications(env, "", nil, false); len(results) != 0 {
		t.Fatalf("results %v", results)
	}
}

func TestSpawnLocalWithoutOwnerDefaultsToTaskID(t *testing.T) {
	env := surfaceEnv(t, false)
	reg := tools.NewProcessRegistry()
	command, gate := notificationChild(t)
	session, err := reg.SpawnLocal(env, command, tools.SpawnLocalOptions{TaskID: "default"})
	if err != nil {
		t.Fatal(err)
	}
	session.NotifyOnComplete = true
	if session.OwnerTaskID != "default" {
		t.Fatalf("owner %q", session.OwnerTaskID)
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	evt := waitCompletion(t, reg)
	if fmt.Sprint(evt["exit_code"]) != "0" || !strings.Contains(fmt.Sprint(evt["output"]), "notification-child-finished") {
		t.Fatalf("evt %v", evt)
	}
	reg.CompletionQueue.Put(evt)
	results := reg.DrainNotifications(env, "", nil, false)
	if len(results) != 1 || !strings.Contains(results[0].Text, "completed normally") {
		t.Fatalf("results %v", results)
	}
}

func TestAttributionLineUsesOwnerTaskID(t *testing.T) {
	child := newFakeChild(&AIAgent{sessionID: "sess-attr-owner"})
	registerFake("sa-9-supp0007", child, map[string]any{"delegation_id": "deleg_attr_owner"})
	defer unregisterSubagent("sa-9-supp0007", nil)
	reg := tools.NewProcessRegistry()
	evt := childCompletionEvt("default", "proc_ownerattr01")
	evt["owner_task_id"] = "sa-9-supp0007"
	reg.CompletionQueue.Put(evt)
	results := reg.DrainNotifications(surfaceEnv(t, true), "", nil, false)
	if len(results) != 1 || !strings.Contains(results[0].Text, "Started by subagent sa-9-supp0007") {
		t.Fatalf("results %v", results)
	}
	if text := tools.FormatProcessNotification(evt); !strings.Contains(text, "Started by subagent sa-9-supp0007") {
		t.Fatalf("formatter %q", text)
	}
}

func TestCompletionNotificationTrimsSubagentOutputWall(t *testing.T) {
	child := newFakeChild(&AIAgent{sessionID: "sess-attr-4"})
	registerFake("sa-2-attr0004", child, map[string]any{"delegation_id": "deleg_attr_4"})
	defer unregisterSubagent("sa-2-attr0004", nil)
	big := strings.Repeat("npm noise line\n", 500)
	text := tools.FormatProcessNotification(map[string]any{
		"type": "completion", "session_id": "proc_deadbeef0002", "task_id": "sa-2-attr0004", "command": "npm ci",
		"exit_code": 0, "output": big,
	})
	if text == "" || len(text) >= len(big) {
		t.Fatalf("len %d", len(text))
	}
}

func TestControlActionNotBlockedAtSpawnCap(t *testing.T) {
	cfg := tools.DefaultToolCallGuardrailConfig()
	cfg.LoopCaps.MaxSubagents = 1
	ctl := tools.NewToolCallGuardrailController(&cfg)
	if ctl.BeforeCall("delegate_task", map[string]any{"goal": "a"}).Action != "allow" ||
		ctl.BeforeCall("delegate_task", map[string]any{"goal": "b"}).Action != "block" {
		t.Fatal("cap not enforced")
	}
	ctl2 := tools.NewToolCallGuardrailController(&cfg)
	if ctl2.BeforeCall("delegate_task", map[string]any{"goal": "a"}).Action != "allow" ||
		ctl2.BeforeCall("delegate_task", map[string]any{"action": "stop", "subagent_id": "x"}).Action != "allow" ||
		ctl2.BeforeCall("delegate_task", map[string]any{"action": "list"}).Action != "allow" ||
		ctl2.BeforeCall("delegate_task", map[string]any{"goal": "c"}).Action != "block" {
		t.Fatal("control action blocked or cap reset")
	}
}
