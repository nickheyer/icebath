package memory

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/skills"
)

var (
	curRunsMu sync.Mutex
	curRuns   [][2]string
)

// Temp home with a skills dir, default curator config and the warn once memory cleared
func curTestHome(t *testing.T, cfg map[string]any) (context.Context, string) {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(filepath.Join(h, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	prevCfg, prevLLM, prevRec := curatorConfig, curatorLLMReview, curatorRecordRun
	curRunsMu.Lock()
	curRuns = nil
	curRunsMu.Unlock()
	curatorRecordRun = func(_ context.Context, trigger, outcome string, counts func() map[string]int, _ string) {
		curRunsMu.Lock()
		curRuns = append(curRuns, [2]string{trigger, outcome})
		curRunsMu.Unlock()
		counts()
	}
	curatorConfig = func(context.Context) map[string]any {
		if cfg == nil {
			return map[string]any{}
		}
		return cfg
	}
	curatorLLMReview = func(context.Context, string) *pyjson.Object { return curatorLLMMeta("llm-stub", nil) }
	curatorWarned.Lock()
	curatorWarned.seen = map[string]bool{}
	curatorWarned.Unlock()
	t.Cleanup(func() { curatorConfig, curatorLLMReview, curatorRecordRun = prevCfg, prevLLM, prevRec })
	t.Cleanup(curatorReviewThreads.Wait)
	return context.Background(), h
}

func curWriteSkill(t *testing.T, skillsDir, name, body string) string {
	t.Helper()
	d := filepath.Join(skillsDir, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "---\nname: " + name + "\ndescription: x\n---\n"
	if body != "" {
		text = "---\nname: " + name + "\ndescription: t\nversion: 1.0\n---\n\n" + body + "\n"
	}
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}

// Agent created usage record whose activity is days old
func curBackdate(t *testing.T, ctx context.Context, name string, days, useCount int, extra map[string]any) {
	t.Helper()
	scope, _, _ := curatorScope(ctx)
	data, err := scope.LoadUsage()
	if err != nil {
		t.Fatal(err)
	}
	rec, err := scope.GetRecord(name)
	if err != nil {
		t.Fatal(err)
	}
	ts := pydatetime.ISOFormatUTC(time.Now().Add(-time.Duration(days) * 24 * time.Hour))
	rec.Set("created_by", "agent")
	rec.Set("created_at", ts)
	if useCount > 0 {
		rec.Set("last_used_at", ts)
		rec.Set("last_activity_at", ts)
	} else {
		rec.Set("last_used_at", nil)
		rec.Set("last_activity_at", nil)
	}
	rec.Set("use_count", useCount)
	for k, v := range extra {
		rec.Set(k, v)
	}
	data.Set(name, rec)
	if !scope.SaveUsage(ctx, data) {
		t.Fatal("save usage failed")
	}
}

func curRecord(t *testing.T, ctx context.Context, name string) *pyjson.Object {
	t.Helper()
	scope, _, _ := curatorScope(ctx)
	rec, err := scope.GetRecord(name)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestCuratorFirstRunDefers(t *testing.T) {
	ctx, _ := curTestHome(t, nil)
	if run, err := CuratorShouldRunNow(ctx, time.Time{}); err != nil || run {
		t.Fatalf("first observation ran: %v %v", run, err)
	}
	st, _ := LoadCuratorState(ctx)
	if v, _ := st.Get("last_run_at"); v == nil {
		t.Fatal("first observation must seed last_run_at")
	}
	if run, _ := CuratorShouldRunNow(ctx, time.Time{}); run {
		t.Fatal("seeded clock must not fire immediately")
	}
}

func TestCuratorSetPausedRoundtrip(t *testing.T) {
	ctx, _ := curTestHome(t, nil)
	for _, want := range []bool{true, false} {
		if err := SetCuratorPaused(ctx, want); err != nil {
			t.Fatal(err)
		}
		if got, _ := CuratorIsPaused(ctx); got != want {
			t.Fatalf("paused %v want %v", got, want)
		}
	}
}

func TestCuratorNonPositiveBoundsFallBackToDefaults(t *testing.T) {
	for _, bad := range []int{0, -5} {
		ctx, h := curTestHome(t, map[string]any{"archive_after_days": bad, "stale_after_days": bad})
		curWriteSkill(t, filepath.Join(h, "skills"), "just-used", "")
		curBackdate(t, ctx, "just-used", 0, 1, nil)
		counts, err := ApplyCuratorTransitions(ctx, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if curatorCount(counts, "archived") != 0 || curRecord(t, ctx, "just-used").Vals["state"] != skills.StateActive {
			t.Fatalf("bad value %d archived a fresh skill", bad)
		}
		if CuratorArchiveAfterDays(ctx) != curatorDefaultArchiveAfterDays || CuratorStaleAfterDays(ctx) != curatorDefaultStaleAfterDays {
			t.Fatal("non positive day counts must fall back to the defaults")
		}
	}
	for _, bad := range []int{0, -3} {
		ctx, _ := curTestHome(t, map[string]any{"interval_hours": bad})
		now := time.Now().UTC()
		SaveCuratorState(ctx, curatorEntry("last_run_at", pydatetime.ISOFormatUTC(now.Add(-time.Minute))))
		if CuratorIntervalHours(ctx) != curatorDefaultIntervalHours {
			t.Fatal("interval must fall back to the default")
		}
		if run, _ := CuratorShouldRunNow(ctx, now); run {
			t.Fatal("non positive interval must not run on every tick")
		}
	}
}

func TestCuratorBadBoundedValueWarnsOncePerDistinctValue(t *testing.T) {
	cfg := map[string]any{"archive_after_days": 0}
	ctx, _ := curTestHome(t, cfg)
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)
	for range 3 {
		CuratorArchiveAfterDays(ctx)
	}
	cfg["archive_after_days"] = -1
	CuratorArchiveAfterDays(ctx)
	CuratorArchiveAfterDays(ctx)
	var msgs []string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, "archive_after_days") {
			msgs = append(msgs, l)
		}
	}
	if len(msgs) != 2 || !strings.Contains(msgs[0], "got 0") || !strings.Contains(msgs[1], "got -1") {
		t.Fatalf("warnings %q", msgs)
	}
}

func TestCuratorPinnedSkillIsNeverTouched(t *testing.T) {
	ctx, h := curTestHome(t, nil)
	curWriteSkill(t, filepath.Join(h, "skills"), "precious", "")
	curBackdate(t, ctx, "precious", 365, 1, map[string]any{"pinned": true, "last_activity_at": nil})
	counts, err := ApplyCuratorTransitions(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	rec := curRecord(t, ctx, "precious")
	if curatorCount(counts, "archived") != 0 || curatorCount(counts, "marked_stale") != 0 || rec.Vals["state"] != "active" || rec.Vals["pinned"] != true {
		t.Fatalf("pinned skill touched: %v %v", counts.Vals, rec.Vals)
	}
}

func TestCuratorStaleSkillArchivedAndFreshKept(t *testing.T) {
	ctx, h := curTestHome(t, nil)
	curWriteSkill(t, filepath.Join(h, "skills"), "orphan", "")
	curWriteSkill(t, filepath.Join(h, "skills"), "recent", "")
	curBackdate(t, ctx, "orphan", 200, 1, nil)
	curBackdate(t, ctx, "recent", 1, 1, nil)
	if _, err := ApplyCuratorTransitions(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if curRecord(t, ctx, "orphan").Vals["state"] != skills.StateArchived || curRecord(t, ctx, "recent").Vals["state"] != skills.StateActive {
		t.Fatal("inactivity transitions wrong")
	}
}

func TestCuratorRunReviewRecordsState(t *testing.T) {
	ctx, h := curTestHome(t, nil)
	curWriteSkill(t, filepath.Join(h, "skills"), "a", "")
	scope, _, _ := curatorScope(ctx)
	scope.MarkAgentCreated(ctx, "a")
	res, err := RunCuratorReview(ctx, CuratorRunOptions{Synchronous: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Get("started_at"); !ok {
		t.Fatal("result lacks started_at")
	}
	st, _ := LoadCuratorState(ctx)
	if st.Vals["last_run_at"] == nil || curatorCount(st, "run_count") < 1 || st.Vals["last_run_summary"] == nil {
		t.Fatalf("state %v", st.Vals)
	}
	curRunsMu.Lock()
	runs := slices.Clone(curRuns)
	curRunsMu.Unlock()
	if len(runs) != 1 || runs[0] != [2]string{"manual", "success"} {
		t.Fatalf("run metric %v", runs)
	}
	report := pyStr(st.Vals["last_report_path"])
	for _, f := range []string{"run.json", "REPORT.md"} {
		if !curatorExists(filepath.Join(report, f)) {
			t.Fatalf("missing %s in %s", f, report)
		}
	}
}

func TestCuratorDryRunInjectsReportOnlyBanner(t *testing.T) {
	ctx, h := curTestHome(t, nil)
	curWriteSkill(t, filepath.Join(h, "skills"), "a", "")
	scope, _, _ := curatorScope(ctx)
	scope.MarkAgentCreated(ctx, "a")
	var captured string
	curatorLLMReview = func(_ context.Context, prompt string) *pyjson.Object {
		captured = prompt
		return curatorLLMMeta("s", nil)
	}
	on := true
	if _, err := RunCuratorReview(ctx, CuratorRunOptions{Synchronous: true, DryRun: true, Consolidate: &on}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(captured, CuratorDryRunBanner) {
		t.Fatal("dry run prompt lacks the banner")
	}
}

func TestCuratorSynchronousRunInvokesLLMStub(t *testing.T) {
	ctx, h := curTestHome(t, nil)
	curWriteSkill(t, filepath.Join(h, "skills"), "a", "")
	scope, _, _ := curatorScope(ctx)
	scope.MarkAgentCreated(ctx, "a")
	calls := 0
	curatorLLMReview = func(context.Context, string) *pyjson.Object {
		calls++
		m := curatorLLMMeta("stubbed-summary", nil)
		m.Set("final", "stubbed-summary")
		m.Set("model", "stub-model")
		m.Set("provider", "stub-provider")
		return m
	}
	var captured []string
	on := true
	if _, err := RunCuratorReview(ctx, CuratorRunOptions{OnSummary: func(s string) { captured = append(captured, s) }, Synchronous: true, Consolidate: &on}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(captured) == 0 || !strings.Contains(strings.Join(captured, "\n"), "stubbed-summary") {
		t.Fatalf("calls %d captured %q", calls, captured)
	}
}

func TestCuratorParseStructuredSummary(t *testing.T) {
	text := "Long human summary here. I processed clusters X, Y, Z.\n\n## Structured summary (required)\n```yaml\nconsolidations:\n" +
		"  - from: anthropic-api\n    into: llm-providers\n    reason: duplicate of the generic llm-providers skill\n" +
		"  - from: openai-api\n    into: llm-providers\n    reason: same — merged with sibling\nprunings:\n" +
		"  - name: random-old-notes\n    reason: pre-curator garbage, no overlap\n```\n"
	cons, prunes := curatorParseStructuredSummary(text)
	got, _ := pyjson.Dumps(cons[0])
	if len(cons) != 2 || got != `{"from": "anthropic-api", "into": "llm-providers", "reason": "duplicate of the generic llm-providers skill"}` {
		t.Fatalf("consolidations %v %s", len(cons), got)
	}
	if len(prunes) != 1 || prunes[0].Vals["reason"] != "pre-curator garbage, no overlap" {
		t.Fatalf("prunings %v", prunes)
	}
	if c, p := curatorParseStructuredSummary("No block in this text."); len(c) != 0 || len(p) != 0 {
		t.Fatal("missing block must give empty lists")
	}
}

func TestCuratorAbsorbedIntoDeclarations(t *testing.T) {
	call := func(args string) any { return map[string]any{"name": "skill_manage", "arguments": args} }
	got := curatorAbsorbedDeclarations([]any{call(`{"action": "delete", "name": "narrow-skill", "absorbed_into": "umbrella"}`)})
	if len(got) != 1 || got["narrow-skill"] != "umbrella" {
		t.Fatalf("declarations %v", got)
	}
	if got := curatorAbsorbedDeclarations([]any{call(`{"action": "patch", "name": "umbrella", "absorbed_into": "something"}`)}); len(got) != 0 {
		t.Fatalf("non delete declarations %v", got)
	}
}

func TestCuratorReconcileClassification(t *testing.T) {
	cons, pruned := curatorReconcile([]string{"pr-review-format"}, nil, nil, nil, map[string]bool{"icebath-agent-dev": true},
		map[string]string{"pr-review-format": "icebath-agent-dev"})
	if len(cons) != 1 || len(pruned) != 0 || cons[0].Vals["into"] != "icebath-agent-dev" || !strings.Contains(pyStr(cons[0].Vals["source"]), "absorbed_into") {
		t.Fatalf("declared absorption lost: %v %v", cons, pruned)
	}
	heur := []*pyjson.Object{curatorEntry("name", "legacy-cons", "into", "umbrella-a", "evidence", "...")}
	cons, pruned = curatorReconcile([]string{"declared-cons", "declared-prune", "legacy-cons", "legacy-prune"}, heur, nil, nil,
		map[string]bool{"umbrella-a": true, "umbrella-b": true}, map[string]string{"declared-cons": "umbrella-b", "declared-prune": ""})
	byName := func(list []*pyjson.Object) map[string]*pyjson.Object {
		out := map[string]*pyjson.Object{}
		for _, e := range list {
			out[pyStr(e.Vals["name"])] = e
		}
		return out
	}
	cb, pb := byName(cons), byName(pruned)
	if cb["declared-cons"] == nil || cb["declared-cons"].Vals["into"] != "umbrella-b" || cb["legacy-cons"] == nil || cb["legacy-cons"].Vals["into"] != "umbrella-a" ||
		pb["declared-prune"] == nil || pb["legacy-prune"] == nil {
		t.Fatalf("mixed reconcile %v %v", cons, pruned)
	}
}

func TestCuratorRenameSummary(t *testing.T) {
	report := func(names ...string) []*pyjson.Object {
		var out []*pyjson.Object
		for _, n := range names {
			out = append(out, curatorEntry("name", n, "state", "active"))
		}
		return out
	}
	if s := curatorRenameSummary(map[string]bool{"alpha": true, "beta": true}, report("alpha", "beta"), nil, ""); s != "" {
		t.Fatalf("summary %q", s)
	}
	calls := []any{
		map[string]any{"name": "skill_manage", "arguments": `{"action": "delete", "name": "merge-me", "absorbed_into": "umbrella"}`},
		map[string]any{"name": "skill_manage", "arguments": `{"action": "delete", "name": "drop-me", "absorbed_into": ""}`},
	}
	s := curatorRenameSummary(map[string]bool{"merge-me": true, "drop-me": true, "umbrella": true}, report("umbrella"), calls, "")
	mi, di := strings.Index(s, "merge-me → umbrella"), strings.Index(s, "drop-me — pruned (stale)")
	if mi < 0 || di < 0 || mi > di || !strings.Contains(s, "keep an umbrella stable: icebath curator pin umbrella") {
		t.Fatalf("summary %q", s)
	}
}

func curBackupHome(t *testing.T, keep any) (context.Context, string) {
	ctx, h := curTestHome(t, nil)
	prevCfg, prevID, prevSnap := curatorBackupConfig, curatorUTCIDNow, curatorSnapshotSkills
	curatorBackupConfig = func(context.Context) map[string]any {
		if keep == nil {
			return map[string]any{}
		}
		return map[string]any{"keep": keep}
	}
	t.Cleanup(func() { curatorBackupConfig, curatorUTCIDNow, curatorSnapshotSkills = prevCfg, prevID, prevSnap })
	return ctx, filepath.Join(h, "skills")
}

func TestCuratorSnapshotUniquifiesAndPrunes(t *testing.T) {
	ctx, sk := curBackupHome(t, nil)
	curWriteSkill(t, sk, "alpha", "body")
	curatorUTCIDNow = func(time.Time) string { return "2026-05-01T12-00-00Z" }
	s1, ok1, _ := SnapshotSkills(ctx, "a", nil)
	s2, ok2, _ := SnapshotSkills(ctx, "b", nil)
	if !ok1 || !ok2 || filepath.Base(s1) != "2026-05-01T12-00-00Z" || filepath.Base(s2) != "2026-05-01T12-00-00Z-01" {
		t.Fatalf("ids %s %s", s1, s2)
	}
	ctx, sk = curBackupHome(t, 3)
	curWriteSkill(t, sk, "alpha", "body")
	var ids []string
	for i := 1; i <= 5; i++ {
		id := "2026-05-0" + string(rune('0'+i)) + "T00-00-00Z"
		ids = append(ids, id)
		curatorUTCIDNow = func(time.Time) string { return id }
		SnapshotSkills(ctx, "n", nil)
	}
	entries, _ := os.ReadDir(filepath.Join(sk, ".curator_backups"))
	var remaining []string
	for _, e := range entries {
		remaining = append(remaining, e.Name())
	}
	sort.Strings(remaining)
	if strings.Join(remaining, ",") != strings.Join(ids[2:], ",") {
		t.Fatalf("remaining %v", remaining)
	}
}

func TestCuratorRollbackIsItselfUndoable(t *testing.T) {
	ctx, sk := curBackupHome(t, nil)
	curWriteSkill(t, sk, "v1", "body")
	if _, ok, _ := SnapshotSkills(ctx, "snapshot-of-v1", nil); !ok {
		t.Fatal("snapshot failed")
	}
	os.RemoveAll(filepath.Join(sk, "v1"))
	curWriteSkill(t, sk, "v2", "body")
	if ok, msg, _ := RollbackCuratorSnapshot(ctx, ""); !ok {
		t.Fatal(msg)
	}
	if !curatorExists(filepath.Join(sk, "v1")) || curatorExists(filepath.Join(sk, "v2")) {
		t.Fatal("tree not restored")
	}
	rows, _ := ListCuratorBackups(ctx)
	found := false
	for _, r := range rows {
		found = found || strings.Contains(pyStr(r.Vals["reason"]), "pre-rollback")
	}
	entries, _ := os.ReadDir(filepath.Join(sk, ".curator_backups"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".rollback-") {
			t.Fatalf("staging left behind %s", e.Name())
		}
	}
	if !found {
		t.Fatal("no pre-rollback safety snapshot listed")
	}
}

func TestCuratorRollbackAbortsAndRefuses(t *testing.T) {
	ctx, sk := curBackupHome(t, nil)
	if ok, _, restored := RollbackCuratorSnapshot(ctx, ""); ok || restored != "" {
		t.Fatal("rollback without snapshots succeeded")
	}
	file := filepath.Join(curWriteSkill(t, sk, "alpha", "snapshot state"), "SKILL.md")
	target, ok, _ := SnapshotSkills(ctx, "rollback-target", nil)
	if !ok {
		t.Fatal("snapshot failed")
	}
	os.WriteFile(file, []byte("current state\n"), 0o644)
	curatorSnapshotSkills = func(context.Context, string, map[string]bool) (string, bool, error) { return "", false, nil }
	if ok, _, restored := RollbackCuratorSnapshot(ctx, filepath.Base(target)); ok || restored != "" {
		t.Fatal("rollback ran without a safety snapshot")
	}
	if b, _ := os.ReadFile(file); string(b) != "current state\n" {
		t.Fatal("current skills changed")
	}
	curatorSnapshotSkills = SnapshotSkills
	mal := filepath.Join(target, curatorArchiveName)
	f, _ := os.Create(mal)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "../../etc/evil.md", Mode: 0o644, Size: 4, Typeflag: tar.TypeReg})
	tw.Write([]byte("evil"))
	tw.Close()
	gz.Close()
	f.Close()
	ok, msg, _ := RollbackCuratorSnapshot(ctx, filepath.Base(target))
	if ok || !(strings.Contains(strings.ToLower(msg), "unsafe") || strings.Contains(msg, "extract")) {
		t.Fatalf("unsafe tarball accepted: %v %s", ok, msg)
	}
	if b, _ := os.ReadFile(file); string(b) != "current state\n" {
		t.Fatal("failed extract did not restore the staged tree")
	}
}

func TestCuratorCLIPauseResumeAndStatus(t *testing.T) {
	ctx, _ := curTestHome(t, nil)
	var out bytes.Buffer
	cli := NewCuratorCLI(&out, &out, strings.NewReader(""))
	for _, sub := range []string{"pause", "status", "resume"} {
		if code, err := cli.Run(ctx, sub, CuratorArgs{}); code != 0 || err != nil {
			t.Fatalf("%s: %d %v", sub, code, err)
		}
	}
	for _, want := range []string{"curator: paused", "curator: PAUSED", "no curator-managed skills", "curator: resumed"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
}
