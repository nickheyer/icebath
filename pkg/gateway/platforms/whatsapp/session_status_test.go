package whatsapp

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func TestSessionStatusReadsTheWhatsmeowStore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	extra := map[string]any{"session_path": dir}
	if got, present, paired, err := SessionStatus(ctx, extra); err != nil || present || paired || got != dir {
		t.Fatalf("absent store %q %v %v %v", got, present, paired, err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, bridgeSessionDBName)+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := sqlstore.NewWithDB(db, "sqlite", waLog.Noop).Upgrade(ctx); err != nil {
		t.Fatal(err)
	}
	if _, present, paired, err := SessionStatus(ctx, extra); err != nil || !present || paired {
		t.Fatalf("unpaired store %v %v %v", present, paired, err)
	}
}
