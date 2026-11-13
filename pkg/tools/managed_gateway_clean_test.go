package tools

import (
	"sync"
	"testing"
)

func TestGatewayTokenCleaningAndDesktopRouting(t *testing.T) {
	isolateSessionContext(t, false)
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	env := &Env{Home: home, Values: &sync.Map{}}
	for in, want := range map[any]string{"  tok \n": "tok", "   ": "", 7: "", nil: ""} {
		if got := cleanGatewayValue(in); got != want {
			t.Fatalf("clean %#v = %q", in, got)
		}
	}
	t.Setenv("TOOL_GATEWAY_USER_TOKEN", "  tok  ")
	if got := readUserTokenOverride(env); got != "tok" {
		t.Fatalf("override %q", got)
	}

	old := NousAccount
	NousAccount = nil
	t.Cleanup(func() { NousAccount = old })
	if onbHostedNames(env) != nil {
		t.Fatal("unavailable connectors list no slugs")
	}

	var routed []string
	SetDesktopEmitter(func(_ *Env, sid, event string, _ map[string]any) error {
		routed = append(routed, event+"@"+sid)
		return nil
	})
	t.Cleanup(func() { SetDesktopEmitter(nil) })
	restoreSessionContextLatch(t)
	SetSessionVars(env, SessionVars{SessionID: "chat-1", UISessionID: "window-1"})
	for _, event := range []string{"message.reaction", "pane.focus"} {
		if ok, err := desktopEmitEvent(env, event, map[string]any{}); !ok || err != nil {
			t.Fatalf("emit %s %v %v", event, ok, err)
		}
	}
	if routed[0] != "message.reaction@chat-1" || routed[1] != "pane.focus@window-1" {
		t.Fatalf("routing %v", routed)
	}
}
