package main

import (
	"context"
	"testing"
)

func TestArgparseReconfigureFlagReachesCmdSetup(t *testing.T) {
	var captured map[string]any
	prev := runSetupWizard
	runSetupWizard = func(_ context.Context, ns map[string]any) (int, error) {
		if captured == nil {
			captured = ns
		}
		return 0, nil
	}
	t.Cleanup(func() { runSetupWizard = prev })
	if err := newCLI().Execute([]string{"setup", "--reconfigure"}); err != nil {
		t.Fatal(err)
	}
	if captured["reconfigure"] != true || captured["quick"] != false {
		t.Fatalf("namespace %v", captured)
	}
}
