package tools

import (
	"sync"
	"testing"
)

func TestSessionEnvReadsSessionValuesThenProcessEnv(t *testing.T) {
	t.Setenv("ICEBATH_SESSION_KEY", "from-process")
	if got := SessionEnv(nil, "ICEBATH_SESSION_KEY"); got != "from-process" {
		t.Fatalf("process env fallback got %q", got)
	}
	env := &Env{Values: &sync.Map{}}
	env.Values.Store("session.key", "from-values")
	if got := SessionEnv(env, "ICEBATH_SESSION_KEY"); got != "from-values" || got != sessionContextValue(env, "ICEBATH_SESSION_KEY") {
		t.Fatalf("session value got %q", got)
	}
}

func TestScopeTerminalCwdMatchesTheTerminalScope(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TERMINAL_CWD", dir)
	env := &Env{Home: t.TempDir()}
	got, err := ScopeTerminalCwd(env)
	want, wantErr := scopeTerminalCwd(env)
	if err != nil || wantErr != nil {
		t.Fatalf("errors %v %v", err, wantErr)
	}
	if got != want || got != dir {
		t.Fatalf("got %q want %q and %q", got, want, dir)
	}
}

func TestSessionToolEnvHookIsUnsetUntilInstalled(t *testing.T) {
	if SessionToolEnv != nil {
		t.Fatal("hook installed before the agent package sets it")
	}
	SessionToolEnv = func(env *Env) map[string]string { return map[string]string{"PATH": env.Home} }
	t.Cleanup(func() { SessionToolEnv = nil })
	if got := SessionToolEnv(&Env{Home: "/h"}); got["PATH"] != "/h" {
		t.Fatalf("installed hook got %v", got)
	}
}

func TestFormatProcessNotificationMatchesTheRegistryFormatter(t *testing.T) {
	if got := FormatProcessNotification(map[string]any{"type": "watch_disabled", "message": "watch off"}); got != "[IMPORTANT: watch off]" {
		t.Fatalf("watch event got %q", got)
	}
	evt := map[string]any{"session_id": "proc_1", "command": "make", "exit_code": 0, "output": "done"}
	if got, want := FormatProcessNotification(evt), formatProcessNotification(evt); got != want || got == "" {
		t.Fatalf("completion got %q want %q", got, want)
	}
}
