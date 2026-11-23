package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Clears the TUI turn cap override for one case
func unsetTUIMaxTurns(t *testing.T) {
	t.Helper()
	t.Setenv("ICEBATH_TUI_MAX_TURNS", "")
	os.Unsetenv("ICEBATH_TUI_MAX_TURNS")
}

// Context on a fresh home whose config.yaml holds the given text
func turnLimitHome(t *testing.T, yaml string) context.Context {
	t.Helper()
	h := t.TempDir()
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return home.WithOverride(context.Background(), h)
}

func TestTUIResolverIntPassthrough(t *testing.T) {
	unsetTUIMaxTurns(t)
	if got := cfgMaxTurns(turnLimitHome(t, "agent:\n  max_turns: 50\n"), 25); got != 50 {
		t.Fatalf("got %d", got)
	}
}

func TestTUIResolverUnsetFallsThroughToDefault(t *testing.T) {
	unsetTUIMaxTurns(t)
	if got := cfgMaxTurns(turnLimitHome(t, "agent: {}\n"), 25); got != 25 {
		t.Fatalf("got %d", got)
	}
}

func TestTUIResolverEnvVarOverride(t *testing.T) {
	t.Setenv("ICEBATH_TUI_MAX_TURNS", "none")
	if got := cfgMaxTurns(turnLimitHome(t, "agent:\n  max_turns: 50\n"), 25); got != config.TurnLimitUnlimited {
		t.Fatalf("got %d", got)
	}
}

func TestTUIResolverEnvVarZeroIsUnlimited(t *testing.T) {
	t.Setenv("ICEBATH_TUI_MAX_TURNS", "0")
	if got := cfgMaxTurns(turnLimitHome(t, "agent:\n  max_turns: 50\n"), 25); got != config.TurnLimitUnlimited {
		t.Fatalf("got %d", got)
	}
}
