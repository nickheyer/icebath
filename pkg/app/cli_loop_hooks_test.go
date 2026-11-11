package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/fakellm"
	"github.com/nickheyer/icebath/pkg/state"
)

// Isolated home whose goal and loop rows land in a fresh state database
func goalHooksHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(home.PatchUserHome(tmp))
	t.Setenv(home.EnvHome, h)
	goals.ClearSessionDBCache()
	t.Cleanup(func() {
		goals.ClearSessionDBCache()
		state.CloseAllUnder(h)
	})
	return h
}

// CLI with an empty pending queue whose goal manager is the given one
func goalHooksCLI(sessionID string, mgr *goals.GoalManager) *CLI {
	c := bareCLI()
	c.pendingInput = &inputQueue{}
	c.sessionID = sessionID
	c.goalManager = mgr
	return c
}

func TestIdleHookQueuesTheContinuationWhenATimedBarrierHasElapsed(t *testing.T) {
	h := goalHooksHome(t)
	recordPrints(t)
	t.Setenv(home.EnvHome, h)
	mgr := goals.NewGoalManager(context.Background(), "resume-idle", goals.DefaultGoalMaxTurns)
	if _, err := mgr.Set("finish the thing", 0, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.WaitForSeconds(1, "cooldown", 0); err != nil {
		t.Fatal(err)
	}
	c := goalHooksCLI("resume-idle", mgr)
	c.maybeResumeParkedGoal()
	if !c.pendingInput.Empty() {
		t.Fatal("parked goal queued a continuation")
	}
	mgr.State().WaitingUntil = unixSeconds() - 1
	goals.SaveGoal(context.Background(), "resume-idle", mgr.State())
	c.lastGoalBarrierCheck = 0.0
	c.maybeResumeParkedGoal()
	queued, ok := c.pendingInput.GetNoWait()
	if !ok || !strings.Contains(queued.(string), "finish the thing") {
		t.Fatalf("queued %#v", queued)
	}
	if mgr.State().WaitingUntil != 0.0 {
		t.Fatalf("barrier kept %v", mgr.State().WaitingUntil)
	}
}

func TestIdleHookIsANoOpForAnUnparkedOrInactiveGoal(t *testing.T) {
	h := goalHooksHome(t)
	recordPrints(t)
	t.Setenv(home.EnvHome, h)
	mgr := goals.NewGoalManager(context.Background(), "resume-noop", goals.DefaultGoalMaxTurns)
	if _, err := mgr.Set("g", 0, nil); err != nil {
		t.Fatal(err)
	}
	c := goalHooksCLI("resume-noop", mgr)
	c.maybeResumeParkedGoal()
	if !c.pendingInput.Empty() {
		t.Fatal("unparked goal queued")
	}
	mgr.Clear()
	c.lastGoalBarrierCheck = 0.0
	c.maybeResumeParkedGoal()
	if !c.pendingInput.Empty() {
		t.Fatal("cleared goal queued")
	}
}

// CLI stub with an active five turn goal wired in
func makeCLIWithGoal(t *testing.T, sessionID, goalText string) (*CLI, *goals.GoalManager) {
	t.Helper()
	mgr := goals.NewGoalManager(context.Background(), sessionID, 5)
	if _, err := mgr.Set(goalText, 0, nil); err != nil {
		t.Fatal(err)
	}
	return goalHooksCLI(sessionID, mgr), mgr
}

// Routes the auxiliary goal judge of the isolated home to a loopback server answering reply
func fakeGoalJudge(t *testing.T, h, reply string) {
	t.Helper()
	srv, err := fakellm.New(nil, fakellm.Options{Aux: func(*fakellm.Request) any { return fakellm.Text{Text: reply} }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	if err := fakellm.WriteHome(h, srv.BaseURL(), "", "", 0); err != nil {
		t.Fatal(err)
	}
}

func TestInterruptedTurnIsResumable(t *testing.T) {
	h := goalHooksHome(t)
	recordPrints(t)
	t.Setenv(home.EnvHome, h)
	c, mgr := makeCLIWithGoal(t, "sid-resume-1", "build a thing")
	c.lastTurnInterrupted = true
	c.conversationHistory = []map[string]any{{"role": "assistant", "content": "partial"}}
	c.maybeContinueGoalAfterTurn()
	if mgr.State().Status != "paused" {
		t.Fatalf("status %v", mgr.State().Status)
	}
	mgr.Resume(false)
	if mgr.State().Status != "active" {
		t.Fatalf("status %v", mgr.State().Status)
	}
}

func TestCleanResponseEnqueuesContinuationWhenJudgeSaysContinue(t *testing.T) {
	h := goalHooksHome(t)
	fakeGoalJudge(t, h, `{"done": false, "reason": "needs more steps"}`)
	recordPrints(t)
	t.Setenv(home.EnvHome, h)
	c, mgr := makeCLIWithGoal(t, "sid-healthy-1", "build a thing")
	c.conversationHistory = []map[string]any{{"role": "user", "content": "go"}, {"role": "assistant", "content": "did some work, more to do"}}
	c.maybeContinueGoalAfterTurn()
	queued, ok := c.pendingInput.GetNoWait()
	if !ok || !strings.Contains(queued.(string), "Continuing toward your standing goal") {
		t.Fatalf("queued %#v", queued)
	}
	if mgr.State().Status != "active" {
		t.Fatalf("status %v", mgr.State().Status)
	}
}

func TestCleanResponseMarksDoneWhenJudgeSaysDone(t *testing.T) {
	h := goalHooksHome(t)
	fakeGoalJudge(t, h, `{"done": true, "reason": "goal satisfied"}`)
	recordPrints(t)
	t.Setenv(home.EnvHome, h)
	c, mgr := makeCLIWithGoal(t, "sid-done-1", "build a thing")
	c.conversationHistory = []map[string]any{{"role": "assistant", "content": "all finished, here's the result"}}
	c.maybeContinueGoalAfterTurn()
	if !c.pendingInput.Empty() {
		t.Fatal("done goal queued a continuation")
	}
	if mgr.State().Status != "done" {
		t.Fatalf("status %v", mgr.State().Status)
	}
}
