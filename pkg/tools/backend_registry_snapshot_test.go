package tools

import (
	"context"
	"testing"
)

type snapshotFakeBackend struct{ name string }

func (b *snapshotFakeBackend) Name() string                           { return b.name }
func (b *snapshotFakeBackend) DisplayName() string                    { return b.name }
func (b *snapshotFakeBackend) Description() string                    { return "" }
func (b *snapshotFakeBackend) EnvDescription() string                 { return "" }
func (b *snapshotFakeBackend) IsRemote() bool                         { return false }
func (b *snapshotFakeBackend) IsContainer() bool                      { return false }
func (b *snapshotFakeBackend) SkipContainerGuards() bool              { return false }
func (b *snapshotFakeBackend) CachePathBase() string                  { return "" }
func (b *snapshotFakeBackend) DefaultCwd() string                     { return "" }
func (b *snapshotFakeBackend) StripEnvKeys() []string                 { return nil }
func (b *snapshotFakeBackend) SessionIsolatedWhenNonpersistent() bool { return false }
func (b *snapshotFakeBackend) SupportsStreams() bool                  { return false }
func (b *snapshotFakeBackend) IsAvailable() bool                      { return true }
func (b *snapshotFakeBackend) CheckRequirements(*Env, map[string]any) error {
	return nil
}
func (b *snapshotFakeBackend) Probe() (string, string)         { return "ok", "" }
func (b *snapshotFakeBackend) SetupInstructions(*Env) []string { return nil }
func (b *snapshotFakeBackend) DoctorChecks() []DoctorCheck     { return nil }
func (b *snapshotFakeBackend) CreateEnvironment(context.Context, *Env, CreateOptions) (Environment, error) {
	return nil, nil
}

func TestRestoreTerminalBackendOnlyWhileCurrentHoldsTheSlot(t *testing.T) {
	first, second := &snapshotFakeBackend{"snapshot-fake"}, &snapshotFakeBackend{"snapshot-fake"}
	if SnapshotTerminalBackend("snapshot-fake") != nil {
		t.Fatal("slot taken before the test")
	}
	if err := RegisterTerminalBackend(first); err != nil {
		t.Fatal(err)
	}
	if RestoreTerminalBackend("snapshot-fake", second, nil) || GetTerminalBackend("snapshot-fake") != first {
		t.Fatal("a stale current must not restore")
	}
	if err := RegisterTerminalBackend(second); err != nil {
		t.Fatal(err)
	}
	if !RestoreTerminalBackend("Snapshot-Fake ", second, first) || GetTerminalBackend("snapshot-fake") != first {
		t.Fatal("previous comes back under the normalized name")
	}
	if !RestoreTerminalBackend("snapshot-fake", first, nil) || SnapshotTerminalBackend("snapshot-fake") != nil {
		t.Fatal("a nil previous empties the slot")
	}
}
