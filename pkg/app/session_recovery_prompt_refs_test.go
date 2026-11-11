package app

import (
	"context"
	"path/filepath"
	"testing"
)

func TestPartialRecoveryClearsOnlyUnreadableSystemPromptRefs(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	tmp := t.TempDir()
	source := filepath.Join(tmp, "corrupt-system-prompts.db")
	output := filepath.Join(tmp, "partial-system-prompts.db")
	const sessionCount = 180
	recoveryMakeManySessionsSource(t, source, sessionCount)
	ctx := context.Background()
	var promptRoot int64
	func() {
		conn := recoveryTestConn(t, source)
		if err := conn.QueryRowContext(ctx, "SELECT rootpage FROM sqlite_master WHERE type = 'table' AND name = 'system_prompts'").Scan(&promptRoot); err != nil {
			t.Fatal(err)
		}
		conn.Close()
	}()
	recoveryCorruptMiddleTableLeaf(t, source, promptRoot, true)
	report, err := recoverSessionDatabase(ctx, source, output, tmp, 8, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if recoveryAt(t, report, "verified") != true || recoveryAt(t, report, "partial") != true ||
		recoveryAt(t, report, "copy", "sessions", "status") != "complete" || recoveryAt(t, report, "copy", "messages", "status") != "complete" ||
		recoveryAt(t, report, "copy", "system_prompts", "status") != "partial" {
		t.Fatalf("report %v", report.Vals)
	}
	cleared, _ := recoveryAt(t, report, "orphan_cleanup", "session_prompt_refs_cleared").(int64)
	if cleared <= 0 || cleared >= sessionCount {
		t.Fatalf("cleared %d", cleared)
	}
	if fk, _ := recoveryAt(t, report, "verification", "foreign_key_check").([]any); len(fk) != 0 {
		t.Fatalf("foreign key check %v", fk)
	}
	conn := recoveryTestConn(t, output)
	var integrity string
	if err := conn.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity %q %v", integrity, err)
	}
	rows, err := conn.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Fatal("foreign key violations in the output")
	}
	rows.Close()
	count := func(q string) int64 {
		var n int64
		if err := conn.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	retained := count("SELECT COUNT(*) FROM sessions WHERE system_prompt_hash IS NOT NULL")
	if count("SELECT COUNT(*) FROM sessions") != sessionCount || retained != sessionCount-cleared || count("SELECT COUNT(*) FROM system_prompts") != retained {
		t.Fatalf("retained %d cleared %d", retained, cleared)
	}
}
