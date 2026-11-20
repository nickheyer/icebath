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

func TestLoadCheckpointsEnabledDefaultsToFalseWhenNoConfigSection(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if loadCheckpointsEnabled(map[string]any{}) {
		t.Fatal("no checkpoints section must fall back to the disabled default")
	}
}

func TestLoadCheckpointsEnabledHonorsConfigEnabledTrue(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if !loadCheckpointsEnabled(map[string]any{"checkpoints": map[string]any{"enabled": true}}) {
		t.Fatal("checkpoints enabled true must enable")
	}
}

func TestLoadCheckpointsEnabledHonorsConfigEnabledFalse(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if loadCheckpointsEnabled(map[string]any{"checkpoints": map[string]any{"enabled": false}}) {
		t.Fatal("checkpoints enabled false must disable")
	}
}

func TestLoadCheckpointsEnabledLegacyBoolForm(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if !loadCheckpointsEnabled(map[string]any{"checkpoints": true}) {
		t.Fatal("legacy checkpoints true must enable")
	}
	if loadCheckpointsEnabled(map[string]any{"checkpoints": false}) {
		t.Fatal("legacy checkpoints false must disable")
	}
}

func TestLoadCheckpointsEnabledMalformedSectionFallsBackToDefault(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if loadCheckpointsEnabled(map[string]any{"checkpoints": "enabled"}) {
		t.Fatal("a malformed section must fall back to the disabled default")
	}
}

func TestLoadCheckpointsEnabledNoneCfgFallsBack(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if loadCheckpointsEnabled(nil) {
		t.Fatal("a nil config must fall back to the disabled default")
	}
}

func TestResolveCheckpointsEnabledConfigEnabledUsedWhenEnvUnset(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	unsetTUICheckpoints(t)
	if !resolveCheckpointsEnabled(map[string]any{"checkpoints": map[string]any{"enabled": true}}) {
		t.Fatal("config enabled must win without the env opt in")
	}
}

func TestResolveCheckpointsEnabledConfigDisabledUsedWhenEnvUnset(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	unsetTUICheckpoints(t)
	if resolveCheckpointsEnabled(map[string]any{"checkpoints": map[string]any{"enabled": false}}) {
		t.Fatal("config disabled must win without the env opt in")
	}
}

func TestResolveCheckpointsEnabledEnvVarOverridesConfig(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_TUI_CHECKPOINTS", "1")
	if !resolveCheckpointsEnabled(map[string]any{"checkpoints": map[string]any{"enabled": false}}) {
		t.Fatal("the env opt in must win over config")
	}
}

func TestResolveCheckpointsEnabledEnvVarFalseOverridesConfigTrue(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_TUI_CHECKPOINTS", "0")
	if resolveCheckpointsEnabled(map[string]any{"checkpoints": map[string]any{"enabled": true}}) {
		t.Fatal("an env opt out must win over config")
	}
}
