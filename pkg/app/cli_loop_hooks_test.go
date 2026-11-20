package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
)

// Isolated home whose goal and loop rows land in a fresh state database
func goalHooksHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(home.PatchUserHome(tmp))
	t.Setenv(home.EnvHome, h)
	goals.ClearSessionDBCache()
	t.Cleanup(func() {
		goals.ClearSessionDBCache()
		state.CloseAllUnder(h)
	})
	return h
}
