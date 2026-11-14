package gatewaycli

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

type surfaceRow struct {
	id, source, created string
	started             float64
}

func openStore(t *testing.T, path string, readOnly bool) *state.SessionDB {
	t.Helper()
	db, err := state.NewSessionDB(context.Background(), path, readOnly)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func storeExec(t *testing.T, db *state.SessionDB, statements ...string) {
	t.Helper()
	ctx := context.Background()
	err := db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		for _, statement := range statements {
			if _, err := conn.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func populateSurfaces(t *testing.T, db *state.SessionDB, rows []surfaceRow) {
	t.Helper()
	ctx := context.Background()
	for _, row := range rows {
		if err := db.CreateSession(state.NewSession{ID: row.id, Source: row.source}); err != nil {
			t.Fatal(err)
		}
	}
	err := db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		for _, row := range rows {
			if _, err := conn.ExecContext(ctx, "UPDATE sessions SET created_source=?, started_at=?, message_count=1 WHERE id=?",
				row.created, row.started, row.id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func newerLocals(count int, start float64, prefix string) []surfaceRow {
	rows := make([]surfaceRow, count)
	for index := range rows {
		rows[index] = surfaceRow{fmt.Sprintf("%s-%03d", prefix, index), "local", []string{"desktop", "tui"}[index%2], start + float64(index)}
	}
	return rows
}

func searchIDs(t *testing.T, reader *SurfaceLabelReader, o SearchSessionsOptions) []string {
	t.Helper()
	rows, err := reader.SearchSessions(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, row := range rows {
		ids = append(ids, row["id"].(string))
	}
	return ids
}

func richIDs(t *testing.T, reader *SurfaceLabelReader, source string, limit, offset int) []string {
	t.Helper()
	rows, err := reader.ListSessionsRich(context.Background(), state.ListSessionsRichOptions{Source: source, Limit: &limit, Offset: offset})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, row := range rows {
		ids = append(ids, row.Vals["id"].(string))
	}
	return ids
}

func wantIDs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ids %v, want %v", got, want)
	}
}

func TestSurfacePagesAreExactAcrossNewerLocalRows(t *testing.T) {
	db := openStore(t, filepath.Join(t.TempDir(), "state.db"), false)
	rows := []surfaceRow{{"telegram-new", "telegram", "telegram", 2_100_000}, {"unknown-new", "local", "unknown", 2_000_000}}
	rows = append(rows, newerLocals(60, 1_900_000, "top")...)
	rows = append(rows, surfaceRow{"legacy-cli", "cli", "desktop", 1_800_000})
	rows = append(rows, newerLocals(10, 1_700_000, "gap-a")...)
	rows = append(rows, surfaceRow{"cli-a", "local", "cli", 1_600_000})
	rows = append(rows, newerLocals(10, 1_500_000, "gap-b")...)
	rows = append(rows, surfaceRow{"cli-b", "local", "cli", 1_400_000})
	rows = append(rows, newerLocals(10, 1_300_000, "gap-c")...)
	rows = append(rows, surfaceRow{"cli-c", "local", "cli", 1_200_000})
	populateSurfaces(t, db, rows)
	reader := NewSurfaceLabelReader(db)
	cli := []string{"cli"}
	wantIDs(t, searchIDs(t, reader, SearchSessionsOptions{Sources: cli, Limit: 10}), "legacy-cli", "cli-a", "cli-b", "cli-c")
	wantIDs(t, searchIDs(t, reader, SearchSessionsOptions{Sources: cli, Limit: 1, Offset: 1}), "cli-a")
	wantIDs(t, searchIDs(t, reader, SearchSessionsOptions{Sources: cli, Limit: 2, Offset: 2}), "cli-b", "cli-c")
	wantIDs(t, richIDs(t, reader, "cli", 10, 0), "legacy-cli", "cli-a", "cli-b", "cli-c")
	wantIDs(t, richIDs(t, reader, "cli", 1, 1), "cli-a")
	wantIDs(t, richIDs(t, reader, "cli", 2, 2), "cli-b", "cli-c")
	wantIDs(t, searchIDs(t, reader, SearchSessionsOptions{Sources: []string{"cli", "oneshot"}, Limit: 1}), "legacy-cli")
}

func preColumnReadonly(t *testing.T, path string) *state.SessionDB {
	t.Helper()
	writer, err := state.NewSessionDB(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.CreateSession(state.NewSession{ID: "legacy-cli", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	storeExec(t, writer, "UPDATE sessions SET message_count=1 WHERE id='legacy-cli'",
		"DROP INDEX IF EXISTS idx_sessions_local_created_started", "ALTER TABLE sessions DROP COLUMN created_source")
	writer.Close()
	return openStore(t, path, true)
}

func TestAbsentSchemaProbeSeesAColumnAddedLater(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	reader := NewSurfaceLabelReader(preColumnReadonly(t, path))
	wantIDs(t, searchIDs(t, reader, SearchSessionsOptions{Sources: []string{"cli"}, Limit: 5}), "legacy-cli")
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Exec("ALTER TABLE sessions ADD COLUMN created_source TEXT"); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Exec("INSERT INTO sessions (id, source, created_source, started_at) VALUES (?, ?, ?, ?)",
		"local-new", "local", "cli", 4_000_000_000); err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if ids := searchIDs(t, reader, SearchSessionsOptions{Sources: []string{"cli"}, Limit: 5}); len(ids) == 0 || ids[0] != "local-new" {
		t.Fatalf("ids %v", ids)
	}
	found := false
	for _, id := range richIDs(t, reader, "cli", 5, 0) {
		found = found || id == "local-new"
	}
	if !found {
		t.Fatal("rich listing misses the row another connection added")
	}
}

func TestReaderOnAReadOnlyStoreFromBeforeTheColumnAndTheRuntimeTables(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	writer, err := state.NewSessionDB(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.CreateSession(state.NewSession{ID: "legacy-cli", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.AppendMessage(ctx, "legacy-cli", map[string]any{"role": "user", "content": "hi"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	if err := writer.CreateSession(state.NewSession{ID: "legacy-stub", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	storeExec(t, writer, "UPDATE sessions SET started_at = started_at + 60 WHERE id = 'legacy-stub'",
		"DROP INDEX IF EXISTS idx_sessions_local_created_started", "ALTER TABLE sessions DROP COLUMN created_source",
		"DROP TABLE session_admissions")
	writer.Close()
	ro := openStore(t, path, true)
	reader := NewSurfaceLabelReader(ro)
	count, err := reader.SessionCount(ctx, state.SessionCountOptions{Source: "cli"})
	if err != nil || count != 2 {
		t.Fatal(count, err)
	}
	wantIDs(t, searchIDs(t, reader, SearchSessionsOptions{Sources: []string{"cli"}, Limit: 1, SkipLaunchStubs: true}), "legacy-cli")
	tables, err := ro.ReadAll(ctx, "SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range tables {
		if row.Vals["name"] == "session_admissions" {
			t.Fatal("the read only open migrated the runtime tables")
		}
	}
	columns, err := ro.ReadAll(ctx, "PRAGMA table_info(sessions)")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range columns {
		if row.Vals["name"] == "created_source" {
			t.Fatal("the read only open migrated the column")
		}
	}
}
