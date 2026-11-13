package goals

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// Command dispatch with no gate denial and the English render, failing the test on a raised error
func goalCmd(t *testing.T, m *GoalManager, arg string, o GoalCommandOptions) GoalCommandResult {
	t.Helper()
	if o.AuthorizeGate == nil {
		o.AuthorizeGate = func() string { return "" }
	}
	result, err := DispatchGoalCommand(m, arg, o)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestGoalCommandSetsAGoalWithItsContractAndKickoff(t *testing.T) {
	goalsTestHome(t)
	m := NewGoalManager(context.Background(), "cmd-set", 0)
	r := goalCmd(t, m, "Ship it\nverify: tests pass", GoalCommandOptions{})
	want := "⊙ Goal set (20-turn budget): Ship it\nCompletion contract:\n- Verification: tests pass\nAfter each turn, a judge model checks if the goal is done against the contract above. " +
		"Icebath keeps working until it is, you pause/clear it, or the budget is exhausted. Use /goal status, /goal show, /goal pause, /goal resume, /goal clear."
	if r.Output != want || !r.Kickoff || r.Prompt == nil || *r.Prompt != "Ship it" || r.Error || r.ClearPending != "" {
		t.Fatalf("result %+v", r)
	}
	r = goalCmd(t, m, "-- pause the cron", GoalCommandOptions{})
	if !strings.HasPrefix(r.Output, "⊙ Goal set (20-turn budget): pause the cron\n(replaced the previous goal)\n  was: Ship it\nAfter each turn, a judge model checks if the goal is done. ") {
		t.Fatalf("verbatim set %q", r.Output)
	}
	if r = goalCmd(t, m, "--", GoalCommandOptions{}); r.Output != "Invalid goal: goal text is empty" || !r.Error {
		t.Fatalf("empty verbatim %+v", r)
	}
	long := strings.Repeat("handoff step; ", 40)
	if r = goalCmd(t, m, long, GoalCommandOptions{LastUserMessage: long}); *r.Prompt != GoalAlreadySeenKick {
		t.Fatalf("repaste kick %q", *r.Prompt)
	}
	render := func(key, def string, values map[string]any) string { return key }
	if r = goalCmd(t, m, "x", GoalCommandOptions{Render: render}); !strings.HasPrefix(r.Output, "gateway.goal.set\n(replaced the previous goal)") {
		t.Fatalf("translated %q", r.Output)
	}
}

func TestGoalCommandDraftsAContractOrFallsBackToAFreeFormGoal(t *testing.T) {
	goalsTestHome(t)
	m := NewGoalManager(context.Background(), "cmd-draft", 0)
	saved := goalCommandDraft
	t.Cleanup(func() { goalCommandDraft = saved })
	var progress []string
	goalCommandDraft = func(context.Context, string, *float64) *GoalContract {
		return &GoalContract{Verification: "suite green"}
	}
	r := goalCmd(t, m, "draft migrate auth", GoalCommandOptions{Progress: func(s string) { progress = append(progress, s) }})
	if r.Output != "⊙ Goal set (20-turn budget): migrate auth\nDrafted completion contract:\n- Verification: suite green\n"+
		"Tighten any field by re-setting the goal with inline lines (e.g. verify: <command>), then /goal resume. Use /goal show to review." ||
		len(progress) != 1 || progress[0] != "Drafting completion contract…" {
		t.Fatalf("drafted %q progress %v", r.Output, progress)
	}
	goalCommandDraft = func(context.Context, string, *float64) *GoalContract { panic(strMethodError([]any{}, "strip")) }
	if r = goalCmd(t, m, "draft other thing", GoalCommandOptions{}); !strings.HasSuffix(r.Output,
		"\nCouldn't draft a contract (aux model unavailable) — running as a free-form goal. The per-turn judge still applies.") {
		t.Fatalf("draft fallback %q", r.Output)
	}
	if r = goalCmd(t, m, "draft", GoalCommandOptions{}); r.Output != "Usage: /goal draft <objective in plain language>" || !r.Error {
		t.Fatalf("bare draft %+v", r)
	}
}

func TestGoalCommandControlVerbsActOnTheExistingGoal(t *testing.T) {
	goalsTestHome(t)
	m := NewGoalManager(context.Background(), "cmd-ctl", 0)
	if r := goalCmd(t, m, "", GoalCommandOptions{}); r.Output != "No active goal. Set one with /goal <text>." {
		t.Fatalf("status %q", r.Output)
	}
	if r := goalCmd(t, m, "pause", GoalCommandOptions{}); r.Output != "No goal set." || r.ClearPending != "" {
		t.Fatalf("pause without goal %+v", r)
	}
	if r := goalCmd(t, m, "resume", GoalCommandOptions{}); r.Output != "No goal to resume." || r.Prompt != nil {
		t.Fatalf("resume without goal %+v", r)
	}
	goalCmd(t, m, "ship it", GoalCommandOptions{})
	if r := goalCmd(t, m, "PAUSE", GoalCommandOptions{}); r.Output != "⏸ Goal paused: ship it" || r.ClearPending != "pause" || m.State().PausedReason != UserPausedReason {
		t.Fatalf("pause %+v", r)
	}
	r := goalCmd(t, m, "resume last goal", GoalCommandOptions{})
	if r.Output != "▶ Goal resumed: ship it\n(ignored 'last goal': a control command never sets goal text — use /goal -- <text> when the goal really starts with a control word.)" ||
		r.Prompt == nil || !strings.Contains(*r.Prompt, "Goal: ship it") || pyStrOf(m.State().Goal) != "ship it" {
		t.Fatalf("bare control %+v", r)
	}
	if r := goalCmd(t, m, "show", GoalCommandOptions{}); r.Output != "⊙ Goal (active, 0/20 turns): ship it\n(no completion contract — set one with /goal draft <objective> or inline field: value lines)" {
		t.Fatalf("show %q", r.Output)
	}
	if r := goalCmd(t, m, "unwait", GoalCommandOptions{}); r.Output != "No wait barrier set." {
		t.Fatalf("unwait %q", r.Output)
	}
	if _, err := m.WaitForSeconds(60, "", 0); err != nil {
		t.Fatal(err)
	}
	if r := goalCmd(t, m, "unwait", GoalCommandOptions{}); r.Output != "▶ Wait barrier cleared — goal loop resumes." {
		t.Fatalf("unwait %q", r.Output)
	}
	if r := goalCmd(t, m, "done", GoalCommandOptions{}); r.Output != "✓ Goal cleared." || r.ClearPending != "clear" || m.State() != nil {
		t.Fatalf("clear %+v", r)
	}
	if r := goalCmd(t, m, "stop", GoalCommandOptions{}); r.Output != "No active goal." || r.ClearPending != "clear" {
		t.Fatalf("clear without goal %+v", r)
	}
}

func TestGoalCommandWaitParksOnALivePidOrReportsUsage(t *testing.T) {
	goalsTestHome(t)
	m := NewGoalManager(context.Background(), "cmd-wait", 0)
	if r := goalCmd(t, m, "wait 4242", GoalCommandOptions{}); r.Output != "/goal wait: no active goal to park" || !r.Error {
		t.Fatalf("no goal %+v", r)
	}
	goalCmd(t, m, "g", GoalCommandOptions{})
	for arg, want := range map[string]string{"wait": "Usage: /goal wait <pid> [reason]", "wait abc": "/goal wait: <pid> must be an integer process id.",
		"wait 0": "/goal wait: pid must be a positive integer", "wait 99999999999999999999": "/goal wait: pid is not alive on this host"} {
		if r := goalCmd(t, m, arg, GoalCommandOptions{}); r.Output != want || !r.Error {
			t.Fatalf("%q gave %+v", arg, r)
		}
	}
	pid := spawnSleeper(t)
	r := goalCmd(t, m, "wait "+strconv.FormatInt(pid, 10)+" CI run", GoalCommandOptions{})
	if r.Output != "⏳ Goal parked on pid "+strconv.FormatInt(pid, 10)+" (CI run). Loop pauses until it exits." || r.Error || !m.IsWaiting() {
		t.Fatalf("park %+v", r)
	}
}

func TestGoalCommandWaitChecksTheActiveGoalBeforeAHugePid(t *testing.T) {
	goalsTestHome(t)
	m := NewGoalManager(context.Background(), "cmd-wait-huge", 0)
	for _, arg := range []string{"wait -99999999999999999999", "wait 99999999999999999999 later"} {
		if r := goalCmd(t, m, arg, GoalCommandOptions{}); r.Output != "/goal wait: no active goal to park" || !r.Error {
			t.Fatalf("%q without a goal gave %+v", arg, r)
		}
	}
	goalCmd(t, m, "ship it", GoalCommandOptions{})
	if r := goalCmd(t, m, "pause", GoalCommandOptions{}); r.Output != "⏸ Goal paused: ship it" || r.ClearPending != "pause" {
		t.Fatalf("pause %+v", r)
	}
	if r := goalCmd(t, m, "wait -99999999999999999999", GoalCommandOptions{}); r.Output != "/goal wait: no active goal to park" || !r.Error {
		t.Fatalf("paused goal gave %+v", r)
	}
	goalCmd(t, m, "resume", GoalCommandOptions{})
	if r := goalCmd(t, m, "wait -99999999999999999999", GoalCommandOptions{}); r.Output != "/goal wait: pid must be a positive integer" || !r.Error {
		t.Fatalf("active goal gave %+v", r)
	}
	if r := goalCmd(t, m, "show", GoalCommandOptions{}); !strings.HasPrefix(r.Output, m.StatusLine()+"\n") || r.Error {
		t.Fatalf("show %+v", r)
	}
	if r := goalCmd(t, m, "clear", GoalCommandOptions{}); r.Output != "✓ Goal cleared." || r.ClearPending != "clear" {
		t.Fatalf("clear %+v", r)
	}
	if r := goalCmd(t, m, "stop", GoalCommandOptions{}); r.Output != "No active goal." || r.ClearPending != "clear" {
		t.Fatalf("second clear %+v", r)
	}
}

func TestGoalCommandGatesNeedAuthorizationToBeAdded(t *testing.T) {
	goalsTestHome(t)
	m := NewGoalManager(context.Background(), "cmd-gate", 0)
	if r := goalCmd(t, m, "gate add make test", GoalCommandOptions{}); r.Output != "/goal gate add: no active goal" || !r.Error {
		t.Fatalf("no goal %+v", r)
	}
	goalCmd(t, m, "g", GoalCommandOptions{})
	deny := func() string { return "only admins may add gates" }
	if r := goalCmd(t, m, "gate add make test", GoalCommandOptions{AuthorizeGate: deny}); r.Output != "only admins may add gates" || !r.Error {
		t.Fatalf("denied %+v", r)
	}
	if r := goalCmd(t, m, "gate add make test", GoalCommandOptions{}); r.Output != "⚿ Gate added: $ make test (3 retries, 300s timeout). It must pass before the goal can complete." {
		t.Fatalf("add %q", r.Output)
	}
	if r := goalCmd(t, m, "gate", GoalCommandOptions{}); r.Output != "- 1. $ make test" {
		t.Fatalf("list %q", r.Output)
	}
	for _, arg := range []string{"gate bogus x", "gate clear now", "gate add", "gate remove"} {
		if r := goalCmd(t, m, arg, GoalCommandOptions{}); r.Output != "Usage: /goal gate [list | add <command> | remove <N> | clear]" || !r.Error {
			t.Fatalf("%q gave %+v", arg, r)
		}
	}
	for arg, want := range map[string]string{"gate rm x": "/goal gate remove: invalid literal for int() with base 10: 'x'",
		"gate remove 5": "/goal gate remove: index out of range (1..1)", "gate remove 99999999999999999999": "/goal gate remove: index out of range (1..1)"} {
		if r := goalCmd(t, m, arg, GoalCommandOptions{}); r.Output != want || !r.Error {
			t.Fatalf("%q gave %+v", arg, r)
		}
	}
	if r := goalCmd(t, m, "gate rm 1", GoalCommandOptions{}); r.Output != "✓ Gate removed: $ make test" {
		t.Fatalf("remove %q", r.Output)
	}
	if r := goalCmd(t, m, "gate clear", GoalCommandOptions{}); r.Output != "✓ Cleared 0 gates." {
		t.Fatalf("clear %q", r.Output)
	}
}

func TestIsGoalControlNamesTheControlForms(t *testing.T) {
	for arg, want := range map[string]bool{"": true, " Status ": true, "resume now": true, "wait 5": true, "gate add x": true, "clear": true,
		"clear the cache": false, "ship it": false, "draft x": false} {
		if IsGoalControl(arg) != want {
			t.Fatalf("IsGoalControl(%q) != %v", arg, want)
		}
	}
}
