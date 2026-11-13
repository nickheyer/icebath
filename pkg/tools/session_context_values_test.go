package tools

import "testing"

func TestSetSessionVarsBindsAnEnvWithoutAValueStore(t *testing.T) {
	isolateSessionContext(t, false)
	env := &Env{}
	restoreSessionContextLatch(t)
	SetSessionVars(env, SessionVars{Platform: "cli", SessionID: "s1"})
	if env.Values == nil {
		t.Fatal("no value store allocated")
	}
	if got := GetSessionEnv(env, "ICEBATH_SESSION_ID", ""); got != "s1" {
		t.Fatalf("session id %q", got)
	}
	if got := GetSessionEnv(env, "ICEBATH_SESSION_PLATFORM", ""); got != "cli" {
		t.Fatalf("platform %q", got)
	}
}
