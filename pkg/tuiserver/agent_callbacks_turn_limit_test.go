package tuiserver

import (
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Clears the TUI turn cap override for one case
func unsetTUIMaxTurns(t *testing.T) {
	t.Helper()
	t.Setenv("ICEBATH_TUI_MAX_TURNS", "")
	os.Unsetenv("ICEBATH_TUI_MAX_TURNS")
}

func TestTUIResolverStringNoneResolvesToUnlimited(t *testing.T) {
	unsetTUIMaxTurns(t)
	if got := cfgMaxTurns(map[string]any{"agent": map[string]any{"max_turns": "none"}}, 25); got != config.TurnLimitUnlimited {
		t.Fatalf("got %d", got)
	}
}

func TestTUIResolverIntPassthrough(t *testing.T) {
	unsetTUIMaxTurns(t)
	if got := cfgMaxTurns(map[string]any{"agent": map[string]any{"max_turns": 50}}, 25); got != 50 {
		t.Fatalf("got %d", got)
	}
}

func TestTUIResolverNoneFallsThroughToDefault(t *testing.T) {
	unsetTUIMaxTurns(t)
	if got := cfgMaxTurns(map[string]any{"agent": map[string]any{}}, 25); got != 25 {
		t.Fatalf("got %d", got)
	}
}

func TestTUIResolverEnvVarOverride(t *testing.T) {
	t.Setenv("ICEBATH_TUI_MAX_TURNS", "none")
	if got := cfgMaxTurns(map[string]any{"agent": map[string]any{"max_turns": 50}}, 25); got != config.TurnLimitUnlimited {
		t.Fatalf("got %d", got)
	}
}

func TestTUIResolverEnvVarZeroIsUnlimited(t *testing.T) {
	t.Setenv("ICEBATH_TUI_MAX_TURNS", "0")
	if got := cfgMaxTurns(map[string]any{"agent": map[string]any{"max_turns": 50}}, 25); got != config.TurnLimitUnlimited {
		t.Fatalf("got %d", got)
	}
}

func TestTUIResolverRootLevelMaxTurns(t *testing.T) {
	unsetTUIMaxTurns(t)
	if got := cfgMaxTurns(map[string]any{"max_turns": "none"}, 25); got != config.TurnLimitUnlimited {
		t.Fatalf("got %d", got)
	}
}
