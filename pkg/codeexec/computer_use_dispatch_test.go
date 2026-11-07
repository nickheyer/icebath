package codeexec

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Bot desktop lease doubles with a settable holder and epoch
func cuLease(t *testing.T, human *bool, epoch *int) {
	t.Helper()
	prevAssert, prevEpoch, prevStart := BotDesktopAssertAgentMayAct, BotDesktopLeaseEpoch, BotDesktopEnsureStartedForTool
	BotDesktopAssertAgentMayAct = func(*tools.Env) (int, error) {
		if *human {
			return *epoch, errors.New("A human has control of this desktop")
		}
		return *epoch, nil
	}
	BotDesktopLeaseEpoch = func(*tools.Env) int { return *epoch }
	BotDesktopEnsureStartedForTool = func(*tools.Env) {}
	t.Cleanup(func() {
		BotDesktopAssertAgentMayAct, BotDesktopLeaseEpoch, BotDesktopEnsureStartedForTool = prevAssert, prevEpoch, prevStart
	})
}

// Injected noop backend under an approvals mode the test picks
func cuDispatchEnv(t *testing.T, approvals string) (*tools.Env, *noopBackend) {
	t.Helper()
	resetBackendForTests()
	t.Cleanup(resetBackendForTests)
	b := &noopBackend{}
	cuBackendMu.Lock()
	cuInjectedBackend = b
	cuBackendMu.Unlock()
	env := cuPluginHome(t, t.TempDir(), nil, "", "")
	env.Config = config.Tree{"approvals": map[string]any{"mode": approvals}}
	return env, b
}

func cuHandle(t *testing.T, env *tools.Env, args map[string]any) string {
	t.Helper()
	out, err := handleComputerUse(context.Background(), env, args, "")
	if err != nil {
		t.Fatal(err)
	}
	s, ok := out.(string)
	if !ok {
		t.Fatalf("result %T", out)
	}
	return s
}

func TestHandleComputerUseRefusesUnknownMissingAndHumanControl(t *testing.T) {
	human, epoch := false, 0
	cuLease(t, &human, &epoch)
	env, _ := cuDispatchEnv(t, "off")
	if got := cuHandle(t, env, map[string]any{"action": "  "}); got != `{"error": "missing `+"`action`"+`"}` {
		t.Fatalf("missing %s", got)
	}
	if got := cuHandle(t, env, map[string]any{"action": "Hotkey"}); !strings.Contains(got, "unknown action 'hotkey' \\u2014 did you mean 'key'?") {
		t.Fatalf("unknown %s", got)
	}
	if got := cuHandle(t, env, map[string]any{"action": "teleport"}); got != `{"error": "unknown action 'teleport'"}` {
		t.Fatalf("unknown without hint %s", got)
	}
	human = true
	if got := cuHandle(t, env, map[string]any{"action": "list_apps"}); !strings.HasPrefix(got, `{"ok": false, "action": "list_apps", "code": "human_has_control"`) {
		t.Fatalf("human %s", got)
	}
	if _, err := handleComputerUse(context.Background(), env, map[string]any{"action": 5}, ""); err == nil {
		t.Fatal("a non string action raises as str.strip does")
	}
}

func TestHandleComputerUseDispatchesThroughTheBackend(t *testing.T) {
	human, epoch := false, 3
	cuLease(t, &human, &epoch)
	env, b := cuDispatchEnv(t, "off")
	if got := cuHandle(t, env, map[string]any{"action": "list_apps"}); !strings.HasSuffix(got, `"count": 0}`) {
		t.Fatalf("list_apps %s", got)
	}
	if got := cuHandle(t, env, map[string]any{"action": "drag", "from_element": 1}); got != `{"error": "drag requires from_coordinate/to_coordinate or from_element/to_element"}` {
		t.Fatalf("drag %s", got)
	}
	if got := cuHandle(t, env, map[string]any{"action": "set_value"}); got != `{"error": "set_value requires `+"`value`"+`"}` {
		t.Fatalf("set_value %s", got)
	}
	if got := cuHandle(t, env, map[string]any{"action": "focus_app", "app": ""}); got != `{"error": "focus_app requires `+"`app`"+`"}` {
		t.Fatalf("focus_app %s", got)
	}
	if got := cuHandle(t, env, map[string]any{"action": "capture", "mode": "x"}); got != `{"error": "bad mode 'x'; use som|vision|ax"}` {
		t.Fatalf("capture %s", got)
	}
	if got := cuHandle(t, env, map[string]any{"action": "wait", "seconds": "abc"}); got != `{"error": "wait failed: could not convert string to float: 'abc'"}` {
		t.Fatalf("wait %s", got)
	}
	if got := cuHandle(t, env, map[string]any{"action": "double_click", "element": int64(4)}); !strings.Contains(got, `"ok": true`) {
		t.Fatalf("double_click %s", got)
	}
	if len(b.calls) == 0 || b.calls[len(b.calls)-1].name != "click" {
		t.Fatalf("calls %+v", b.calls)
	}
}

func TestHandleComputerUseFenceDiscardsAResultAfterTakeover(t *testing.T) {
	human, epoch := false, 1
	cuLease(t, &human, &epoch)
	env, _ := cuDispatchEnv(t, "off")
	prev := BotDesktopLeaseEpoch
	BotDesktopLeaseEpoch = func(*tools.Env) int { return epoch + 1 }
	t.Cleanup(func() { BotDesktopLeaseEpoch = prev })
	got := cuHandle(t, env, map[string]any{"action": "list_apps"})
	if !strings.Contains(got, `"code": "human_has_control"`) || !strings.Contains(got, "its result was discarded") {
		t.Fatalf("fence %s", got)
	}
}

func TestHandleComputerUseDestructiveActionFailsClosedWithoutHuman(t *testing.T) {
	human, epoch := false, 0
	cuLease(t, &human, &epoch)
	env, b := cuDispatchEnv(t, "manual")
	got := cuHandle(t, env, map[string]any{"action": "click", "element": int64(1)})
	if !strings.Contains(got, "BLOCKED: computer_use `click` requires approval but no interactive user or gateway is present") || len(b.calls) != 0 {
		t.Fatalf("approval %s calls %+v", got, b.calls)
	}
}

func TestCaptureAfterModeReadsConfig(t *testing.T) {
	for raw, want := range map[string]string{"": "som", " AX ": "ax", "vision": "vision", "bogus": "som"} {
		cuConfig(t, map[string]any{"computer_use": map[string]any{"capture_after_mode": raw}}, false)
		if got := captureAfterMode(&tools.Env{}); got != want {
			t.Fatalf("%q read %q want %q", raw, got, want)
		}
	}
}
