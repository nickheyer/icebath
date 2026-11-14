package state

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRecentRouteTieOnLastSeenPrefersLaterRow(t *testing.T) {
	db := mustSessionDB(t, filepath.Join(t.TempDir(), "state.db"), false)
	t.Cleanup(func() { db.Close() })
	if err := db.CreateSession(NewSession{ID: "s1", Source: "tui", Model: "alpha"}); err != nil {
		t.Fatal(err)
	}
	err := db.executeWrite(context.Background(), func(conn SQLiteConn) error {
		for _, model := range []string{"alpha", "zeta"} {
			if _, err := conn.ExecContext(context.Background(), "INSERT INTO session_model_usage (session_id, model, billing_provider, task,"+
				" api_call_count, last_seen) VALUES ('s1', ?, 'p', '', 1, 100.0)", model); err != nil {
				return err
			}
		}
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	route, err := db.GetRecentSessionModelRoute(context.Background(), "s1")
	if err != nil || route == nil {
		t.Fatalf("route %v err %v", route, err)
	}
	if model, _ := route.Get("model"); model != "zeta" {
		t.Fatalf("model %v", model)
	}
}
