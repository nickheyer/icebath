package app

import (
	"context"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

const yoloSessionKey = "test-cli-yolo-session"

// Clears the bypass state around a case and returns a stand in CLI bound to the session id
func yoloStandIn(t *testing.T, sessionID string) *CLI {
	t.Helper()
	caseHome(t)
	t.Setenv("ICEBATH_YOLO_MODE", "")
	t.Cleanup(tools.SwapYoloModeFrozen(false))
	for _, key := range []string{yoloSessionKey, "default", sessionID} {
		tools.ClearSession(key)
		key := key
		t.Cleanup(func() { tools.ClearSession(key) })
	}
	return &CLI{ctx: context.Background(), sessionID: sessionID}
}

func TestToggleYoloDisablesSessionBypassOnSecondCall(t *testing.T) {
	c := yoloStandIn(t, yoloSessionKey)
	withConsoleInput(t, "")
	c.toggleYolo()
	if !tools.IsSessionYoloEnabled(yoloSessionKey) {
		t.Fatal("toggle on")
	}
	c.toggleYolo()
	if tools.IsSessionYoloEnabled(yoloSessionKey) {
		t.Fatal("toggle off")
	}
}

func TestTwoIndependentSessionsAreIsolated(t *testing.T) {
	a := yoloStandIn(t, "session-yolo-a")
	yoloStandIn(t, "session-yolo-b")
	withConsoleInput(t, "")
	a.toggleYolo()
	if !tools.IsSessionYoloEnabled("session-yolo-a") || tools.IsSessionYoloEnabled("session-yolo-b") {
		t.Fatal("sessions not isolated")
	}
}

func TestToggleYoloBypassesDangerousCommandCheck(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	c := yoloStandIn(t, yoloSessionKey)
	withConsoleInput(t, "")
	env := &tools.Env{Home: h, Values: &sync.Map{}}
	tools.SetCurrentSessionKey(env, yoloSessionKey)
	t.Cleanup(func() { tools.ResetCurrentSessionKey(env) })
	if tools.IsCurrentSessionYoloEnabled(env) {
		t.Fatal("the bypass was on before the toggle")
	}
	c.toggleYolo()
	if !tools.IsCurrentSessionYoloEnabled(env) {
		t.Fatal("the toggle did not arm the bypass of the bound session")
	}
	if result := tools.CheckAllCommandGuards(context.Background(), env, "rm -rf /tmp/scratch-xyzzy", "local", true); !result.Approved {
		t.Fatalf("guard result %#v", result)
	}
}

func TestTransferMovesYoloToNewSession(t *testing.T) {
	yoloStandIn(t, "old-id")
	yoloStandIn(t, "new-id")
	tools.EnableSessionYolo("old-id")
	tools.TransferSessionYolo("old-id", "new-id")
	if !tools.IsSessionYoloEnabled("new-id") || tools.IsSessionYoloEnabled("old-id") {
		t.Fatal("transfer")
	}
}

func TestTransferHandlesEmptyInputsSafely(t *testing.T) {
	yoloStandIn(t, "new")
	yoloStandIn(t, "old")
	tools.TransferSessionYolo("", "new")
	tools.TransferSessionYolo("old", "")
	if tools.IsSessionYoloEnabled("new") || tools.IsSessionYoloEnabled("old") {
		t.Fatal("empty transfer enabled a session")
	}
}
