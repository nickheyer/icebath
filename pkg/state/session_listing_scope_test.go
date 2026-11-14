package state

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
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
	if ok, ex := SubagentListingScope(ctx, off, "", nil, nil); ok || ex != nil {
		t.Fatalf("off %v %v", ok, ex)
	}
	if ok, ex := SubagentListingScope(ctx, on, "", nil, nil); !ok || ex != nil {
		t.Fatalf("on %v %v", ok, ex)
	}
	if !ShowSubagentSessions(ctx, spelled) {
		t.Fatal("string spelling not truthy")
	}
	if ok, ex := SubagentListingScope(ctx, on, "", nil, []string{"cron", "subagent"}); !ok || !reflect.DeepEqual(ex, []string{"cron"}) {
		t.Fatalf("recents %v %v", ok, ex)
	}
	if ok, ex := SubagentListingScope(ctx, on, "", nil, []string{"subagent"}); !ok || ex != nil {
		t.Fatalf("only subagent %v %v", ok, ex)
	}
	if ok, _ := SubagentListingScope(ctx, on, "cron", nil, nil); ok {
		t.Fatal("named source joined subagents")
	}
	if ok, _ := SubagentListingScope(ctx, on, "", []string{"telegram"}, nil); ok {
		t.Fatal("named sources joined subagents")
	}
	other := []string{"cron", "cli", "desktop"}
	if ok, ex := SubagentListingScope(ctx, on, "", nil, other); ok || !reflect.DeepEqual(ex, other) {
		t.Fatalf("platform slice %v %v", ok, ex)
	}
	if ShowSubagentSessions(ctx, filepath.Join(t.TempDir(), "missing")) {
		t.Fatal("unreadable home reported on")
	}
}
