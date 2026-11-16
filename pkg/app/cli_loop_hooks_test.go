package app

import (
	"context"
	"os"
	"path/filepath"
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

// Stand in CLI with an active five turn goal wired in
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
