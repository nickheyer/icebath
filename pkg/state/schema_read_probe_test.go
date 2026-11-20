package state

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// File store carrying SCHEMA_SQL and optionally DEFERRED_INDEX_SQL
func probeStore(t *testing.T, deferred bool) SQLiteConn {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	conn, err := ConnectSQLite(filepath.Join(t.TempDir(), "probe.db"), SQLiteConfig{Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	ctx := context.Background()
	if err := executeScript(ctx, conn, SchemaSQL); err != nil {
		t.Fatal(err)
	}
	if deferred {
		if err := executeScript(ctx, conn, DeferredIndexSQL); err != nil {
			t.Fatal(err)
		}
	}
	return conn
}

// Probe statement selecting from one table
func probeFor(t *testing.T, table string) string {
	t.Helper()
	statements, err := SchemaReadProbeStatements(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range statements {
		if strings.Contains(s, fmt.Sprintf(`FROM "%s"`, table)) {
			return s
		}
	}
	t.Fatalf("no probe statement for table %s", table)
	return ""
}

func TestProbesCoverEveryDeclaredColumn(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	expected, err := parseSchemaColumns(context.Background(), SchemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range expected.Keys {
		probe := probeFor(t, table)
		for _, col := range expected.Vals[table].(*pyjson.Object).Keys {
			if !strings.Contains(probe, fmt.Sprintf(`"%s"."%s"`, table, col)) {
				t.Fatalf("column %s.%s declared in SCHEMA_SQL but not probed", table, col)
			}
		}
	}
}

func TestProbesPassOnFreshSchema(t *testing.T) {
	conn := probeStore(t, true)
	statements, err := SchemaReadProbeStatements(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range statements {
		if _, err := fetchOne(context.Background(), conn, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func TestProbesFailOnMissingColumn(t *testing.T) {
	conn := probeStore(t, true)
	mustExecConn(t, conn, "DROP INDEX IF EXISTS idx_sessions_effective_activity")
	mustExecConn(t, conn, "ALTER TABLE sessions DROP COLUMN last_activity_at")
	_, err := fetchOne(context.Background(), conn, probeFor(t, "sessions"))
	if !isOperationalError(err) || !strings.Contains(errText(err), "no such column: sessions.last_activity_at") {
		t.Fatalf("%v", err)
	}
}

func TestProbesFailOnMissingTable(t *testing.T) {
	conn := probeStore(t, false)
	if err := executeScript(context.Background(), conn, "DROP TABLE gateway_routing"); err != nil {
		t.Fatal(err)
	}
	_, err := fetchOne(context.Background(), conn, probeFor(t, "gateway_routing"))
	if !isOperationalError(err) || !strings.Contains(strings.ToLower(errText(err)), "no such table") {
		t.Fatalf("%v", err)
	}
}
