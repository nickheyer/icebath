package gateway

import (
	"testing"
)

func TestAStaleOwnerCannotDeleteAStoredClose(t *testing.T) {
	ctx, db, a := newTestAuthority(t, "")
	record := `{"op": "x", "reason": "cli_close"}`
	if err := db.SetMeta(ctx, viewerClosePrefix+"chat", record); err != nil {
		t.Fatal(err)
	}
	stale := NewSessionAuthority(a.Runner, a.ProfileID, a.InstanceID, db, a.Epoch-1)
	if removed, err := forgetPersistedClose(ctx, stale, "chat", record); err != nil || removed {
		t.Fatalf("stale owner removed %v %v", removed, err)
	}
	if removed, err := forgetPersistedClose(ctx, a, "chat", "another close"); err != nil || removed {
		t.Fatalf("other record removed %v %v", removed, err)
	}
	if stored, found, err := db.GetMeta(ctx, viewerClosePrefix+"chat"); err != nil || !found || stored != record {
		t.Fatalf("stored %q %v %v", stored, found, err)
	}
	if removed, err := forgetPersistedClose(ctx, a, "chat", record); err != nil || !removed {
		t.Fatalf("owner kept its close %v %v", removed, err)
	}
	if _, found, err := db.GetMeta(ctx, viewerClosePrefix+"chat"); err != nil || found {
		t.Fatalf("close still stored %v %v", found, err)
	}
}
