package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Header offset of a b-tree page in the database file bytes
func lfPageHeader(t *testing.T, data []byte, page int64) int64 {
	t.Helper()
	size := int64(data[16])<<8 | int64(data[17])
	if size == 1 {
		size = 65_536
	}
	header := (page - 1) * size
	if page == 1 {
		header += 100
	}
	if data[header] != 0x0A && data[header] != 0x0D {
		t.Fatalf("page %d is no leaf (%#x)", page, data[header])
	}
	return header
}

// Cell count of a leaf page
func lfLeafCellCount(t *testing.T, path string, page int64) int64 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	header := lfPageHeader(t, data, page)
	return int64(data[header+3])<<8 | int64(data[header+4])
}

// Gives a leaf page an impossible cell count
func lfCorruptLeaf(t *testing.T, path string, page int64) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	header := lfPageHeader(t, data, page)
	data[header+3], data[header+4] = 0xff, 0xff
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExactLookupRecoversTailRowNextToDamagedHighEdge(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	tmp := t.TempDir()
	source := filepath.Join(tmp, "tail-damaged.db")
	output := filepath.Join(tmp, "tail-recovered.db")
	const messageCount = 320
	messagesRoot, countRoot, hasCountIndex := recoveryMakePageSpanningSource(t, source, messageCount)
	_, leaves := recoveryBtreeLeafPages(t, source, messagesRoot)
	if len(leaves) < 3 {
		t.Fatalf("leaves %v", leaves)
	}
	rightmost := leaves[len(leaves)-1]
	lost := lfLeafCellCount(t, source, rightmost)
	if lost <= 0 || lost >= messageCount {
		t.Fatalf("lost rows %d", lost)
	}
	boundary := messageCount - lost
	lfCorruptLeaf(t, source, rightmost)
	if hasCountIndex {
		_, indexLeaves := recoveryBtreeLeafPages(t, source, countRoot)
		lfCorruptLeaf(t, source, indexLeaves[len(indexLeaves)-1])
	}
	report, err := recoverSessionDatabase(context.Background(), source, output, tmp, 8, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	copied := recoveryAt(t, report, "copy", "messages").(*pyjson.Object)
	bounds := copied.GetOr("rowid_bounds", nil).(*pyjson.Object)
	errs, _ := bounds.GetOr("errors", nil).([]any)
	highProbe := false
	for _, e := range errs {
		highProbe = highProbe || strings.Contains(fmt.Sprint(e), "high rowid")
	}
	fallback, _ := bounds.GetOr("fallback_edges", nil).([]any)
	aggregate, _ := bounds.GetOr("aggregate_edges", []any{}).([]any)
	if !highProbe || (!lfListHas(fallback, "high") && !lfListHas(aggregate, "high")) {
		t.Fatalf("bounds %v", bounds.Vals)
	}
	conn := recoveryTestConn(t, output)
	rows, err := conn.QueryContext(context.Background(), "SELECT id FROM messages")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[id] = true
	}
	rows.Close()
	exact, _ := copied.GetOr("exact_lookup_recovered", nil).(int64)
	if !ids[1] || !ids[boundary] || exact < 1 || int64(len(ids)) != boundary {
		t.Fatalf("boundary %d recovered %d exact %d", boundary, len(ids), exact)
	}
	for id := int64(1); id <= boundary; id++ {
		if !ids[id] {
			t.Fatalf("row %d missing", id)
		}
	}
	if !pyjson.Equal(recoveryAt(t, report, "verification", "integrity_check"), []any{"ok"}) || recoveryAt(t, report, "verified") != true {
		t.Fatalf("verification %v", recoveryAt(t, report, "verification"))
	}
}

func lfListHas(list []any, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
