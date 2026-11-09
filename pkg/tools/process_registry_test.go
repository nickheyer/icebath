package tools

import "testing"

func TestOneshotCompletionWaitSecondsDefaultsToTenMinutes(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	env := &Env{Home: h, Cwd: h}
	if got := OneshotCompletionWaitSeconds(env); got != 600 {
		t.Fatalf("got %v", got)
	}
}
