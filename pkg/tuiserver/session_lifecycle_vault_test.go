package tuiserver

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// test_vault_backends.py release half through _finalize_session, a sibling close keeps the token and the owner close drops it
func TestOnlyTheOwningSessionReleaseDropsAToken(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Cleanup(browser.LockAllVaultProfiles)
	ctx := tools.WithThreadLocals(context.Background())
	owner := "sess-A"
	browser.SetVaultSessionID(ctx, &owner)
	generation := browser.BeginVaultUnlock(ctx, "bitwarden")
	if !browser.StoreVaultSessionToken(ctx, "bitwarden", "SESSION-TOKEN", &generation) {
		t.Fatal("the unlock made under sess-A must store its token")
	}
	browser.SetVaultSessionID(ctx, nil)
	if !browser.IsVaultUnlocked(ctx, "bitwarden") {
		t.Fatal("the manager must read unlocked after the unlock")
	}
	finalizeSession(ctx, tuigateway.NewSession(map[string]any{"_sid": "sess-B"}), "tui_close")
	if !browser.IsVaultUnlocked(ctx, "bitwarden") {
		t.Fatal("closing an unrelated sibling session must keep the unlock")
	}
	finalizeSession(ctx, tuigateway.NewSession(map[string]any{"_sid": "sess-A"}), "tui_close")
	if browser.IsVaultUnlocked(ctx, "bitwarden") {
		t.Fatal("closing the session that unlocked must lock its manager")
	}
}
