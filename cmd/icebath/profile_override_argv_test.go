package main

import (
	"os"
	"slices"
	"testing"
)

// The consumed profile flag leaves the process argv the way hermes rewrites sys.argv, so recorded argv omits it
func TestProfileFlagIsStrippedFromProcessArgv(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	saved, savedProfile := os.Args, explicitCLIProfile
	t.Cleanup(func() { os.Args, explicitCLIProfile = saved, savedProfile })
	os.Args = []string{"icebath", "--profile", "default", "gateway", "run", "--quiet", "--idle-exit"}
	got := applyProfileOverride(os.Args[1:])
	want := []string{"gateway", "run", "--quiet", "--idle-exit"}
	if !slices.Equal(got, want) {
		t.Fatalf("returned argv %q", got)
	}
	if !slices.Equal(os.Args, append([]string{"icebath"}, want...)) {
		t.Fatalf("process argv %q", os.Args)
	}
}
