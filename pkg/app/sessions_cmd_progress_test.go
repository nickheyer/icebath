package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestOptimizeProgressLineFormatsEachPhase(t *testing.T) {
	cases := []struct {
		info *pyjson.Object
		want string
	}{
		{pyjson.ObjectOf("phase", "backfill", "percent", 5, "indexed", 1234, "total", 56789), "  Rebuilding index:   5% (1,234/56,789 messages)"},
		{pyjson.ObjectOf("phase", "backfill_cjk"), "  Building CJK index:   0% (0/0 messages)"},
		{pyjson.ObjectOf("phase", "teardown", "table", "fts_v22_trash_messages_fts", "table_index", 1, "table_count", 2, "percent", 100,
			"indexed", 10, "total", 10, "paced", true),
			"  Reclaiming old index: table 1/2 (messages_fts) 100% (10/10 rows) — paced: another process is using the database"},
		{pyjson.ObjectOf("phase", "vacuum", "elapsed", 125.7, "size_bytes", 3*(1<<20), "estimated_bytes", 1572864),
			"  Compacting database (VACUUM): 2m 05s elapsed, 3.0 MB → ~1.5 MB"},
		{pyjson.ObjectOf("phase", "vacuum", "elapsed", 5, "size_bytes", 2147483648), "  Compacting database (VACUUM): 0m 05s elapsed, 2,048.0 MB → ~0.0 MB"},
	}
	for _, c := range cases {
		if got := optimizeProgressLine(c.info); got != c.want {
			t.Fatalf("line %q, want %q", got, c.want)
		}
	}
}

func TestOptimizeProgressOverwritesOneLinePerPhase(t *testing.T) {
	var out strings.Builder
	p := &optimizeProgress{w: &out}
	p.call(pyjson.ObjectOf("phase", "backfill", "percent", 0, "indexed", 0, "total", 0))
	p.call(pyjson.ObjectOf("phase", "backfill", "percent", 50, "indexed", 5000, "total", 10000))
	p.call(pyjson.ObjectOf("phase", "backfill", "percent", 50, "indexed", 5000, "total", 10000))
	p.call(pyjson.ObjectOf("phase", "backfill", "percent", 99, "indexed", 9, "total", 10000))
	p.call(pyjson.ObjectOf("phase", "vacuum", "elapsed", 1, "size_bytes", 1048576, "estimated_bytes", 524288))
	p.call(pyjson.ObjectOf("phase", "done"))
	p.close(false)
	want := "\r  Rebuilding index:  50% (5,000/10,000 messages)" +
		"\r  Rebuilding index:  99% (9/10,000 messages)    " +
		"\r  Rebuilding index: 100% (10,000/10,000 messages)\n" +
		"\r  Compacting database (VACUUM): 0m 01s elapsed, 1.0 MB → ~0.5 MB\n"
	if out.String() != want {
		t.Fatalf("progress %q, want %q", out.String(), want)
	}
}

func TestRecoveryProgressDrawsOneLinePerTable(t *testing.T) {
	r, out := sessionsTestRun(t, sessionsTestHome(t), map[string]any{}, "")
	p := &recoveryProgress{r: r}
	p.call(pyjson.ObjectOf("table", "sessions", "copied_rows", 1500, "source_rows", 2500))
	p.call(pyjson.ObjectOf("table", "sessions", "copied_rows", 2500, "source_rows", 2500))
	p.call(pyjson.ObjectOf("table", "messages", "copied_rows", nil))
	p.finish()
	want := "  sessions: \r  sessions: 1,500/2,500\r  sessions: 2,500/2,500\n  messages: \r  messages: 0\n"
	if out.String() != want {
		t.Fatalf("progress %q, want %q", out.String(), want)
	}
}

func TestPrintRecoveryVerdictReadsTheReport(t *testing.T) {
	counts := pyjson.ObjectOf("verification", pyjson.ObjectOf("table_counts", pyjson.ObjectOf("sessions", 1234, "messages", 56789)))
	partial := pyjson.ObjectOf("verified", true, "verification", rowGet(counts, "verification"))
	bestEffort := pyjson.ObjectOf("verified", true, "best_effort", true)
	cases := []struct {
		report       *pyjson.Object
		allowPartial bool
		code         int
		want         string
	}{
		{pyjson.ObjectOf("complete", true), false, 0, "✓ Recovered database verified at: out.db\n  The active session database was not changed.\n" +
			"  Review the JSON report before installing this database.\n"},
		{partial, true, 0, "✓ Partial recovery output verified at: out.db\n  Recovered 1,234 sessions and 56,789 messages.\n" +
			"  The active session database was not changed.\n" +
			"  This output is incomplete. Review every skipped range and orphan count in the JSON report before installing it.\n"},
		{bestEffort, true, 0, "✓ BEST-EFFORT page-level salvage verified at: out.db\n" +
			"  The source table schemas were unreadable; rows were rebuilt from raw pages via sqlite3 .recover and mapped heuristically.\n" +
			"  Recovered 0 sessions and 0 messages.\n  The active session database was not changed.\n" +
			"  This output is incomplete. Review every skipped range and orphan count in the JSON report before installing it.\n"},
		{partial, false, 1, "✗ Recovery output did not pass every verification check.\n" +
			"  Do not install it. Review the JSON report for partial data or errors.\n"},
	}
	for _, c := range cases {
		r, out := sessionsTestRun(t, sessionsTestHome(t), map[string]any{}, "")
		if code := printRecoveryVerdict(r, c.report, "out.db", c.allowPartial); code != c.code || out.String() != c.want {
			t.Fatalf("code %d output %q, want %d %q", code, out.String(), c.code, c.want)
		}
	}
}

func TestRecoverRejectsUnusableFlagCombinations(t *testing.T) {
	h := sessionsTestHome(t)
	existing := filepath.Join(h, "rebuilt.db.recovery.json")
	if err := os.WriteFile(existing, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := func(overrides map[string]any) map[string]any {
		ns := map[string]any{"sessions_action": "recover", "source": filepath.Join(h, "state.db"), "output": nil, "inspect_only": false,
			"work_dir": nil, "chunk_size": 1000, "allow_partial": false, "report": nil}
		for k, v := range overrides {
			ns[k] = v
		}
		return ns
	}
	cases := []struct {
		ns   map[string]any
		want string
	}{
		{base(map[string]any{"inspect_only": true, "output": "out.db"}), "Error: --output cannot be used with --inspect-only.\n"},
		{base(map[string]any{"inspect_only": true, "allow_partial": true}), "Error: --allow-partial cannot be used with --inspect-only.\n"},
		{base(nil), "Error: --output is required unless --inspect-only is used.\n"},
		{base(map[string]any{"output": filepath.Join(h, "rebuilt.db")}), "Error: refusing to overwrite existing report: " + existing + "\n"},
	}
	for _, c := range cases {
		if code, out := runSessionsCmd(t, h, c.ns, ""); code != 2 || out != c.want {
			t.Fatalf("code %d output %q, want 2 %q", code, out, c.want)
		}
	}
}

func TestConfirmOrExitCodeSeparatesDeclineFromNoAnswer(t *testing.T) {
	cases := []struct {
		stdin, unattended string
		code              int
		refused           bool
		want              string
	}{
		{" YES \n", "--yes", 0, false, "Go? [y/N] "},
		{"y", "--yes", 0, false, "Go? [y/N] "},
		{"nope\n", "--yes", 0, true, "Go? [y/N] Declined.\n"},
		{"", "--yes", 1, true, "Go? [y/N] \nNo answer on a non-interactive stdin; nothing was changed. Pass --yes to run it unattended.\n"},
		{"", "", 1, true, "Go? [y/N] \nNo answer on a non-interactive stdin; nothing was changed. Run it from an interactive terminal.\n"},
	}
	for _, c := range cases {
		r, out := sessionsTestRun(t, sessionsTestHome(t), map[string]any{}, c.stdin)
		code, refused, err := confirmOrExitCode(r, "Go? [y/N] ", "Declined.", c.unattended)
		if err != nil || code != c.code || refused != c.refused || out.String() != c.want {
			t.Fatalf("stdin %q gave %d %v %v %q, want %d %v %q", c.stdin, code, refused, err, out.String(), c.code, c.refused, c.want)
		}
	}
}

func TestListSurvivesCorruptTimestampRows(t *testing.T) {
	h := sessionsTestHome(t)
	db := sessionsTestDB(t, h)
	ctx := context.Background()
	for _, sid := range []string{"good", "bad-text", "bad-huge"} {
		sessionsTestSeed(t, db, state.NewSession{ID: sid, Source: "cli"}, "hello from "+sid)
	}
	if err := db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		for _, stmt := range []string{
			"UPDATE sessions SET started_at='not-a-timestamp', last_activity_at='not-a-timestamp' WHERE id='bad-text'",
			"UPDATE messages SET timestamp='not-a-timestamp' WHERE session_id='bad-text'",
			"UPDATE sessions SET started_at=8.4e252 WHERE id='bad-huge'",
			"UPDATE messages SET timestamp=1e30 WHERE session_id='bad-huge'",
		} {
			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r, out := sessionsTestRun(t, h, map[string]any{"limit": 20, "source": nil, "all": false, "workspace": nil}, "")
	if code, err := cmdList(r, db); err != nil || code != 0 {
		t.Fatalf("list code %d %v", code, err)
	}
	listing := out.String()
	for _, sid := range []string{"good", "bad-text", "bad-huge"} {
		if !strings.Contains(listing, sid) {
			t.Fatalf("listing lost %s: %s", sid, listing)
		}
	}
	if !strings.Contains(listing, "?") {
		t.Fatalf("a corrupt timestamp did not degrade to a question mark: %s", listing)
	}
}

func TestPruneDeletesAfterConfirmation(t *testing.T) {
	h := flowStore(t)
	ns := heldStoreArgs("prune", false)
	ns["source"], ns["yes"] = "cli", false
	code, out := runSessionsCmd(t, h, ns, "y\n")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	last := lines[len(lines)-1]
	if code != 0 || len(lines) != 5 || !strings.HasPrefix(last, "Delete these 3 session(s) (oldest activity ") ||
		!strings.HasSuffix(last, ")? [y/N] Pruned 3 session(s).") {
		t.Fatalf("prune code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, map[string]any{"sessions_action": "stats"}, "")
	if code != 0 || !strings.HasPrefix(out, "Total sessions: 1\nTotal messages: 2\n") {
		t.Fatalf("stats after prune code %d output %q", code, out)
	}
}
