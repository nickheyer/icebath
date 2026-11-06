package diskcleanup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/plugins"
)

var callIDs atomic.Int64

// Runs pre_tool_call, the side effect and post_tool_call with the same identity fields as the dispatcher
func runTool(t *testing.T, h *hooks, tool string, args map[string]any, create func(), ids map[string]any) {
	t.Helper()
	kw := map[string]any{"tool_name": tool, "args": args, "tool_call_id": fmt.Sprintf("call-%d", callIDs.Add(1))}
	for k, v := range ids {
		kw[k] = v
	}
	if _, err := h.onPreToolCall(bg, kw); err != nil {
		t.Fatal(err)
	}
	if create != nil {
		create()
	}
	kw["result"] = "OK"
	if _, err := h.onPostToolCall(bg, kw); err != nil {
		t.Fatal(err)
	}
}

func endSession(t *testing.T, h *hooks, sid string) {
	t.Helper()
	if _, err := h.onSessionEnd(bg, map[string]any{"session_id": sid, "completed": true, "interrupted": false}); err != nil {
		t.Fatal(err)
	}
}

func ids(task, session string) map[string]any {
	return map[string]any{"task_id": task, "session_id": session}
}

// Tracked entries decoded from tracked.json
func trackedJSON(t *testing.T, h string) []any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h, "disk-cleanup", "tracked.json"))
	if err != nil {
		t.Fatal(err)
	}
	v, err := pyjson.Loads(string(data))
	if err != nil {
		t.Fatal(err)
	}
	return v.([]any)
}

func TestSessionEndHookLeavesWorkspaceFilesAlone(t *testing.T) {
	h, hk := isolate(t), newHooks()
	keep := filepath.Join(h, "workspace", "proj", "tests", "test_parse.py")
	os.MkdirAll(filepath.Dir(keep), 0o755)
	scratch := filepath.Join(h, "tmp_scratch.py")
	for _, p := range []string{keep, scratch} {
		runTool(t, hk, "write_file", map[string]any{"path": p, "content": "x"}, func() { write(t, p, "x") }, ids("t_ws", "s_ws"))
	}
	if mustGuess(t, keep) != "" || mustGuess(t, scratch) != "test" {
		t.Fatal("classification")
	}
	endSession(t, hk, "s_ws")
	if gone(keep) || !gone(scratch) {
		t.Fatal("workspace touched or scratch kept")
	}
}

func TestKanbanTestFilesAreNeverTrackedOrDeleted(t *testing.T) {
	h, hk := isolate(t), newHooks()
	att := filepath.Join(h, "kanban", "attachments", "t1", "test_evidence.sh")
	write(t, att, "x")
	if mustGuess(t, att) != "" {
		t.Fatal("kanban classified")
	}
	mustSave(t, entry(att, "test", daysAgo(0), 1))
	newAtt := filepath.Join(filepath.Dir(att), "test_new.sh")
	runTool(t, hk, "write_file", map[string]any{"path": newAtt, "content": "x"}, func() { write(t, newAtt, "y") }, ids("t1", "s_kb"))
	scratch := filepath.Join(h, "test_scratch.py")
	runTool(t, hk, "write_file", map[string]any{"path": scratch, "content": "x"}, func() { write(t, scratch, "x") }, ids("t1", "s_kb"))
	endSession(t, hk, "s_kb")
	if gone(att) || gone(newAtt) || !gone(scratch) || len(mustLoad(t)) != 0 {
		t.Fatal("kanban files touched or scratch kept")
	}
}

func TestWriteFileTestPatternTracked(t *testing.T) {
	h, hk := isolate(t), newHooks()
	p := filepath.Join(h, "test_created.py")
	runTool(t, hk, "write_file", map[string]any{"path": p, "content": "x"}, func() { write(t, p, "x") }, ids("t1", "s1"))
	data := trackedJSON(t, h)
	if len(data) != 1 {
		t.Fatalf("%v", data)
	}
	if c, _ := data[0].(*pyjson.Object).Get("category"); c != "test" {
		t.Fatalf("category %v", c)
	}
}

func TestTerminalCommandPicksUpPaths(t *testing.T) {
	h, hk := isolate(t), newHooks()
	p := filepath.Join(h, "tmp_created.log")
	runTool(t, hk, "terminal", map[string]any{"command": "touch " + p}, func() { write(t, p, "x") }, ids("t3", "s3"))
	resolved, _ := resolvePath(p)
	found := false
	for _, i := range trackedJSON(t, h) {
		if v, _ := i.(*pyjson.Object).Get("path"); v == resolved {
			found = true
		}
	}
	if !found {
		t.Fatal("terminal path not tracked")
	}
}

func TestIgnoresUnrelatedTool(t *testing.T) {
	h, hk := isolate(t), newHooks()
	hk.onPostToolCall(bg, map[string]any{"tool_name": "read_file", "args": map[string]any{"path": filepath.Join(h, "test_x.py")}, "result": "contents", "task_id": "t4", "session_id": "s4"})
	data, err := os.ReadFile(filepath.Join(h, "disk-cleanup", "tracked.json"))
	if err == nil && strings.TrimSpace(string(data)) != "[]" {
		t.Fatalf("tracked %s", data)
	}
}

func TestPathsTheCallDidNotCreateAreNeverTracked(t *testing.T) {
	h, hk := isolate(t), newHooks()
	script := filepath.Join(h, "scripts", "test_uptime.py")
	write(t, script, "print('up')\n")
	hookDir := filepath.Join(h, "hooks", "test_notify")
	os.MkdirAll(hookDir, 0o755)
	old := time.Unix(1_700_000_000, 0)
	os.Chtimes(script, old, old)
	os.Chtimes(hookDir, old, old)
	sess := map[string]any{"session_id": "s_keep"}
	runTool(t, hk, "patch", map[string]any{"path": script, "old_string": "up", "new_string": "ok"}, func() { write(t, script, "print('ok')\n") }, sess)
	runTool(t, hk, "terminal", map[string]any{"command": "ls -la " + hookDir}, nil, sess)
	runTool(t, hk, "terminal", map[string]any{"command": "find " + h + " -name 'test_*'"}, nil, sess)
	endSession(t, hk, "s_keep")
	if gone(script) || !isDir(hookDir) {
		t.Fatal("pre-existing paths removed")
	}
}

func TestACallNeverUsesAnotherSessionsSnapshot(t *testing.T) {
	h, hk := isolate(t), newHooks()
	userFile := filepath.Join(h, "test_user.py")
	write(t, userFile, "keep")
	newFile := filepath.Join(h, "test_new.py")
	a := map[string]any{"tool_call_id": "call_0", "task_id": "sA", "session_id": "sA"}
	b := map[string]any{"tool_call_id": "call_0", "task_id": "sB", "session_id": "sB"}
	with := func(base map[string]any, kv ...any) map[string]any {
		out := map[string]any{}
		for k, v := range base {
			out[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			out[kv[i].(string)] = kv[i+1]
		}
		return out
	}
	hk.onPreToolCall(bg, with(a, "tool_name", "patch", "args", map[string]any{"path": userFile}))
	hk.onPreToolCall(bg, with(b, "tool_name", "write_file", "args", map[string]any{"path": newFile}))
	write(t, newFile, "x")
	hk.onPostToolCall(bg, with(a, "tool_name", "patch", "args", map[string]any{"path": userFile}, "result", "OK"))
	hk.onPostToolCall(bg, with(b, "tool_name", "write_file", "args", map[string]any{"path": newFile}, "result", "OK"))
	endSession(t, hk, "sA")
	if data, _ := os.ReadFile(userFile); string(data) != "keep" || !gone(newFile) {
		t.Fatal("snapshot crossed sessions")
	}
}

func TestRunsQuickWhenTestFilesTracked(t *testing.T) {
	h, hk := isolate(t), newHooks()
	p := filepath.Join(h, "test_cleanup.py")
	runTool(t, hk, "write_file", map[string]any{"path": p, "content": "x"}, func() { write(t, p, "x") }, ids("", "s1"))
	if gone(p) {
		t.Fatal("deleted early")
	}
	endSession(t, hk, "s1")
	if !gone(p) {
		t.Fatal("test file should be auto-deleted")
	}
}

func TestPreCallSnapshotsExpireAfterTheirTTL(t *testing.T) {
	isolate(t)
	hk := newHooks()
	hk.preCalls[[2]string{"o", "stale"}] = preCall{taken: time.Now().Add(-preCallTTL - time.Second), absent: map[string]bool{"/x": true}}
	hk.onPreToolCall(bg, map[string]any{"tool_name": "write_file", "args": map[string]any{"path": "/nope/test_a.py"}, "tool_call_id": "c1", "session_id": "s"})
	if _, ok := hk.preCalls[[2]string{"o", "stale"}]; ok || len(hk.preCalls) != 1 {
		t.Fatalf("snapshots %v", hk.preCalls)
	}
}

func TestExtractPathsFromTerminal(t *testing.T) {
	got := extractPathsFromTerminal(map[string]any{"command": `touch /tmp/icebath-x/test_foo.py "~/a b" rel 'C:\file' -f`})
	if !slices.Equal(got, []string{"/tmp/icebath-x/test_foo.py", "~/a b", `C:\file`}) {
		t.Fatalf("%q", got)
	}
	if extractPathsFromTerminal(map[string]any{"command": `echo "unterminated`}) != nil || extractPathsFromTerminal(map[string]any{"command": 3}) != nil {
		t.Fatal("bad command produced paths")
	}
}

func TestSlashCommandSubcommands(t *testing.T) {
	h := isolate(t)
	hk := newHooks()
	run := func(raw string) string {
		t.Helper()
		out, err := hk.handleSlash(bg, raw)
		if err != nil {
			t.Fatal(err)
		}
		return out.(string)
	}
	if run("") != helpText || run("--help") != helpText || run("bogus") != "Unknown subcommand: bogus\n\n"+helpText {
		t.Fatal("help routing")
	}
	if !strings.HasPrefix(helpText, "/disk-cleanup \u2014 ephemeral-file cleanup\n\nSubcommands:\n") {
		t.Fatal(helpText)
	}
	if run("track") != "Usage: /disk-cleanup track <path> <category>" || run("forget") != "Usage: /disk-cleanup forget <path>" {
		t.Fatal("usage")
	}
	p := filepath.Join(h, "test_slash.py")
	write(t, p, "abc")
	if got := run("track " + p + " nope"); got != "Unknown category 'nope'. Allowed: ['chrome-profile', 'cron-output', 'download', 'other', 'research', 'temp', 'test']" {
		t.Fatal(got)
	}
	if got := run("track " + p + " test"); got != "Tracked "+p+" as 'test'." {
		t.Fatal(got)
	}
	if got := run("track " + p + " test"); got != "Not tracked (already present, missing, or outside ICEBATH_HOME): "+p {
		t.Fatal(got)
	}
	status := run("status")
	if !strings.Contains(status, "Category             ") || !strings.Contains(status, "test                      1       3.0 B") || !strings.Contains(status, "   1.    3.0 B  [test]  "+p) {
		t.Fatal(status)
	}
	if got := run("dry-run"); got != "Dry-run preview (nothing deleted):\n  Auto-delete : 1 files (3.0 B)\n    [test] "+p+"\n  Needs prompt: 0 files (0.0 B)\n\n  Total potential: 3.0 B" {
		t.Fatal(got)
	}
	big := filepath.Join(h, "research.bin")
	write(t, big, "r")
	mustSave(t, append(mustLoad(t), entry(big, "research", daysAgo(40), 1))...)
	if got := run("deep"); got != "[disk-cleanup] Cleaned 1 files + 0 empty dirs, freed 3.0 B.\n\n1 item(s) need confirmation (1.0 B):\n  [research] "+big+"\n\nRun `/disk-cleanup forget <path>` to skip, or delete manually via terminal." {
		t.Fatal(got)
	}
	if got := run("forget " + big); got != "Removed 1 tracking entry for "+big+"." {
		t.Fatal(got)
	}
	if got := run("forget " + big); got != "Not found in tracking: "+big {
		t.Fatal(got)
	}
	if got := run("quick"); got != "[disk-cleanup] Cleaned 0 files + 0 empty dirs, freed 0.0 B." {
		t.Fatal(got)
	}
	if got := fmtSummary(Summary{Deleted: 2, Errors: []string{"a", "b"}}); got != "[disk-cleanup] Cleaned 2 files + 0 empty dirs, freed 0.0 B.\n  2 error(s); see cleanup.log." {
		t.Fatal(got)
	}
	if got, _ := cmdForget(bg, []string{"forget", p, p}); got != "Not found in tracking: "+p {
		t.Fatal(got)
	}
}

func TestTrackPrintsUnlessSilent(t *testing.T) {
	h := isolate(t)
	var buf bytes.Buffer
	saved := stdout
	stdout = &buf
	t.Cleanup(func() { stdout = saved })
	p := filepath.Join(h, "loud.tmp")
	write(t, p, "x")
	if ok, err := Track(bg, p, "weird", false); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if buf.String() != "Tracked: "+p+" (other, 1.0 B)\n" {
		t.Fatalf("%q", buf.String())
	}
}

// Bundled plugin rows from a fresh manager over the isolated home
func loadedRows(t *testing.T, h string) map[string]map[string]any {
	t.Helper()
	m := plugins.NewManager(h)
	m.DiscoverAndLoad(bg, true)
	rows := map[string]map[string]any{}
	for _, r := range m.ListPlugins() {
		rows[r["key"].(string)] = r
	}
	return rows
}

func TestDiskCleanupDiscoveredButNotLoadedByDefault(t *testing.T) {
	h := isolate(t)
	row, ok := loadedRows(t, h)["disk-cleanup"]
	if !ok {
		t.Fatal("disk-cleanup not discovered")
	}
	errText, _ := row["error"].(string)
	if row["source"] != "bundled" || row["enabled"] != false || !strings.Contains(errText, "not enabled") {
		t.Fatalf("%v", row)
	}
}

func TestDisabledBeatsEnabled(t *testing.T) {
	h := isolate(t)
	write(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled:\n  - disk-cleanup\n  disabled:\n  - disk-cleanup\n")
	row := loadedRows(t, h)["disk-cleanup"]
	if row["enabled"] != false || row["error"] != "listed in both plugins.enabled and plugins.disabled \u2014 disabled wins" {
		t.Fatalf("%v", row)
	}
}

func TestMemoryAndContextEngineSubdirsSkipped(t *testing.T) {
	h := isolate(t)
	write(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled:\n  - memory\n  - context_engine\n  - disk-cleanup\n")
	rows := loadedRows(t, h)
	if _, ok := rows["memory"]; ok {
		t.Fatal("memory listed")
	}
	if _, ok := rows["context_engine"]; ok {
		t.Fatal("context_engine listed")
	}
	if rows["disk-cleanup"]["enabled"] != true || rows["disk-cleanup"]["hooks"] != 3 || rows["disk-cleanup"]["commands"] != 1 {
		t.Fatalf("%v", rows["disk-cleanup"])
	}
}

func TestPluginsListShowsTheCompiledDiskCleanup(t *testing.T) {
	h := isolate(t)
	entries, err := plugins.DiscoverAllPlugins(h)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(entries, func(e plugins.PluginEntry) bool { return e.Key == "disk-cleanup" })
	if i < 0 || entries[i].Source != "bundled" || entries[i].Version != "2.0.0" || entries[i].Name != "disk-cleanup" {
		t.Fatalf("%v", entries)
	}
	var out bytes.Buffer
	if err := plugins.CmdList(bg, plugins.ListOptions{JSON: true}, &out); err != nil {
		t.Fatal(err)
	}
	var payload []map[string]any
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatal(err, out.String())
	}
	if !slices.ContainsFunc(payload, func(p map[string]any) bool { return p["name"] == "disk-cleanup" }) {
		t.Fatal(out.String())
	}
	out.Reset()
	if err := plugins.CmdList(bg, plugins.ListOptions{Plain: true, NoBundled: true}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "disk-cleanup") {
		t.Fatal(out.String())
	}
}
