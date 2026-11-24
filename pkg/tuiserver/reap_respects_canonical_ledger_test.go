package tuiserver

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Second handle on the session store of a case, the gateway side that writes the ledger
func ledgerHandle(t *testing.T, dbPath string) *state.DB {
	t.Helper()
	db, err := state.NewSessionDB(context.Background(), dbPath, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
