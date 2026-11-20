package gatewayapi

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/state"
)

// Runner double of the owner fixture, holding the launch authority and resolving every source to the API adapter
type ownerRunner struct {
	store     *gateway.SessionStore
	authority *gateway.SessionAuthority
	adapter   gateway.PlatformAdapter
}

func (r *ownerRunner) SessionRuntimeDescriptor() map[string]any { return map[string]any{} }
func (r *ownerRunner) Draining() bool                           { return false }
func (r *ownerRunner) Running() bool                            { return true }
func (r *ownerRunner) SessionStore() *gateway.SessionStore      { return r.store }
func (r *ownerRunner) MultiplexProfiles() bool                  { return false }
func (r *ownerRunner) SessionAuthority() *gateway.SessionAuthority {
	return r.authority
}
func (r *ownerRunner) AdapterForSource(context.Context, *gateway.SessionSource) (gateway.PlatformAdapter, error) {
	return r.adapter, nil
}

// Canonical authority over a fresh profile store and the API server adapter bound to its runner and database
func newOwnerAPI(t *testing.T) (context.Context, *gateway.SessionAuthority, *APIServerAdapter) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, filepath.Join(dir, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &ownerRunner{store: gateway.NewSessionStore(ctx, filepath.Join(dir, "sessions"), &gateway.GatewayConfig{}, nil)}
	runner.store.PinDB(db)
	epoch, err := db.BeginRuntimeEpoch(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	runner.authority = gateway.NewSessionAuthority(runner, "default", "first", db, epoch)
	adapter, err := NewAPIServerAdapter(ctx, &gateway.PlatformConfig{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	adapter.GatewayRunner = runner
	adapter.sessionDB = db
	runner.adapter = adapter
	t.Cleanup(func() {
		if adapter.responseStore != nil {
			adapter.responseStore.Close(ctx)
		}
		if adapter.runIdempotencyStore != nil {
			adapter.runIdempotencyStore.Close()
		}
	})
	return ctx, runner.authority, adapter
}

// Asserts err is the runtime refusal with reason
func requireRefusal(t *testing.T, err error, reason string) {
	t.Helper()
	var refusal *state.RuntimeStoreError
	if !errors.As(err, &refusal) || refusal.Reason != reason {
		t.Fatalf("err = %v, want refusal %s", err, reason)
	}
}
