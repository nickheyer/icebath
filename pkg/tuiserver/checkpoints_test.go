package tuiserver

import (
	"os"
	"testing"
)

// Clears the checkpoint opt in for one case, restored afterwards
func unsetTUICheckpoints(t *testing.T) {
	t.Setenv("ICEBATH_TUI_CHECKPOINTS", "")
	if err := os.Unsetenv("ICEBATH_TUI_CHECKPOINTS"); err != nil {
		t.Fatal(err)
	}
}

func TestResolveCheckpointsEnabledDefaultsToFalse(t *testing.T) {
	unsetTUICheckpoints(t)
	if resolveCheckpointsEnabled(turnLimitHome(t, "model: {}\n")) {
		t.Fatal("no checkpoints section must fall back to the disabled default")
	}
}

func TestResolveCheckpointsEnabledConfigEnabledUsedWhenEnvUnset(t *testing.T) {
	unsetTUICheckpoints(t)
	if !resolveCheckpointsEnabled(turnLimitHome(t, "checkpoints:\n  enabled: true\n")) {
		t.Fatal("config enabled must win without the env opt in")
	}
}

func TestResolveCheckpointsEnabledConfigDisabledUsedWhenEnvUnset(t *testing.T) {
	unsetTUICheckpoints(t)
	if resolveCheckpointsEnabled(turnLimitHome(t, "checkpoints:\n  enabled: false\n")) {
		t.Fatal("config disabled must win without the env opt in")
	}
}

func TestResolveCheckpointsEnabledEnvVarOverridesConfig(t *testing.T) {
	t.Setenv("ICEBATH_TUI_CHECKPOINTS", "1")
	if !resolveCheckpointsEnabled(turnLimitHome(t, "checkpoints:\n  enabled: false\n")) {
		t.Fatal("the env opt in must win over config")
	}
}

func TestResolveCheckpointsEnabledEnvVarFalseOverridesConfigTrue(t *testing.T) {
	t.Setenv("ICEBATH_TUI_CHECKPOINTS", "0")
	if resolveCheckpointsEnabled(turnLimitHome(t, "checkpoints:\n  enabled: true\n")) {
		t.Fatal("an env opt out must win over config")
	}
}
