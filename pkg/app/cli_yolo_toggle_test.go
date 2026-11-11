package app

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

const yoloSessionKey = "test-cli-yolo-session"

// Clears the bypass state around a case and returns a stand in CLI bound to the session id
func yoloStandIn(t *testing.T, sessionID string) *CLI {
	t.Helper()
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

func TestHelperReflectsToggle(t *testing.T) {
	c := yoloStandIn(t, yoloSessionKey)
	withConsoleInput(t, "")
	if c.isSessionYoloActive() {
		t.Fatal("active before toggle")
	}
	c.toggleYolo()
	if !c.isSessionYoloActive() {
		t.Fatal("inactive after on")
	}
	c.toggleYolo()
	if c.isSessionYoloActive() {
		t.Fatal("active after off")
	}
}

func TestHelperHonorsFrozenYoloMode(t *testing.T) {
	c := yoloStandIn(t, yoloSessionKey)
	t.Cleanup(tools.SwapYoloModeFrozen(true))
	if !c.isSessionYoloActive() {
		t.Fatal("frozen yolo not reflected")
	}
}

func TestToggleUnderFrozenYoloReportsLockedAndStaysOn(t *testing.T) {
	c := yoloStandIn(t, yoloSessionKey)
	out := withConsoleInput(t, "")
	t.Cleanup(tools.SwapYoloModeFrozen(true))
	c.toggleYolo()
	c.toggleYolo()
	if !c.isSessionYoloActive() || tools.IsSessionYoloEnabled(yoloSessionKey) {
		t.Fatal("frozen toggle changed the session flag")
	}
	printed := out.String()
	if !strings.Contains(printed, "locked ON") || strings.Contains(printed, "will require approval") {
		t.Fatalf("printed %q", printed)
	}
}

func TestToggleYoloBypassesDangerousCommandCheck(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	c := yoloStandIn(t, yoloSessionKey)
	withConsoleInput(t, "")
	env := &tools.Env{Home: h}
	tools.SetCurrentSessionKey(env, yoloSessionKey)
	t.Cleanup(func() { tools.ResetCurrentSessionKey(env) })
	c.toggleYolo()
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
