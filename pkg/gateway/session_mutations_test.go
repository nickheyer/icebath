package gateway

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// test_delete_secondary_profile_multiplex.py test_runtime_owns_authority_requires_runner_membership
func TestRuntimeOwnsAuthorityRequiresRunnerMembership(t *testing.T) {
	launch, secondary := &SessionAuthority{ProfileID: t.TempDir()}, &SessionAuthority{ProfileID: t.TempDir()}
	runner := &recoverTestRunner{registry: servedRegistry(t, launch, secondary, "secondary"), launch: launch}
	launch.Runner, secondary.Runner = runner, runner
	if !runtimeOwnsAuthority(launch) || !runtimeOwnsAuthority(secondary) {
		t.Fatal("a served authority is owned by its runner")
	}
	other := &SessionAuthority{ProfileID: t.TempDir()}
	foreignRegistry := NewSessionAuthorities(other.ProfileID, false)
	if err := foreignRegistry.Add(other.ProfileID, other, nil); err != nil {
		t.Fatal(err)
	}
	foreign := &SessionAuthority{Runner: &recoverTestRunner{registry: foreignRegistry, launch: &SessionAuthority{}}}
	if runtimeOwnsAuthority(foreign) {
		t.Fatal("an authority the runner does not hold keeps the coordination refusal")
	}
	if runtimeOwnsAuthority(&SessionAuthority{}) {
		t.Fatal("an authority with no runner is not owned")
	}
	legacy := &SessionAuthority{}
	legacyRunner := &recoverTestRunner{launch: legacy}
	legacy.Runner = legacyRunner
	if !runtimeOwnsAuthority(legacy) {
		t.Fatal("a registry less runner owns its single launch authority")
	}
	if runtimeOwnsAuthority(&SessionAuthority{Runner: legacyRunner}) {
		t.Fatal("another authority of a registry less runner is not owned")
	}
}

// Launch home routing store whose fence write always fails
type unfencedStoreDB struct{ SessionStoreDB }

// test_delete_secondary_profile_multiplex.py test_a_failed_shared_fence_fails_the_canonical_delete_not_the_prune
func TestAFailedSharedFenceFailsTheCanonicalDeleteNotThePrune(t *testing.T) {
	ctx, db := newTestStateDB(t)
	launchDB, err := state.NewSessionDB(ctx, filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	store := NewSessionStore(ctx, filepath.Join(t.TempDir(), "sessions"), DefaultGatewayConfig(ctx), nil)
	t.Cleanup(func() { _ = launchDB.Close() })
	store.PinDB(unfencedStoreDB{launchDB})
	a := NewSessionAuthority(&recoverTestRunner{store: store}, "p", "current", db, 1)
	err = retireLiveSessions(context.Background(), a, []string{"gone"}, true)
	if !isRuntimeRefusal(err, "storage_unavailable") {
		t.Fatalf("canonical delete must refuse storage_unavailable, got %v", err)
	}
	if err := retireLiveSessions(context.Background(), a, []string{"gone"}, false); err != nil {
		t.Fatalf("a prune keeps its committed result, got %v", err)
	}
}
