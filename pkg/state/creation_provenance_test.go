package state

import (
	"context"
	"testing"
)

func TestPreSeededCreatorRemainsVisibleAfterGatewayRouting(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	if err := db.UpdateTokenCounts("previous", TokenCountDelta{InputTokens: 5}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(NewSession{ID: "previous", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetSessionTitle(ctx, "previous", "Previous conversation"); err != nil {
		t.Fatal(err)
	}
	key, chat := "local:previous", "previous"
	if err := db.RecordGatewaySessionPeer(ctx, "previous", GatewayPeer{Source: "local", SessionKey: &key, ChatID: &chat}); err != nil {
		t.Fatal(err)
	}
	if got := crossField(t, db, "previous", "created_source"); got != "cli" {
		t.Fatal(got)
	}
	if got := crossField(t, db, "previous", "input_tokens"); got != int64(5) {
		t.Fatal(got)
	}
}

func TestRoutingLabelLocalIsNotCreationProvenance(t *testing.T) {
	db := usageDB(t)
	if err := db.UpdateTokenCounts("placeholder", TokenCountDelta{InputTokens: 1}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"placeholder", "frozen"} {
		if err := db.CreateSession(NewSession{ID: id, Source: "local"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := crossField(t, db, "placeholder", "created_source"); got != "unknown" {
		t.Fatal(got)
	}
	if got := crossField(t, db, "frozen", "created_source"); got != "local" {
		t.Fatal(got)
	}
	for _, id := range []string{"placeholder", "frozen"} {
		if err := db.CreateSession(NewSession{ID: id, Source: "cli"}); err != nil {
			t.Fatal(err)
		}
		if got := crossField(t, db, id, "created_source"); got != "cli" {
			t.Fatal(id, got)
		}
	}
}
