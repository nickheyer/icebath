package state

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func listingHome(t *testing.T, show string) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("sessions:\n  show_subagents: "+show+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestSubagentListingScopeFollowsTheStoreHomeConfig(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	off, on, spelled := listingHome(t, "false"), listingHome(t, "true"), listingHome(t, `" Yes "`)
	if ShowSubagentSessions(ctx, off) {
		t.Fatal("off reported on")
	}
	if !ShowSubagentSessions(ctx, on) {
		t.Fatal("on reported off")
	}
	if !ShowSubagentSessions(ctx, spelled) {
		t.Fatal("string spelling not truthy")
	}
	if ShowSubagentSessions(ctx, filepath.Join(t.TempDir(), "missing")) {
		t.Fatal("unreadable home reported on")
	}
}
