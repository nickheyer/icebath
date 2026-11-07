package tools

import (
	"testing"
)

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
