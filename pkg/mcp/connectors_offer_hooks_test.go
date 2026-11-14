package mcp

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Nous state after one status read of rows under an identity with the given auth method
func offerHookState(t *testing.T, method string, rows []any) map[string]any {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	store := provider.NewStore(h)
	if _, err := store.SaveActiveProviderState("nous", map[string]any{"auth_method": method}); err != nil {
		t.Fatal(err)
	}
	tools.NoteConnectedAppsSeen(home.WithOverride(context.Background(), h), &tools.Env{Home: h}, rows)
	data, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	return store.ProviderState(data, "nous")
}

func TestInstalledOfferHooksFlagAGuestOnItsFirstConnectedApp(t *testing.T) {
	connected := []any{map[string]any{"connector": "gmail", "connected": false}, map[string]any{"connector": "linear", "connected": true}}
	if state := offerHookState(t, provider.AnonAccountTier, connected); state[provider.ConnectedAppFlag] != true {
		t.Fatalf("guest state %v", state)
	}
	if state := offerHookState(t, provider.AnonAccountTier, connected[:1]); state[provider.ConnectedAppFlag] != nil {
		t.Fatalf("no connected app flagged %v", state)
	}
	if state := offerHookState(t, "oauth", connected); state[provider.ConnectedAppFlag] != nil {
		t.Fatalf("signed in state flagged %v", state)
	}
}
