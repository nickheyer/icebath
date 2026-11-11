package gateway

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
)

// Opens a store at a path under an isolated home
func backfillStore(t *testing.T, ctx context.Context, path string) *state.SessionDB {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := state.NewSessionDB(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// Creates a session row whose owner column is forced to the given value, NULL for nil
func legacySession(t *testing.T, ctx context.Context, db *state.SessionDB, sessionID string, profileName any) {
	t.Helper()
	if err := db.CreateSession(state.NewSession{ID: sessionID, Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if err := db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, "UPDATE sessions SET profile_name = ? WHERE id = ?", profileName, sessionID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// Owner column of every session row by id
func sessionOwners(t *testing.T, ctx context.Context, db *state.SessionDB) map[string]any {
	t.Helper()
	owners := map[string]any{}
	if err := db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		rows, err := conn.QueryContext(ctx, "SELECT id, profile_name FROM sessions ORDER BY id")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var owner any
			if err := rows.Scan(&id, &owner); err != nil {
				return err
			}
			if raw, ok := owner.([]byte); ok {
				owner = string(raw)
			}
			owners[id] = owner
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return owners
}

func TestDefaultStoreStampsOnlyUnownedRows(t *testing.T) {
	ctx, db := newTestStateDB(t)
	legacySession(t, ctx, db, "null-row", nil)
	legacySession(t, ctx, db, "blank-row", "  ")
	legacySession(t, ctx, db, "owned-row", "work")
	if stamped := backfillLegacySessionOwners(ctx, db); stamped != 2 {
		t.Fatalf("stamped %d rows, want 2", stamped)
	}
	want := map[string]any{"blank-row": "default", "null-row": "default", "owned-row": "work"}
	if owners := sessionOwners(t, ctx, db); !reflect.DeepEqual(owners, want) {
		t.Fatalf("owners %v, want %v", owners, want)
	}
	if stamped := backfillLegacySessionOwners(ctx, db); stamped != 0 {
		t.Fatalf("second pass stamped %d rows", stamped)
	}
}

func TestNamedProfileStoreStampsItsOwnName(t *testing.T) {
	ctx, _ := newTestStateDB(t)
	db := backfillStore(t, ctx, filepath.Join(home.Root(), "profiles", "work", "state.db"))
	legacySession(t, ctx, db, "null-row", nil)
	if stamped := backfillLegacySessionOwners(ctx, db); stamped != 1 {
		t.Fatalf("stamped %d rows, want 1", stamped)
	}
	if owners := sessionOwners(t, ctx, db); !reflect.DeepEqual(owners, map[string]any{"null-row": "work"}) {
		t.Fatalf("owners %v", owners)
	}
}

func TestStoreOutsideTheProfileTreeKeepsNull(t *testing.T) {
	ctx, _ := newTestStateDB(t)
	db := backfillStore(t, ctx, filepath.Join(t.TempDir(), "elsewhere", "state.db"))
	legacySession(t, ctx, db, "null-row", nil)
	if stamped := backfillLegacySessionOwners(ctx, db); stamped != 0 {
		t.Fatalf("stamped %d rows, want 0", stamped)
	}
	if owners := sessionOwners(t, ctx, db); !reflect.DeepEqual(owners, map[string]any{"null-row": nil}) {
		t.Fatalf("owners %v", owners)
	}
}
