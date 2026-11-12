package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Clears the live subagent registry around a case
func cleanSubagentRegistry(t *testing.T) {
	t.Helper()
	agent.ClearActiveSubagents()
	t.Cleanup(agent.ClearActiveSubagents)
}

// Agent owning children under the session id
func ownerAgent(t *testing.T, sessionID string) *GatewayAgent {
	return offlineAgent(t, &CLI{ctx: context.Background(), sessionID: sessionID})
}

// Child double whose steer text is recorded
type steerableChild struct{ delivered *[]string }

func (s steerableChild) Steer(text string) bool {
	*s.delivered = append(*s.delivered, text)
	return true
}

// CLI holding only the agent, session and pending queue the dock reads
func dockCLI(t *testing.T, a *GatewayAgent, sessionID string) *CLI {
	t.Helper()
	recordPrints(t)
	c := bareCLI()
	c.sessionID, c.pendingInput = sessionID, &inputQueue{}
	if a != nil {
		c.agent = a
	}
	return c
}

// Polls until the condition holds or five seconds pass
func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestDockScopesChildrenAndFitsShortNarrowTerminal(t *testing.T) {
	cleanSubagentRegistry(t)
	for i := 0; i < 8; i++ {
		agent.RegisterSubagent(map[string]any{"subagent_id": string(rune('0' + i)), "owner_agent_session_id": "owner",
			"goal": strings.Repeat("Check 界", 20), "started_at": 10.0, "status": "running", "last_tool": "read_file"})
	}
	agent.RegisterSubagent(map[string]any{"subagent_id": "foreign", "owner_agent_session_id": "other", "goal": "SECRET"})
	c := dockCLI(t, ownerAgent(t, "owner"), "")
	dock := newSubagentMonitor(c)
	if !dock.refreshAt(20) {
		t.Fatal("first refresh unchanged")
	}
	text := dock.dockText(32, 14)
	if !strings.Contains(text, c.t("cli.subagents.count_live", map[string]any{"count": 8})) || !strings.Contains(text, "Ctrl+T") ||
		!strings.Contains(text, "read_file") {
		t.Fatalf("dock %q", text)
	}
	if strings.Contains(text, "SECRET") || len(strings.Split(text, "\n")) > 3 {
		t.Fatalf("dock %q", text)
	}
	for _, line := range strings.Split(text, "\n") {
		if cellWidth(line) > 32 {
			t.Fatalf("line wider than the terminal %q", line)
		}
	}
	if dock.refreshAt(20) {
		t.Fatal("unchanged refresh reported a change")
	}
	agent.ClearActiveSubagents()
	if !dock.refreshAt(21) || dock.dockText(32, 14) != "" || dock.refreshAt(22) {
		t.Fatal("dock kept rows after the children left")
	}
	c.tui.subagentDockWidget = &ptWindow{name: "dock"}
	status, composer := &ptWindow{name: "status"}, &ptWindow{name: "composer"}
	children := c.buildTUILayoutChildren(tuiLayoutChildren{spacer: &ptWindow{name: "spacer"}, statusBar: status,
		inputRuleTop: &ptWindow{name: "top"}, inputArea: composer, inputRuleBot: &ptWindow{name: "bottom"}})
	index := func(w *ptWindow) int {
		for i, child := range children {
			if child == w {
				return i
			}
		}
		return -1
	}
	if !(index(c.tui.subagentDockWidget) < index(status) && index(status) < index(composer)) {
		t.Fatal("dock not above the status bar and composer")
	}
}

func TestMonitorControlsRecheckOwnershipAndKeepSelection(t *testing.T) {
	cleanSubagentRegistry(t)
	var delivered []string
	agent.RegisterSubagent(map[string]any{"subagent_id": "mine", "owner_agent_session_id": "owner", "goal": "Check module",
		"started_at": 10.0, "status": "running", "agent": steerableChild{delivered: &delivered}})
	owner := ownerAgent(t, "owner")
	dock := newSubagentMonitor(dockCLI(t, owner, ""))
	dock.refreshAt(20)
	dock.selectBy(1)
	if row, _ := dock.selected(); row == nil || row["subagent_id"] != "mine" {
		t.Fatalf("selected %v", row)
	}
	result, err := dock.control("steer", "Focus on tests", nil)
	if err != nil || result["status"] != "queued" || len(delivered) != 1 || delivered[0] != "Focus on tests" {
		t.Fatalf("steer %v %v delivered %q", result, err, delivered)
	}
	owner.SetSessionID("different")
	result, err = dock.control("steer", "must not arrive", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result["error"]; !ok || len(delivered) != 1 {
		t.Fatalf("foreign steer %v delivered %q", result, delivered)
	}
}

// Starts a background shell process through the default registry
func spawnDockProcess(t *testing.T, command string) *tools.ProcessSession {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZDOTDIR", t.TempDir())
	proc, err := tools.DefaultProcessRegistry().SpawnLocal(context.Background(), nil, command,
		tools.SpawnLocalOptions{Cwd: ".", TaskID: "t", OwnerTaskID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		registry := tools.DefaultProcessRegistry()
		registry.KillProcess(context.Background(), nil, proc.ID, "process.kill", true)
		deadline := time.Now().Add(5 * time.Second)
		for !processExited(proc.ID) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		registry.ListSessions(context.Background(), nil, "", "", false)
		registry.ForgetFinished(proc.ID)
	})
	return proc
}

// Whether the registry saw the process exit
func processExited(id string) bool {
	proc := tools.DefaultProcessRegistry().Get(nil, id)
	return proc != nil && proc.DockFields().ExitedAt != 0
}

func TestDockPaintsProcessesUnderAgentsAndRetiresFinishedRows(t *testing.T) {
	cleanSubagentRegistry(t)
	agent.RegisterSubagent(map[string]any{"subagent_id": "a1", "owner_agent_session_id": "owner", "goal": "Check module",
		"started_at": nowSeconds() - 5, "status": "running", "last_tool": "read_file"})
	quick := spawnDockProcess(t, "echo hello-dock; exit 3")
	slow := spawnDockProcess(t, "sleep 30")
	ctx := context.Background()
	waitUntil(t, func() bool {
		tools.DefaultProcessRegistry().ListSessions(ctx, nil, "", "", false)
		return processExited(quick.ID)
	})
	c := dockCLI(t, ownerAgent(t, "owner"), "")
	dock := newSubagentMonitor(c)
	if !dock.refreshAt(nowSeconds()) {
		t.Fatal("refresh unchanged")
	}
	text := dock.dockText(100, 30)
	lines := strings.Split(text, "\n")
	if !strings.Contains(lines[0], c.t("cli.subagents.subagents_heading", map[string]any{"count": 1})) {
		t.Fatalf("first line %q", lines[0])
	}
	summary := c.t("cli.subagents.count_running", map[string]any{"count": 1}) + " · " + c.t("cli.subagents.count_done", map[string]any{"count": 1})
	heading := c.t("cli.subagents.processes_heading", map[string]any{"summary": summary, "controls": ""})
	agentsAt, procsAt := -1, -1
	for i, line := range lines {
		if strings.Contains(line, "Check module") && agentsAt < 0 {
			agentsAt = i
		}
		if strings.Contains(line, heading) && procsAt < 0 {
			procsAt = i
		}
	}
	if agentsAt < 0 || procsAt < 0 || agentsAt >= procsAt {
		t.Fatalf("agents %d processes %d in %q", agentsAt, procsAt, text)
	}
	if !strings.Contains(text, "⚙ sleep 30") || !strings.Contains(text, c.t("cli.dock.starting", nil)) {
		t.Fatalf("running row missing in %q", text)
	}
	if !strings.Contains(text, "✘ echo hello-dock; exit 3 · "+c.t("cli.dock.exit_code", map[string]any{"code": 3})) {
		t.Fatalf("failed row missing in %q", text)
	}
	for _, line := range lines {
		if cellWidth(line) > 100 {
			t.Fatalf("wide line %q", line)
		}
	}
	narrow := strings.Split(dock.dockText(40, 14), "\n")
	hasAgent, hasProcs := false, false
	for _, line := range narrow {
		hasAgent = hasAgent || strings.Contains(line, "Check module")
		hasProcs = hasProcs || strings.Contains(line, c.t("cli.subagents.title_processes", nil))
		if cellWidth(line) > 40 {
			t.Fatalf("wide narrow line %q", line)
		}
	}
	if !hasAgent || !hasProcs {
		t.Fatalf("narrow dock lost a block %q", narrow)
	}
	dock.collapsed = true
	collapsed := dock.dockText(100, 30)
	want := c.t("cli.subagents.count_live", map[string]any{"count": 1}) + " · " + c.t("cli.subagents.count_procs_one", map[string]any{"count": 1})
	if strings.Contains(collapsed, "\n") || !strings.Contains(collapsed, want) {
		t.Fatalf("collapsed %q", collapsed)
	}
	later := processRows(ctx, localProcessRegistry{}, nowSeconds()+processDockRetainSeconds+1)
	if len(later) != 1 || later[0]["id"] != slow.ID {
		t.Fatalf("rows after the window %v", later)
	}
}

func TestMonitorControlsStopProcessesAndNeverSteerThem(t *testing.T) {
	cleanSubagentRegistry(t)
	slow := spawnDockProcess(t, "sleep 30")
	dock := newSubagentMonitor(dockCLI(t, nil, ""))
	dock.refresh()
	dock.selectedID = slow.ID
	if _, isProcess := dock.selected(); !isProcess {
		t.Fatal("process not selected")
	}
	result, _ := dock.control("steer", "nope", nil)
	if _, ok := result["error"]; !ok || processExited(slow.ID) {
		t.Fatalf("steer %v", result)
	}
	result, _ = dock.control("stop", nil, nil)
	if result["status"] != "killed" {
		t.Fatalf("stop %v", result)
	}
	waitUntil(t, func() bool { return processExited(slow.ID) })
	dock.refresh()
	found := false
	for _, r := range dock.processes {
		found = found || (r["id"] == slow.ID && r["status"] == "killed")
	}
	if !found {
		t.Fatalf("processes %v", dock.processes)
	}
}

func TestDockIgnoresAGoalManagerLeftOverFromAnotherSession(t *testing.T) {
	recordPrints(t)
	mgr := goals.NewGoalManager(context.Background(), "old-sid", 0)
	if _, err := mgr.Set("previous session objective", 0, nil); err != nil {
		t.Fatal(err)
	}
	c := bareCLI()
	c.sessionID, c.goalManager = "old-sid", mgr
	if !strings.HasPrefix(sessionDockGoalLine(c), "⊙ Goal") {
		t.Fatalf("goal line %q", sessionDockGoalLine(c))
	}
	c.sessionID = "new-sid"
	if sessionDockGoalLine(c) != "" {
		t.Fatal("stale manager painted")
	}
	c.goalManager = nil
	if sessionDockGoalLine(c) != "" {
		t.Fatal("goal line without a manager")
	}
}

func TestCollapsedSummaryPrioritizesLiveCountAndControlsAtSmallWidths(t *testing.T) {
	recordPrints(t)
	c := bareCLI()
	monitor := newSubagentMonitor(c)
	for i := 0; i < 6; i++ {
		monitor.entries = append(monitor.entries, map[string]any{"goal": strings.Repeat("Inspect 界 ", 40), "elapsed": 12,
			"last_tool": "terminal", "status": "running"})
	}
	monitor.collapsed = true
	for _, width := range []int{100, 80, 40, 24, 10, 1} {
		text := monitor.dockText(width, 20)
		if strings.Contains(text, "\n") || cellWidth(text) > width {
			t.Fatalf("width %d text %q", width, text)
		}
		if width >= 40 && (!strings.Contains(text, c.t("cli.subagents.count_live", map[string]any{"count": 6})) ||
			!strings.Contains(text, "Ctrl+T") || !strings.Contains(text, "Ctrl+R")) {
			t.Fatalf("width %d lost the count or controls %q", width, text)
		}
		if width >= 80 && !strings.Contains(text, c.t("cli.subagents.last_tool", map[string]any{"tool": "terminal"})) {
			t.Fatalf("width %d lost the activity %q", width, text)
		}
	}
	monitor.entries = nil
	if monitor.dockText(80, 20) != "" {
		t.Fatal("empty monitor painted")
	}
}

func TestDockPaintsGoalOnTopAndQueueLastAndFollowsTheirLifecycle(t *testing.T) {
	c := dockCLI(t, nil, "sid-a")
	ctx := context.Background()
	mgr := goals.NewGoalManager(ctx, "sid-a", 20)
	if _, err := mgr.Set("ship the dock feature", 0, nil); err != nil {
		t.Fatal(err)
	}
	c.goalManager = mgr
	c.pendingInput.Put("first follow-up\nwith a newline")
	c.pendingInput.Put(submittedInput{Text: "look at this", Images: []string{"shot.png"}})
	c.pendingInput.Put(&voiceInputMessage{Text: "spoken ask"})
	c.pendingInput.Put(tools.NewTimelineNotification("raw wall", "Process abc exited 0", "notice", ""))
	dock := newSubagentMonitor(c)
	if !dock.refreshAt(nowSeconds()) || !dock.hasRows() {
		t.Fatal("dock empty")
	}
	lines := strings.Split(dock.dockText(80, 40), "\n")
	if !strings.Contains(lines[0], "⊙ Goal (active, 0/20 turns): ship the dock feature") {
		t.Fatalf("first line %q", lines[0])
	}
	queueAt := -1
	for i, line := range lines {
		if strings.Contains(line, "Queue · 4 queued") {
			queueAt = i
			break
		}
	}
	if queueAt < 0 {
		t.Fatalf("no queue heading in %q", lines)
	}
	var queued []string
	for _, line := range lines[queueAt+1:] {
		queued = append(queued, strings.TrimSpace(line))
	}
	if strings.Join(queued, "|") != "1. first follow-up with a newline|2. look at this|3. spoken ask|+1 more" {
		t.Fatalf("queued rows %q", queued)
	}
	for _, line := range lines {
		if cellWidth(line) > 80 {
			t.Fatalf("wide line %q", line)
		}
	}
	dock.collapsed = true
	collapsed := dock.dockText(80, 40)
	counts := c.t("cli.subagents.goal_active", nil) + " · " + c.t("cli.subagents.count_queued", map[string]any{"count": 4})
	if strings.Contains(collapsed, "\n") || !strings.Contains(collapsed, c.t("cli.subagents.heading_session_restore", map[string]any{"count": counts})+" · ⊙ Goal") ||
		strings.Contains(collapsed, "Ctrl+T") || cellWidth(collapsed) > 80 {
		t.Fatalf("collapsed %q", collapsed)
	}
	dock.collapsed = false
	mgr.Pause("user-paused")
	c.mutatePendingInput(func([]any) []any { return nil })
	if !dock.refreshAt(nowSeconds()) || !strings.HasPrefix(dock.dockText(80, 40), " ⏸ Goal (paused") {
		t.Fatalf("paused dock %q", dock.dockText(80, 40))
	}
	mgr.Clear()
	if !dock.refreshAt(nowSeconds()) || dock.hasRows() || dock.dockText(80, 40) != "" {
		t.Fatal("cleared goal still painted")
	}
}

func TestDockLoopRowCountsDownToTheNextWakeup(t *testing.T) {
	c := dockCLI(t, nil, "sid-a")
	ctx := context.Background()
	mgr := agent.NewLoopManager(ctx, "sid-a")
	interval := int64(45 * 60)
	if _, err := mgr.Set(ctx, "check the deploy", &interval, 0, "", nil); err != nil {
		t.Fatal(err)
	}
	mgr.FireTick(ctx)
	mgr.CompleteTick(ctx, "deploy still running")
	due := mgr.State().NextDueAt
	c.loopManager = mgr
	dock := newSubagentMonitor(c)
	if !dock.refreshAt(due-45*60) || !dock.hasRows() {
		t.Fatal("dock empty")
	}
	if got := strings.TrimSpace(dock.dockText(100, 40)); got != "↻ loop · every 45m · 1 run · next in 45m · check the deploy" {
		t.Fatalf("loop row %q", got)
	}
	if !dock.refreshAt(due-30*60+1) || !strings.Contains(dock.loop, "next in 30m") {
		t.Fatalf("loop %q", dock.loop)
	}
	if !dock.refreshAt(due-42) || !strings.Contains(dock.loop, "next in 42s") {
		t.Fatalf("loop %q", dock.loop)
	}
	if dock.refreshAt(due - 42) {
		t.Fatal("unchanged text repainted")
	}
	dock.collapsed = true
	if got := strings.TrimSpace(dock.dockText(100, 40)); got != "↻ loop next in 42s · Ctrl+R restore · check the deploy" {
		t.Fatalf("collapsed %q", got)
	}
	if _, err := mgr.Pause(ctx, "user-paused"); err != nil {
		t.Fatal(err)
	}
	if !dock.refreshAt(due) || dock.loop != "⏸ loop paused · user-paused · 1 run · check the deploy" {
		t.Fatalf("paused loop %q", dock.loop)
	}
	mgr.Clear(ctx)
	if !dock.refreshAt(due) || dock.hasRows() {
		t.Fatal("cleared loop still painted")
	}
}

func TestDockRepaintsAGoalAToolChangedMidTurn(t *testing.T) {
	c := dockCLI(t, nil, "sid-a")
	ctx := context.Background()
	mgr := goals.NewGoalManager(ctx, "sid-a", 0)
	if _, err := mgr.Set("ship the dock feature", 0, nil); err != nil {
		t.Fatal(err)
	}
	c.goalManager = mgr
	dock := newSubagentMonitor(c)
	dock.refresh()
	if _, err := goals.NewGoalManager(ctx, "sid-a", 0).AddSubgoal("tests pass"); err != nil {
		t.Fatal(err)
	}
	dock.refresh()
	if strings.Contains(dock.dockText(80, 40), "subgoal") {
		t.Fatal("dock reread the goal without a control update")
	}
	c.markAutomationStale()
	if !dock.refreshAt(nowSeconds()) || !strings.Contains(strings.Split(dock.dockText(80, 40), "\n")[0], "1 subgoal") {
		t.Fatalf("dock %q", dock.dockText(80, 40))
	}
	if c.automationStale {
		t.Fatal("stale flag kept")
	}
}
