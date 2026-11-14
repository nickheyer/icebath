package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type desktopSeen struct {
	sid, event string
	payload    map[string]any
}

// Installs an emitter appending every event, cleared after the test
func recordDesktopEvents(t *testing.T) *[]desktopSeen {
	t.Helper()
	seen := &[]desktopSeen{}
	SetDesktopEmitter(func(_ *Env, sid, event string, payload map[string]any) error {
		*seen = append(*seen, desktopSeen{sid, event, payload})
		return nil
	})
	t.Cleanup(func() { SetDesktopEmitter(nil) })
	return seen
}

func TestDesktopUnavailableWithoutEmitter(t *testing.T) {
	SetDesktopEmitter(nil)
	if ok, err := desktopEmitEvent(desktopTestEnv(t, ""), "preview.open", map[string]any{"url": "x"}); DesktopAvailable() || ok || err != nil {
		t.Fatalf("available %v ok %v err %v", DesktopAvailable(), ok, err)
	}
}

func TestDesktopRoutesEventToOwningWindow(t *testing.T) {
	t.Setenv("ICEBATH_UI_SESSION_ID", "win-7")
	t.Setenv("ICEBATH_SESSION_ID", "")
	seen := recordDesktopEvents(t)
	if !DesktopAvailable() {
		t.Fatal("emitter not wired")
	}
	if ok, _ := desktopEmitEvent(desktopTestEnv(t, ""), "pane.reveal", map[string]any{"pane": "terminal"}); !ok {
		t.Fatal("emit failed")
	}
	if want := []desktopSeen{{"win-7", "pane.reveal", map[string]any{"pane": "terminal"}}}; !reflect.DeepEqual(*seen, want) {
		t.Fatalf("%v", *seen)
	}
}

func TestDesktopSessionScopedEventUsesSessionID(t *testing.T) {
	t.Setenv("ICEBATH_UI_SESSION_ID", "win-3")
	t.Setenv("ICEBATH_SESSION_ID", "sess-9")
	seen := recordDesktopEvents(t)
	payload := map[string]any{"row_id": 42, "reactions": []any{"❤️"}}
	if ok, _ := desktopEmitEvent(desktopTestEnv(t, ""), "message.reaction", payload); !ok {
		t.Fatal("emit failed")
	}
	if want := []desktopSeen{{"sess-9", "message.reaction", payload}}; !reflect.DeepEqual(*seen, want) {
		t.Fatalf("%v", *seen)
	}
}

func TestDesktopSessionScopedFallsBackToUISessionID(t *testing.T) {
	t.Setenv("ICEBATH_UI_SESSION_ID", "win-5")
	t.Setenv("ICEBATH_SESSION_ID", "")
	seen := recordDesktopEvents(t)
	payload := map[string]any{"row_id": 1, "reactions": []any{"\U0001F44D"}}
	if ok, _ := desktopEmitEvent(desktopTestEnv(t, ""), "message.reaction", payload); !ok {
		t.Fatal("emit failed")
	}
	if want := []desktopSeen{{"win-5", "message.reaction", payload}}; !reflect.DeepEqual(*seen, want) {
		t.Fatalf("%v", *seen)
	}
}

func TestDesktopEmitOrErrorWritesPythonJSON(t *testing.T) {
	env := desktopTestEnv(t, "")
	recordDesktopEvents(t)
	if got := focusPaneTool(env, " Chat "); got != `{"success": true, "pane": "chat"}` {
		t.Fatalf("%s", got)
	}
	SetDesktopEmitter(func(*Env, string, string, map[string]any) error { return errors.New("socket closed") })
	if got := mustJSON(t, focusPaneTool(env, "chat")); got["error"] != "Failed to focus the chat pane: socket closed" {
		t.Fatalf("%v", got)
	}
	SetDesktopEmitter(nil)
	if got := mustJSON(t, focusPaneTool(env, "chat")); got["error"] != "Pane focus is only available in the Icebath desktop app." {
		t.Fatalf("%v", got)
	}
}

func TestDesktopPassthroughJSONKeepsKeyOrder(t *testing.T) {
	for raw, want := range map[string]string{
		`{"z":1,"a":[true,null],"é":"ü"}`: `{"z": 1, "a": [true, null], "é": "ü"}`,
		`not json`:                        `{"text": "not json"}`,
		`NaN`:                             `NaN`,
	} {
		if got := desktopPassthroughJSON(raw); got != want {
			t.Fatalf("%s -> %s want %s", raw, got, want)
		}
	}
}

// Writes a display section into the env home config
func writeDisplayConfig(t *testing.T, env *Env, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(env.Home, "config.yaml"), []byte("display:\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAMissingSettingLeavesTheFeatureAtItsDefault(t *testing.T) {
	env := desktopTestEnv(t, "")
	if !desktopUserEnabled(env, "in_app_tips", true) || desktopUserEnabled(env, "message_reactions", false) {
		t.Fatal("absence changed the default")
	}
}

func TestTheUsersAnswerWinsInBothDirections(t *testing.T) {
	env := desktopTestEnv(t, "")
	writeDisplayConfig(t, env, "  in_app_tips: false\n  message_reactions: true\n")
	if desktopUserEnabled(env, "in_app_tips", true) || !desktopUserEnabled(env, "message_reactions", false) {
		t.Fatal("user answer ignored")
	}
}

func TestSwitchingAFeatureOffWithdrawsItsTool(t *testing.T) {
	for _, tc := range []struct{ setting, tool string }{{"in_app_tips", "show_tip"}, {"in_app_tours", "gui_tour"}} {
		env := desktopTestEnv(t, "")
		def := Default.Entry(context.Background(), env, tc.tool)
		if def == nil || def.Check == nil {
			t.Fatalf("%s not registered with a check", tc.tool)
		}
		if !def.Check(env) {
			t.Fatalf("%s default off", tc.tool)
		}
		writeDisplayConfig(t, env, "  "+tc.setting+": false\n")
		if def.Check(env) {
			t.Fatalf("%s still on", tc.tool)
		}
		writeDisplayConfig(t, env, "  "+tc.setting+": true\n")
		if !def.Check(env) {
			t.Fatalf("%s still off", tc.tool)
		}
	}
}
