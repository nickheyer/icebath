package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// One long session whose messages span many b-tree pages, vacuumed into a rollback journal file
func recoveryMakePageSpanningSource(t *testing.T, path string, messageCount int) (int64, int64, bool) {
	t.Helper()
	caseHome(t)
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: "partial-recovery-session", Source: "cli", Cwd: "/tmp/partial-recovery"}); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < messageCount; n++ {
		role := "assistant"
		if n%2 == 0 {
			role = "user"
		}
		content := fmt.Sprintf("partial recovery payload %04d ", n) + strings.Repeat(string(rune(65+n%26)), 1500)
		if _, err := db.AppendMessage(ctx, "partial-recovery-session", map[string]any{"role": role, "content": content}, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	conn := recoveryTestConn(t, path)
	for _, q := range []string{"PRAGMA wal_checkpoint(TRUNCATE)", "PRAGMA journal_mode=DELETE", "VACUUM"} {
		recoveryTestExec(t, conn, q)
	}
	rows, err := conn.QueryContext(ctx, "EXPLAIN QUERY PLAN SELECT COUNT(*) FROM messages")
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	columns, _ := rows.Columns()
	for rows.Next() {
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, fmt.Sprint(values[3]))
	}
	rows.Close()
	joined := strings.Join(plan, " ")
	indexRows, err := conn.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'messages'")
	if err != nil {
		t.Fatal(err)
	}
	countIndex := ""
	for indexRows.Next() {
		var name string
		if err := indexRows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if countIndex == "" && strings.HasSuffix(joined, name) {
			countIndex = name
		}
	}
	indexRows.Close()
	root := func(name string) int64 {
		var page int64
		if err := conn.QueryRowContext(ctx, "SELECT rootpage FROM sqlite_master WHERE name = ?", name).Scan(&page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	if countIndex == "" {
		return root("messages"), 0, false
	}
	return root("messages"), root(countIndex), true
}

// Damages the cell count of the middle leaf under root, keeping the root and the leaves on both sides
func recoveryCorruptMiddleTableLeaf(t *testing.T, path string, root int64, requireInterior bool) int64 {
	t.Helper()
	pageSize, leaves := recoveryBtreeLeafPages(t, path, root)
	if len(leaves) == 0 || (requireInterior && len(leaves) < 3) {
		t.Fatalf("leaves %v", leaves)
	}
	leaf := leaves[len(leaves)/2]
	header := (leaf - 1) * pageSize
	if leaf == 1 {
		header += 100
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if data[header] != 0x0A && data[header] != 0x0D {
		t.Fatalf("leaf type %#x", data[header])
	}
	data[header+3], data[header+4] = 0xff, 0xff
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return leaf
}

func recoverySHA256(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}

func TestCliAllowPartialSalvagesRowsAcrossACorruptLeaf(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	tmp := t.TempDir()
	source := filepath.Join(tmp, "corrupt-state.db")
	rejected := filepath.Join(tmp, "rejected.db")
	output := filepath.Join(tmp, "partial-recovered.db")
	const messageCount = 320
	messagesRoot, countRoot, hasCountIndex := recoveryMakePageSpanningSource(t, source, messageCount)
	corruptPage := recoveryCorruptMiddleTableLeaf(t, source, messagesRoot, true)
	if hasCountIndex {
		recoveryCorruptMiddleTableLeaf(t, source, countRoot, false)
	}
	sourceHash := recoverySHA256(t, source)
	ctx := context.Background()
	inspection, err := inspectSessionDatabase(ctx, source, tmp)
	if err != nil {
		t.Fatal(err)
	}
	if recoveryAt(t, inspection, "recoverable") != false || recoveryAt(t, inspection, "tables", "messages", "rows") != nil {
		t.Fatalf("inspection %v", inspection.Vals)
	}
	_, err = recoverSessionDatabase(ctx, source, rejected, tmp, 1000, nil, false)
	var recErr *sessionRecoveryError
	if !errors.As(err, &recErr) || recErr.kind != "source" || !strings.Contains(recErr.Error(), "messages") {
		t.Fatalf("strict recovery error %v", err)
	}
	if _, err := os.Stat(rejected); err == nil {
		t.Fatal("rejected output was written")
	}
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "isolated-icebath-home"))
	var out bytes.Buffer
	ns := map[string]any{"sessions_action": "recover", "source": source, "output": output, "inspect_only": false, "work_dir": tmp,
		"chunk_size": 8, "allow_partial": true, "report": nil}
	code, err := CmdSessions(ctx, filepath.Join(tmp, "isolated-icebath-home"), ns, &out, func() error { return nil })
	if err != nil || code != 0 {
		t.Fatalf("exit %d %v\n%s", code, err, out.String())
	}
	if !strings.Contains(out.String(), "Partial recovery output verified") || !strings.Contains(out.String(), "active session database was not changed") {
		t.Fatalf("output %s", out.String())
	}
	if recoverySHA256(t, source) != sourceHash {
		t.Fatal("source changed")
	}
	raw, err := os.ReadFile(output + ".recovery.json")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := pyjson.Loads(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	report := decoded.(*pyjson.Object)
	for key, want := range map[string]any{"allow_partial": true, "verified": true, "complete": false, "partial": true, "installed": false, "source_unchanged": true} {
		if recoveryAt(t, report, key) != want {
			t.Fatalf("%s = %v", key, recoveryAt(t, report, key))
		}
	}
	if recoveryAt(t, report, "verification", "healthy") != true || !pyjson.Equal(recoveryAt(t, report, "verification", "integrity_check"), []any{"ok"}) ||
		!pyjson.Equal(recoveryAt(t, report, "verification", "foreign_key_check"), []any{}) ||
		!pyjson.Equal(recoveryAt(t, report, "verification", "table_counts", "sessions"), 1) {
		t.Fatalf("verification %v", recoveryAt(t, report, "verification"))
	}
	copied := recoveryAt(t, report, "copy", "messages").(*pyjson.Object)
	copiedRows, _ := copied.GetOr("copied_rows", nil).(int64)
	if copied.GetOr("status", nil) != "partial" || copiedRows >= messageCount || copiedRows <= 0 || copied.GetOr("query_limit_reached", nil) != false {
		t.Fatalf("messages copy %v", copied.Vals)
	}
	ranges, _ := copied.GetOr("skipped_rowid_ranges", nil).([]any)
	spans := slices.ContainsFunc(ranges, func(item any) bool {
		r := item.(*pyjson.Object)
		low, _ := r.GetOr("low", nil).(int64)
		high, _ := r.GetOr("high", nil).(int64)
		return low <= messageCount && high >= 1
	})
	if len(ranges) == 0 || !spans {
		t.Fatalf("skipped ranges %v", ranges)
	}
	conn := recoveryTestConn(t, output)
	rows, err := conn.QueryContext(ctx, "SELECT id FROM messages")
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
	var integrity string
	if err := conn.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity %q %v", integrity, err)
	}
	if !ids[1] || !ids[messageCount] || int64(len(ids)) != copiedRows {
		t.Fatalf("recovered %d ids, first %v last %v", len(ids), ids[1], ids[messageCount])
	}
	_, leaves := recoveryBtreeLeafPages(t, source, messagesRoot)
	if corruptPage == slices.Min(leaves) || corruptPage == slices.Max(leaves) {
		t.Fatalf("damaged leaf %d is an edge of %v", corruptPage, leaves)
	}
}
