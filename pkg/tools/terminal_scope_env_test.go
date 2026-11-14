package tools

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestEnvForProfileHomeRehomesAndSharesSessionValues(t *testing.T) {
	isolateSessionContext(t, false)
	launch, routed := t.TempDir(), filepath.Join(t.TempDir(), "profiles", "coder")
	if err := os.MkdirAll(routed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(routed, "config.yaml"), []byte("terminal:\n  backend: docker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", launch)
	base := &Env{Home: launch, Values: &sync.Map{}}
	restoreSessionContextLatch(t)
	SetSessionVars(base, SessionVars{SessionKey: "agent:main:cli", Profile: "default"})
	t.Cleanup(func() { ClearSessionVars(base) })
	ctx := WithProfileTerminalScope(context.Background(), routed, nil)
	scoped := EnvForProfileHome(ctx, base, routed)
	if scoped.Home != routed || scoped.TerminalScope == nil || scoped.TerminalScope != GetTerminalScope(ctx) {
		t.Fatalf("scoped home %q scope %v", scoped.Home, scoped.TerminalScope)
	}
	if got := GetSessionEnv(scoped, "ICEBATH_SESSION_PROFILE", ""); got != "default" {
		t.Fatalf("session profile %q", got)
	}
	if backend := scoped.Config.Get("terminal.backend"); backend != "docker" {
		t.Fatalf("config %v", scoped.Config)
	}
	if base.Home != launch || base.TerminalScope != nil {
		t.Fatal("base env mutated")
	}
}
