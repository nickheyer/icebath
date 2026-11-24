package tuiserver

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Points the launch store at db for the test, restoring the shared handle afterwards
func useSharedDB(t *testing.T, db *state.DB) {
	t.Helper()
	dbMu.Lock()
	savedDB, savedErr := sharedDB, dbError
	sharedDB, dbError = db, nil
	dbMu.Unlock()
	t.Cleanup(func() {
		dbMu.Lock()
		sharedDB, dbError = savedDB, savedErr
		dbMu.Unlock()
	})
}
