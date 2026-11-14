package app

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestStoreCreatedAtRuntimeSchemaMapsSourceByName(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	tmp := t.TempDir()
	output := filepath.Join(tmp, "mapped.db")
	lfFreshStore(t, output)
	dest := recoveryTestConn(t, output)
	lf := recoveryTestConn(t, filepath.Join(tmp, "lost_and_found.db"))
	columns := lfSchemaColumns(t, dest, "sessions")
	if !slices.Equal(columns[:4], []string{"id", "runtime_revision", "runtime_generation", "source"}) {
		t.Fatalf("declared order %v", columns[:4])
	}
	row := map[string]any{"id": "20260909_090909_fff006", "runtime_revision": int64(0), "runtime_generation": int64(0),
		"source": "telegram", "started_at": 1_754_000_000.0, "title": "fresh runtime store"}
	cells := make([]any, len(columns))
	for i, c := range columns {
		cells[i] = row[c]
	}
	if kind := classifyLostAndFoundRow(int64(len(cells)), cells); kind != "sessions" {
		t.Fatalf("classified %q", kind)
	}
	names := make([]string, len(cells))
	for i := range names {
		names[i] = fmt.Sprintf("c%d", i)
	}
	recoveryTestExec(t, lf, "CREATE TABLE lost_and_found (rootpgno INTEGER, pgno INTEGER, nfield INTEGER, id INTEGER, "+strings.Join(names, ", ")+")")
	args := append([]any{2, 5, len(cells), 1}, cells...)
	recoveryTestExec(t, lf, "INSERT INTO lost_and_found VALUES ("+strings.TrimSuffix(strings.Repeat("?, ", 4+len(cells)), ", ")+")", args...)
	recoveryTestExec(t, dest, "PRAGMA foreign_keys=OFF")
	report, err := mapLostAndFoundRows(context.Background(), lf, dest)
	if err != nil {
		t.Fatal(err)
	}
	mapped := report.GetOr("mapped", nil).(*pyjson.Object)
	if !pyjson.Equal(mapped.GetOr("sessions", nil), 1) || !pyjson.Equal(report.GetOr("unrecognized_layout_rows", nil), 0) {
		t.Fatalf("report %v", report.Vals)
	}
	var source, title string
	if err := dest.QueryRowContext(context.Background(), "SELECT source, title FROM sessions WHERE id = ?", row["id"]).Scan(&source, &title); err != nil ||
		source != "telegram" || title != "fresh runtime store" {
		t.Fatalf("row %q %q %v", source, title, err)
	}
}
