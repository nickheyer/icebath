package agent

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func TestStampedEpochSatisfiesTheRequestAfterTheStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, err := state.OpenHome(filepath.Join(t.TempDir(), "home"), state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sid := "prompt-refresh-session"
	if err := db.EnsureSession(state.NewSession{ID: sid, Source: "whatsapp"}); err != nil {
		t.Fatal(err)
	}
	a := &AIAgent{sessionDB: db, sessionID: sid}
	row, _ := db.GetSession(ctx, sid)
	if got := storedPromptRefreshEpoch(ctx, a, row); got != 0 || a.promptRefreshEpoch == nil || *a.promptRefreshEpoch != 0 {
		t.Fatalf("no request rebuilt at %d", got)
	}
	first, err := db.RequestPromptRefresh(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	row, _ = db.GetSession(ctx, sid)
	if got := storedPromptRefreshEpoch(ctx, a, row); got != first {
		t.Fatalf("pending request rebuilt at %d, want %d", got, first)
	}
	stampPromptEpoch(a, first)
	row, _ = db.GetSession(ctx, sid)
	if got := storedPromptRefreshEpoch(ctx, a, row); got != 0 || *a.promptRefreshEpoch != first {
		t.Fatalf("stamped epoch %d rebuilt again at %d", first, got)
	}
	second, err := db.RequestPromptRefresh(ctx, &sid)
	if err != nil || second <= first {
		t.Fatalf("session request epoch %d after %d: %v", second, first, err)
	}
	row, _ = db.GetSession(ctx, sid)
	if got := storedPromptRefreshEpoch(ctx, a, row); got != second {
		t.Fatalf("session scoped request rebuilt at %d, want %d", got, second)
	}
	if got := intEpoch(int64(9)); got != 9 {
		t.Fatalf("int64 epoch read as %d", got)
	}
}
