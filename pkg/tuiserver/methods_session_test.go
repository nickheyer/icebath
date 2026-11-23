package tuiserver

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuihost"
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

// Runtime ids of the live sessions a case added
func liveSessionIDs() []string {
	var out []string
	for _, live := range tuihost.LiveSessions() {
		out = append(out, live.SID)
	}
	return out
}

func rpc(t *testing.T, method string, params map[string]any) map[string]any {
	t.Helper()
	return handleRequest(context.Background(), map[string]any{"id": "r1", "method": method, "params": params})
}

// Create response of the guard fixture, cols defaulted to eighty
func guardCreate(params map[string]any) map[string]any {
	withCols := map[string]any{"cols": 80}
	for k, v := range params {
		withCols[k] = v
	}
	return lookupMethod("session.create")(context.Background(), "r1", withCols)
}
