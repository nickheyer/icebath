package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestSessionsDbOpenFailurePointsToRepair(t *testing.T) {
	h := sessionsTestHome(t)
	if err := os.WriteFile(filepath.Join(h, "state.db"), []byte("not a database"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := runSessionsCmd(t, h, map[string]any{"sessions_action": "list", "session_id": nil}, "")
	if code != 1 || !strings.Contains(out, "icebath sessions repair") || !strings.Contains(out, "Details: ") {
		t.Fatalf("code %d output %s", code, out)
	}
}

func TestListIncludesACompressionTipWhoseRootHasNoCreatedSource(t *testing.T) {
	h := sessionsTestHome(t)
	db := sessionsTestDB(t, h)
	ctx := context.Background()
	sessionsTestSeed(t, db, state.NewSession{ID: "chain-root", Source: "local"})
	if err := db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, "UPDATE sessions SET created_source=NULL WHERE id=?", "chain-root")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessage(ctx, "chain-root", map[string]any{"role": "user", "content": "hi"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	sessionsTestSeed(t, db, state.NewSession{ID: "delegate-run", Source: "subagent", ParentSessionID: "chain-root"})
	if got := sessionColumn(t, db, "chain-root", "created_source"); got != nil {
		t.Fatalf("root created_source %v", got)
	}
	for _, link := range [][2]string{{"chain-root", "chain-mid"}, {"chain-mid", "chain-tip"}} {
		publish := state.NewCompressionChildPublish(link[0], link[1], "local", []map[string]any{{"role": "user", "content": "hi"}})
		publish.RequireCompressionLease = false
		if err := db.PublishCompressionChild(ctx, publish); err != nil {
			t.Fatal(err)
		}
	}
	model, source := "m", "cli"
	if err := db.UpdateTokenCounts("chain-tip", state.TokenCountDelta{InputTokens: 2, OutputTokens: 1, Model: &model, APICallCount: 1, Source: &source}); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{"chain-root", "chain-mid", "chain-tip"} {
		if got := sessionColumn(t, db, sid, "created_source"); got != "cli" {
			t.Fatalf("%s created_source %v", sid, got)
		}
	}
	r, out := sessionsTestRun(t, h, map[string]any{"limit": 20, "source": "cli", "workspace": nil}, "")
	if code, err := cmdList(r, db); err != nil || code != 0 {
		t.Fatalf("list code %d %v", code, err)
	}
	if listed := out.String(); !strings.Contains(listed, "chain-tip") || strings.Contains(listed, "chain-root") {
		t.Fatalf("listing %s", listed)
	}
	found, err := gatewaycli.NewSurfaceLabelReader(db).SearchSessions(ctx, gatewaycli.SearchSessionsOptions{Sources: []string{"cli"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, row := range found {
		seen = seen || row["id"] == "chain-tip"
	}
	if !seen {
		t.Fatalf("search rows %v", found)
	}
	out.Reset()
	if code, err := cmdStats(r, db); err != nil || code != 0 {
		t.Fatalf("stats code %d %v", code, err)
	}
	if !strings.Contains(out.String(), "cli: 3 sessions") {
		t.Fatalf("stats %s", out.String())
	}
}

// Home with three ended cli sessions and one open telegram session
func flowStore(t *testing.T) string {
	t.Helper()
	h := sessionsTestHome(t)
	db := sessionsTestDB(t, h)
	for _, id := range []string{"flow-a", "flow-b", "flow-c"} {
		sessionsTestSeed(t, db, state.NewSession{ID: id, Source: "cli", Model: "vendor/model-x"}, "first words of "+id)
		if err := db.EndSession(id, "cli_close"); err != nil {
			t.Fatal(err)
		}
	}
	sessionsTestSeed(t, db, state.NewSession{ID: "flow-open", Source: "telegram"}, "still open")
	calls := []any{map[string]any{"id": "call-1", "type": "function", "function": map[string]any{"name": "read_file", "arguments": "{}"}}}
	if _, err := db.AppendMessage(context.Background(), "flow-open", map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "text", "text": "réponse"}}, "tool_calls": calls}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestSessionsRenamePinAndPinnedListing(t *testing.T) {
	h := flowStore(t)
	code, out := runSessionsCmd(t, h, map[string]any{"sessions_action": "rename", "session_id": "flow-a", "title": []string{"Kept", "chat"}}, "")
	if code != 0 || out != "Session 'flow-a' renamed to: Kept chat\n" {
		t.Fatalf("rename code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, map[string]any{"sessions_action": "rename", "session_id": "flow-b", "title": []string{" ", ""}}, "")
	if code != 1 || out != "Error: title cannot be empty or whitespace-only.\n" {
		t.Fatalf("blank rename code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, map[string]any{"sessions_action": "rename", "session_id": "flow-b", "title": []string{"a\nb"}}, "")
	if code != 1 || out != "Error: title cannot contain newlines.\n" {
		t.Fatalf("newline rename code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, map[string]any{"sessions_action": "rename", "session_id": "flow-b", "title": []string{"Kept", "chat"}}, "")
	if code != 1 || !strings.HasPrefix(out, "Error: ") {
		t.Fatalf("duplicate rename code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, map[string]any{"sessions_action": "pin", "session_ids": []string{"flow-a", "nope"}}, "")
	if code != 1 || out != "Pinned session 'flow-a'.  (Kept chat)\nNo session 'nope'. Run: icebath sessions list to find the id.\n" {
		t.Fatalf("pin code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, map[string]any{"sessions_action": "pinned", "source": nil, "json": true}, "")
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); code != 0 || err != nil || len(rows) != 1 || rows[0]["id"] != "flow-a" ||
		rows[0]["title"] != "Kept chat" || rows[0]["source"] != "cli" || rows[0]["message_count"] != 1.0 {
		t.Fatalf("pinned json code %d %v output %s", code, err, out)
	}
	if !strings.HasPrefix(out, "[\n  {\n    \"id\": \"flow-a\",\n    \"title\": \"Kept chat\",\n    \"source\": \"cli\",\n    \"last_active\": ") {
		t.Fatalf("pinned json layout %q", out)
	}
	code, out = runSessionsCmd(t, h, map[string]any{"sessions_action": "pinned", "source": nil, "json": false}, "")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if code != 0 || len(lines) != 3 || lines[0] != "Title                            Last Active   Src       ID" ||
		lines[1] != strings.Repeat("─", 100) || lines[2] != "Kept chat                        just now      cli       flow-a" {
		t.Fatalf("pinned table code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, map[string]any{"sessions_action": "delete", "session_id": "flow-a", "yes": false}, "n\n")
	if code != 0 || out != "Delete session 'flow-a' (this session is PINNED) and all its messages? [y/N] Cancelled.\n" {
		t.Fatalf("declined delete code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, map[string]any{"sessions_action": "delete", "session_id": "flow-a", "yes": false}, "")
	if code != 1 || !strings.HasSuffix(out, "\nNo answer on a non-interactive stdin; nothing was changed. Pass --yes to run it unattended.\n") {
		t.Fatalf("unattended delete code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, map[string]any{"sessions_action": "unpin", "session_ids": []string{"flow-a"}}, "")
	if code != 0 || out != "Unpinned session 'flow-a'.  (Kept chat)\n" {
		t.Fatalf("unpin code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, map[string]any{"sessions_action": "delete", "session_id": "flow-a", "yes": true}, "")
	if code != 0 || out != "Deleted session 'flow-a'.\n" {
		t.Fatalf("delete code %d output %q", code, out)
	}
}

func TestSessionsPruneArchiveAndExportFlow(t *testing.T) {
	h := flowStore(t)
	filters := func(action string, overrides map[string]any) map[string]any {
		ns := heldStoreArgs("prune", false)
		ns["sessions_action"] = action
		for k, v := range overrides {
			ns[k] = v
		}
		return ns
	}
	code, out := runSessionsCmd(t, h, filters("archive", nil), "")
	if code != 1 || !strings.HasPrefix(out, "Refusing to archive every ended session: pass at least one filter") {
		t.Fatalf("bare archive code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, filters("prune", map[string]any{"older_than": "soon"}), "")
	if code != 1 || !strings.HasPrefix(out, "Error: Invalid value for --older-than: 'soon'. Use a duration like '5h', ") {
		t.Fatalf("bad bound code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, filters("prune", nil), "")
	if code != 0 || !strings.HasPrefix(out, "No sessions match (last active before ") {
		t.Fatalf("bare prune code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, filters("archive", map[string]any{"source": "telegram", "dry_run": true}), "")
	if code != 0 || !strings.Contains(out, "Note: 1 matching session still open — skipped because archive defaults to ended sessions. Use --include-open to archive them too.\n") ||
		!strings.HasSuffix(out, "No sessions match (source 'telegram').\n") {
		t.Fatalf("open archive code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, filters("prune", map[string]any{"source": "cli", "dry_run": true, "yes": false}), "")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if code != 0 || len(lines) != 5 || !strings.HasPrefix(lines[0], "3 session(s) match (source 'cli'; oldest activity ") ||
		!strings.HasPrefix(lines[1], "  flow-a  ") || !strings.HasSuffix(lines[1], " cli        model-x                     1 msgs  ") ||
		lines[4] != "Dry run — nothing deleted." {
		t.Fatalf("prune preview code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, filters("archive", map[string]any{"title": "nothing-has-this"}), "")
	if code != 0 || out != "No sessions match (title contains 'nothing-has-this').\n" {
		t.Fatalf("empty archive code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, filters("archive", map[string]any{"source": "cli", "min_messages": 1}), "")
	if code != 0 || out != "Archived 3 session(s). They're hidden from listings but fully recoverable (nothing was deleted).\n" {
		t.Fatalf("archive code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, filters("prune", map[string]any{"source": "cli", "yes": false}), "y\n")
	if code != 0 || out != "No sessions match (source 'cli').\n" {
		t.Fatalf("prune without archived code %d output %q", code, out)
	}
	exportNS := func(overrides map[string]any) map[string]any {
		ns := filters("export", map[string]any{"format": "jsonl", "output": nil, "only": nil, "session_id": nil, "redact": false,
			"lineage": "single", "delete_after_verified": false, "upload": false, "public": false, "no_redact": false})
		for k, v := range overrides {
			ns[k] = v
		}
		return ns
	}
	code, out = runSessionsCmd(t, h, exportNS(nil), "")
	if code != 1 || out != "JSONL export requires an output path (use - for stdout).\n" {
		t.Fatalf("export without output code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, exportNS(map[string]any{"format": "html", "output": "-"}), "")
	if code != 1 || out != "HTML export requires an output file path.\n" {
		t.Fatalf("html to stdout code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, exportNS(map[string]any{"output": "-", "dry_run": true}), "")
	if code != 1 || out != "--dry-run requires at least one filter.\n" {
		t.Fatalf("bare dry run code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, exportNS(map[string]any{"output": "-", "session_id": "missing-id"}), "")
	if code != 1 || out != "No session 'missing-id'. Run: icebath sessions list to find the id.\n" {
		t.Fatalf("export unknown id code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, exportNS(map[string]any{"output": "-", "source": "cli", "dry_run": true}), "")
	if code != 0 || !strings.HasPrefix(out, "Would export 3 session(s) (source 'cli').\n  flow-a  cli\n") {
		t.Fatalf("export preview code %d output %q", code, out)
	}
	outDir := filepath.Join(h, "exports") + string(os.PathSeparator)
	code, out = runSessionsCmd(t, h, exportNS(map[string]any{"output": outDir, "session_id": "flow-b"}), "")
	target := outDir + "icebath_session_flow-b.jsonl"
	if code != 0 || out != "Exported 1 session to "+target+"\n" {
		t.Fatalf("export to dir code %d output %q", code, out)
	}
	data, err := os.ReadFile(target)
	var exported map[string]any
	if err != nil || json.Unmarshal(data, &exported) != nil || exported["id"] != "flow-b" || !strings.HasSuffix(string(data), "}\n") {
		t.Fatalf("exported file %v %s", err, data)
	}
	code, out = runSessionsCmd(t, h, exportNS(map[string]any{"output": "-", "session_id": "flow-open"}), "")
	var open map[string]any
	if err := json.Unmarshal([]byte(out), &open); code != 0 || err != nil || !strings.Contains(out, "réponse") {
		t.Fatalf("export with structured content code %d %v output %q", code, err, out)
	}
	messages := open["messages"].([]any)
	last := messages[len(messages)-1].(map[string]any)
	part, _ := last["content"].([]any)[0].(map[string]any)
	call, _ := last["tool_calls"].([]any)[0].(map[string]any)
	if part["text"] != "réponse" || call["id"] != "call-1" || call["function"].(map[string]any)["name"] != "read_file" {
		t.Fatalf("structured content lost its shape: %v", last)
	}
	code, out = runSessionsCmd(t, h, exportNS(map[string]any{"format": "md", "output": "-"}), "")
	if code != 1 || out != "Markdown/QMD export writes files; stdout (-) is only supported with --format jsonl.\n" {
		t.Fatalf("markdown to stdout code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, exportNS(map[string]any{"format": "md", "delete_after_verified": true, "yes": false}), "")
	if code != 1 || out != "--delete-after-verified requires --yes.\n" {
		t.Fatalf("delete after verified code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, exportNS(map[string]any{"format": "md"}), "")
	if code != 1 || !strings.HasPrefix(out, "Refusing bulk export without a filter.") {
		t.Fatalf("bulk markdown code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, map[string]any{"sessions_action": "stats"}, "")
	if code != 0 || !strings.HasPrefix(out, "Total sessions: 1\nTotal messages: 5\n  telegram: 1 sessions\n") ||
		!strings.Contains(out, "\nDatabase size: ") {
		t.Fatalf("stats code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, map[string]any{"sessions_action": nil}, "")
	if code != 0 || out != "usage: icebath sessions\n" {
		t.Fatalf("bare sessions code %d output %q", code, out)
	}
}

func TestSessionsMaintenanceFlow(t *testing.T) {
	h := flowStore(t)
	code, out := runSessionsCmd(t, h, map[string]any{"sessions_action": "optimize", "force": false}, "")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if code != 0 || len(lines) != 3 || lines[0] != "Optimizing session store (FTS merge + VACUUM)…" ||
		!strings.HasPrefix(lines[1], "Optimized ") || !strings.HasSuffix(lines[1], " FTS index(es).") || !strings.HasPrefix(lines[2], "Database size: ") {
		t.Fatalf("optimize code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, map[string]any{"sessions_action": "optimize-storage", "force": false, "yes": true, "no_vacuum": false}, "")
	if code != 0 || out != "Search index is already on the compact layout — nothing to do.\n" {
		t.Fatalf("optimize-storage code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, map[string]any{"sessions_action": "clean-markers", "dry_run": true, "no_backup": false}, "")
	if code != 0 || out != "Dry run — scanning for stale tool-call marker rows (#78148)…\n✓ No affected rows found — nothing to clean.\n" {
		t.Fatalf("clean-markers code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, map[string]any{"sessions_action": "repair-routing", "apply": false, "max_gap_seconds": nil}, "")
	if code != 0 || !strings.HasPrefix(out, "flow-a  (cli, 1 messages)\n  ✗ not repairable — ") ||
		!strings.HasSuffix(out, "\n\n4 orphaned session(s) found, none unambiguously repairable. Nothing to do.\n") {
		t.Fatalf("repair-routing code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, map[string]any{"sessions_action": "repair-prompts", "apply": false, "json": false, "session_id": nil}, "")
	if code != 0 || out != "No degraded stored system prompts found.\n" {
		t.Fatalf("repair-prompts code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, map[string]any{"sessions_action": "refresh-prompts", "session": nil, "json": true}, "")
	if code != 0 || out != "{\"epoch\": 1, \"session_id\": null, \"sessions\": 0}\n" {
		t.Fatalf("refresh-prompts json code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, map[string]any{"sessions_action": "refresh-prompts", "session": "flow-b", "json": false}, "")
	if code != 0 || !strings.HasPrefix(out, "Requested a system prompt refresh for flow-b (0 session(s) with a stored prompt).\n") {
		t.Fatalf("refresh-prompts code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, map[string]any{"sessions_action": "refresh-prompts", "session": "no-such", "json": false}, "")
	if code != 1 || out != "No session matches 'no-such'.\n" {
		t.Fatalf("refresh-prompts unknown code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, heldStoreArgsNeverActive(), "")
	if code != 0 || out != "No never-active keyed sessions older than 30 day(s).\n" {
		t.Fatalf("never-active code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, map[string]any{"sessions_action": "retitle-skills", "apply": false, "limit": 200}, "")
	if code != 0 || out != "No sessions were titled from a /skill invocation.\n" {
		t.Fatalf("retitle-skills code %d output %q", code, out)
	}
}

// Namespace of prune --never-active with every filter unset
func heldStoreArgsNeverActive() map[string]any {
	ns := heldStoreArgs("prune", false)
	ns["never_active"] = true
	return ns
}

func TestSessionsExportCLIPromptOnlyStdout(t *testing.T) {
	h := sessionsTestHome(t)
	path := filepath.Join(h, "state.db")
	db := sessionsTestDB(t, h)
	sessionsTestSeed(t, db, state.NewSession{ID: "sess-123", Source: "cli"})
	for _, message := range [][2]any{{"system", "hidden system context"}, {"user", "Why is login broken?"},
		{"assistant", "I will inspect the auth middleware."}, {"tool", "def redirect_after_login(): pass"},
		{"user", []any{map[string]any{"type": "text", "text": "Only show me the prompts."}}}} {
		if _, err := db.AppendMessage(context.Background(), "sess-123", map[string]any{"role": message[0], "content": message[1]}, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ns := heldStoreArgs("prune", false)
	for k, v := range map[string]any{"sessions_action": "export", "output": "-", "session_id": "sess", "only": "user-prompts", "format": "jsonl",
		"redact": false, "lineage": "single", "delete_after_verified": false, "upload": false, "public": false, "no_redact": false} {
		ns[k] = v
	}
	code, output := runSessionsCmd(t, h, ns, "")
	if code != 0 {
		t.Fatalf("export code %d output %s", code, output)
	}
	var texts []string
	for _, line := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		texts = append(texts, record["text"].(string))
	}
	if len(texts) != 2 || texts[0] != "Why is login broken?" || texts[1] != "Only show me the prompts." {
		t.Fatalf("prompt texts %v", texts)
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(before) {
		t.Fatalf("the export changed state.db: %v", err)
	}
}

func TestSessionsMarkdownExportAndVerifiedDelete(t *testing.T) {
	h := flowStore(t)
	outDir := filepath.Join(h, "md-out")
	exportNS := func(overrides map[string]any) map[string]any {
		ns := heldStoreArgs("prune", false)
		for k, v := range map[string]any{"sessions_action": "export", "format": "md", "output": outDir, "only": nil, "session_id": nil, "redact": false,
			"lineage": "single", "delete_after_verified": false, "upload": false, "public": false, "no_redact": false, "yes": false} {
			ns[k] = v
		}
		for k, v := range overrides {
			ns[k] = v
		}
		return ns
	}
	target := filepath.Join(outDir, "flow-a-session.md")
	code, out := runSessionsCmd(t, h, exportNS(map[string]any{"session_id": "flow-a"}), "")
	if code != 0 || out != "Exported 1 session (1 message) to "+target+"\n" {
		t.Fatalf("single export code %d output %q", code, out)
	}
	text, err := os.ReadFile(target)
	if err != nil || !strings.Contains(string(text), "\n### User — ") || !strings.Contains(string(text), "\n\nfirst words of flow-a\n") ||
		!strings.Contains(string(text), "\n- Exported messages: `1`\n") {
		t.Fatalf("export file %v %s", err, text)
	}
	code, out = runSessionsCmd(t, h, exportNS(map[string]any{"session_id": "flow-a"}), "")
	if code != 1 || out != "Export already exists: "+target+". Pass --force to overwrite.\n" {
		t.Fatalf("repeat export code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, exportNS(map[string]any{"source": "cli"}), "")
	if code != 0 || out != "Skipping existing export: "+target+". Pass --force to overwrite.\nExported 2 session(s) to "+outDir+"\n" {
		t.Fatalf("bulk export code %d output %q", code, out)
	}
	manifest, err := os.ReadFile(filepath.Join(outDir, "manifest.jsonl"))
	if err != nil || strings.Count(string(manifest), "\n") != 3 {
		t.Fatalf("manifest %v %s", err, manifest)
	}
	code, out = runSessionsCmd(t, h, exportNS(map[string]any{"session_id": "flow-a", "delete_after_verified": true, "yes": true, "force": true}), "")
	if code != 0 || out != "Exported 1 session (1 message) to "+target+"\nDeleted exported session 'flow-a'.\n" {
		t.Fatalf("verified delete code %d output %q", code, out)
	}
	code, out = runSessionsCmd(t, h, exportNS(map[string]any{"session_id": "flow-a"}), "")
	if code != 1 || out != "No session 'flow-a'. Run: icebath sessions list to find the id.\n" {
		t.Fatalf("export after delete code %d output %q", code, out)
	}
}
