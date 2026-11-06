package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

var sendColumnNames = []string{"sent_at", "send_state", "send_attempts", "next_attempt_at", "last_error", "sent_install_id"}

func outboxColumns(t *testing.T, s *SharedMetricsStore) []string {
	return storeColumn(t, s, "SELECT name FROM pragma_table_info('package_outbox')")
}

// Database the store wrote before the send columns existed, three exported packages in its outbox
func legacyDB(t *testing.T) *SharedMetricsStore {
	t.Helper()
	dir := t.TempDir()
	raw := &SharedMetricsStore{DatabasePath: filepath.Join(dir, "metrics.sqlite3"), OutboxDirectory: filepath.Join(dir, "outbox")}
	storeExec(t, raw, "CREATE TABLE telemetry_state (key TEXT PRIMARY KEY, value TEXT NOT NULL)")
	storeExec(t, raw, "INSERT INTO telemetry_state(key, value) VALUES ('schema_version', '2')")
	storeExec(t, raw, `CREATE TABLE package_outbox (package_id TEXT PRIMARY KEY, period_start TEXT NOT NULL,
		period_end TEXT NOT NULL, payload_json TEXT NOT NULL, created_at TEXT NOT NULL, exported_at TEXT)`)
	storeExec(t, raw, `CREATE TABLE counter_aggregates (period_start TEXT NOT NULL, metric_name TEXT NOT NULL,
		icebath_version TEXT NOT NULL, os_family TEXT NOT NULL, architecture TEXT NOT NULL, install_method TEXT NOT NULL,
		dimensions_json TEXT NOT NULL, value INTEGER NOT NULL, packaged_value INTEGER NOT NULL,
		PRIMARY KEY (period_start, metric_name, icebath_version, os_family, architecture, install_method, dimensions_json))`)
	for i := 0; i < 3; i++ {
		payload, _ := json.Marshal(map[string]any{"package_id": fmt.Sprintf("pkg-%d", i)})
		storeExec(t, raw, `INSERT INTO package_outbox(package_id, period_start, period_end, payload_json, created_at, exported_at)
			VALUES (?, ?, ?, ?, ?, ?)`, fmt.Sprintf("pkg-%d", i), fmt.Sprintf("2026-08-2%d", i), fmt.Sprintf("2026-08-2%d", i+1),
			strings.Replace(string(payload), ":", ": ", 1), fmt.Sprintf("2026-08-2%dT00:00:00Z", i), fmt.Sprintf("2026-08-2%dT01:00:00Z", i))
	}
	return raw
}

func reopen(t *testing.T, raw *SharedMetricsStore) {
	t.Helper()
	if _, err := NewSharedMetricsStore(context.Background(), raw.DatabasePath, raw.OutboxDirectory); err != nil {
		t.Fatal(err)
	}
}

func TestSendColumnsOnAFreshDatabase(t *testing.T) {
	s := openStore(t, t.TempDir())
	storeExec(t, s, `INSERT INTO package_outbox(package_id, period_start, period_end, payload_json, created_at)
		VALUES ('p', '2026-01-01', '2026-01-02', '{}', '2026-01-01T00:00:00Z')`)
	r := rowOf(t, s, "p")
	if r.SendAttempts != 0 || r.SendState != nil || r.SentInstallID != nil {
		t.Fatalf("%+v", r)
	}
}

func TestUpgradeFromPreSendDatabase(t *testing.T) {
	t.Run("upgrade_preserves_every_row", func(t *testing.T) {
		raw := legacyDB(t)
		reopen(t, raw)
		got := storeColumn(t, raw, "SELECT package_id || '=' || payload_json FROM package_outbox ORDER BY package_id")
		want := `pkg-0={"package_id": "pkg-0"},pkg-1={"package_id": "pkg-1"},pkg-2={"package_id": "pkg-2"}`
		if strings.Join(got, ",") != want {
			t.Fatal(got)
		}
	})
	t.Run("upgrade_adds_the_send_columns", func(t *testing.T) {
		raw := legacyDB(t)
		reopen(t, raw)
		have := map[string]bool{}
		for _, c := range outboxColumns(t, raw) {
			have[c] = true
		}
		for _, c := range sendColumnNames {
			if !have[c] {
				t.Fatal(c)
			}
		}
	})
	t.Run("upgrade_does_not_move_the_schema_version", func(t *testing.T) {
		raw := legacyDB(t)
		reopen(t, raw)
		if v := storeScalar[string](t, raw, "SELECT value FROM telemetry_state WHERE key = 'schema_version'"); v != "2" {
			t.Fatal(v)
		}
	})
	t.Run("migration_is_idempotent", func(t *testing.T) {
		raw := legacyDB(t)
		for i := 0; i < 3; i++ {
			reopen(t, raw)
		}
		seen := map[string]bool{}
		for _, c := range outboxColumns(t, raw) {
			if seen[c] {
				t.Fatal("columns were added more than once", c)
			}
			seen[c] = true
		}
	})
	t.Run("queries_written_before_this_change_still_work", func(t *testing.T) {
		raw := legacyDB(t)
		reopen(t, raw)
		if rows := storeColumn(t, raw, `SELECT package_id FROM package_outbox WHERE exported_at IS NULL
			ORDER BY created_at, package_id`); len(rows) != 0 {
			t.Fatal(rows)
		}
	})
}
