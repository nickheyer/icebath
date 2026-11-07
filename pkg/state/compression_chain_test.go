package state

import (
	"context"
	"testing"
)

func TestIsCompressionChildRowExportedNamesOnlyCompressionContinuations(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "parent", Source: "telegram"})
	if err := db.EndSession("parent", "compression"); err != nil {
		t.Fatal(err)
	}
	readSession(t, db, NewSession{ID: "child", Source: "telegram", ParentSessionID: "parent"})
	readSession(t, db, NewSession{ID: "other", Source: "telegram"})
	for id, want := range map[string]bool{"child": true, "other": false} {
		row, err := db.GetSession(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		got, err := db.IsCompressionChildRow(ctx, row)
		if err != nil || got != want {
			t.Fatalf("%s: got %v, %v want %v", id, got, err, want)
		}
	}
}
