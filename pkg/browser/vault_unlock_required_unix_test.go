//go:build !windows

package browser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Executable stand in for a manager CLI that prints stderr and exits 1
func failingManagerCLI(t *testing.T, name, stderr string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho '"+stderr+"' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestManagerCallsRaiseUnlockRequiredAndDropADeadSession(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "")
	ctx := context.Background()
	t.Cleanup(LockAllVaultProfiles)

	bw := NewBitwardenLoginBackend(ctx, map[string]any{"binary_path": failingManagerCLI(t, "bw", "Vault is locked.")})
	_, err := bw.ResolvePassword("bw:item-1")
	var locked *UnlockRequired
	if !errors.As(err, &locked) || locked.Backend != bw || err.Error() != "Bitwarden is locked" {
		t.Fatalf("locked bw gave %v", err)
	}
	if !StoreVaultSessionToken(ctx, "bitwarden", "stale-token", nil) || !IsVaultUnlocked(ctx, "bitwarden") {
		t.Fatal("session token was not stored")
	}
	if _, err := bw.ResolvePassword("bw:item-1"); !errors.As(err, &locked) || IsVaultUnlocked(ctx, "bitwarden") {
		t.Fatalf("a locked vault reply gave %v and unlocked %v", err, IsVaultUnlocked(ctx, "bitwarden"))
	}

	broken := NewBitwardenLoginBackend(ctx, map[string]any{"binary_path": failingManagerCLI(t, "bw", "Not found.")})
	StoreVaultSessionToken(ctx, "bitwarden", "live-token", nil)
	if _, err := broken.ResolvePassword("bw:item-2"); err == nil || err.Error() != "bw failed: Not found.\n" || !IsVaultUnlocked(ctx, "bitwarden") {
		t.Fatalf("an unrelated bw failure gave %v", err)
	}

	op := NewOnePasswordLoginBackend(ctx, map[string]any{"binary_path": failingManagerCLI(t, "op", "[ERROR] You are not signed in")})
	if items, err := op.ListItems(); err != nil || len(items) != 0 {
		t.Fatalf("locked op listed %v err %v", items, err)
	}
	StoreVaultSessionToken(ctx, "onepassword", "stale-op", nil)
	_, err = op.ListItems()
	if !errors.As(err, &locked) || locked.Backend != op || err.Error() != "1Password is locked" || IsVaultUnlocked(ctx, "onepassword") {
		t.Fatalf("a signed out op reply gave %v and unlocked %v", err, IsVaultUnlocked(ctx, "onepassword"))
	}
}
