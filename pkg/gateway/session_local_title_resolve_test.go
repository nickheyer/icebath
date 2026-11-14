package gateway

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func titledLocal(t *testing.T, ctx context.Context, db *state.SessionDB, a *SessionAuthority, sid, user, title string) {
	t.Helper()
	if err := db.CreateSession(state.NewSession{ID: sid, Source: "cli", ChatID: sid, UserID: user}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetSessionTitle(ctx, sid, title); err != nil {
		t.Fatal(err)
	}
	owner := user
	a.Sessions[sid] = NewLiveSession(&SessionSource{Platform: PlatformLocal, ChatID: sid, UserID: &owner, ChatType: "dm"}, "route-"+sid)
}

func TestTitleResolvesLikeClassicCliAndNeverConfirmsForeignTitles(t *testing.T) {
	saved, savedLabels := state.ParseAuthenticatedSubject, state.MessagingPlatformLabels
	state.ParseAuthenticatedSubject, state.MessagingPlatformLabels = ParseAuthenticatedSubject, MessagingPlatformLabels
	t.Cleanup(func() { state.ParseAuthenticatedSubject, state.MessagingPlatformLabels = saved, savedLabels })
	ctx, db := newTestStateDB(t)
	epoch, err := db.BeginRuntimeEpoch(ctx, "current")
	if err != nil {
		t.Fatal(err)
	}
	a := NewSessionAuthority(&authorityRunnerDouble{}, "owned", "current", db, epoch)
	titledLocal(t, ctx, db, a, "local-a", "human", "thread")
	titledLocal(t, ctx, db, a, "local-a2", "human", "thread #2")
	titledLocal(t, ctx, db, a, "local-b", "stranger", "private")
	if err := db.CreateSession(state.NewSession{ID: "tg-1", Source: "telegram", ChatID: "chat", UserID: "human"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetSessionTitle(ctx, "tg-1", "telegram thread"); err != nil {
		t.Fatal(err)
	}
	human := Principal{Subject: "human", ProfileID: "owned", Capabilities: map[string]bool{"session:read": true}, TransportID: "viewer"}
	resolve := func(title string) (string, error) {
		ref, err := resolveTitledSession(ctx, a, human, title, false)
		if err != nil {
			return "", err
		}
		return ref.SessionID, nil
	}
	for title, want := range map[string]string{"thread": "local-a2", "thread #2": "local-a2", "local-a": "local-a"} {
		if got, err := resolve(title); err != nil || got != want {
			t.Fatalf("resolve(%q) = %q, %v; want %q", title, got, err, want)
		}
	}
	for _, foreign := range []string{"private", "telegram thread", "nope"} {
		if _, err := resolve(foreign); refusalReasonOf(err) != "not_found" {
			t.Fatalf("resolve(%q) err %v", foreign, err)
		}
	}
	if ref, err := resolveTitledSession(ctx, a, human, "nope", true); err != nil || ref != nil {
		t.Fatalf("missing ok %v %v", ref, err)
	}
	if _, err := resolve(""); refusalReasonOf(err) != "invalid_params" {
		t.Fatalf("empty title err %v", err)
	}
}
