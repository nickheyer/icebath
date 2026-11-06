package tools

import (
	"context"
	"testing"
)

// Installs the offer hook doubles pkg/mcp wires in production, this binary never links pkg/mcp
func init() {
	NoteAppConnected = func(context.Context, *Env) {}
	NoteConnectedAppsSeen = func(context.Context, *Env, []any) {}
}

func TestStatusReadHandsEveryListedRowToTheOfferHook(t *testing.T) {
	env := managedTestSetup(t)
	t.Setenv("ICEBATH_HOME", env.Home)
	oldPortal := NousPortalBaseURL
	NousPortalBaseURL = func(context.Context) (string, error) { return "https://portal.test", nil }
	t.Cleanup(func() { NousPortalBaseURL = oldPortal })
	var seen [][]any
	NoteConnectedAppsSeen = func(_ context.Context, _ *Env, rows []any) { seen = append(seen, rows) }
	client := &toolCasesClient{}
	ManageConnections(context.Background(), env, map[string]any{"action": "status"},
		ManageOptions{ClientFactory: func() (ManagedClient, error) { return client, nil }})
	if len(seen) != 1 || len(seen[0]) != 2 {
		t.Fatalf("seen %v", seen)
	}
	if row, _ := seen[0][1].(map[string]any); row["connector"] != "linear" || row["connected"] != true {
		t.Fatalf("rows %v", seen[0])
	}
}
