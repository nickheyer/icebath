package memory_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/memory"
)

func cronWriteJobs(t *testing.T, h string, payload any) {
	t.Helper()
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(h, "cron"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "cron", "jobs.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Jobs as stored in jobs.json, the list form or the jobs key of the wrapper form
func cronLoadJobs(t *testing.T, h string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h, "cron", "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wrapped struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Jobs != nil {
		return wrapped.Jobs
	}
	var list []map[string]any
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("jobs.json %s: %v", raw, err)
	}
	return list
}

func cronJob(ref string) []map[string]any {
	return []map[string]any{{"id": "job1", "name": "quarterly digest", "enabled": true, "prompt": "write the digest", "skills": []string{ref}, "schedule": map[string]any{"kind": "cron", "expr": "0 9 1 */3 *"}}}
}

func cronStateOf(t *testing.T, ctx context.Context, name string) any {
	t.Helper()
	return memory.CurGet(memory.CurRecord(t, ctx, name), "state")
}

func cronAsStrings(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

func TestCuratorCronReferencedSkillByNameSurvivesInactivity(t *testing.T) {
	ctx, h := memory.CurTestHome(t, nil)
	memory.CurWriteSkill(t, filepath.Join(h, "skills"), "quarterly-report", "")
	memory.CurBackdate(t, ctx, "quarterly-report", 200, 1, nil)
	cronWriteJobs(t, h, cronJob("quarterly-report"))
	counts, err := memory.ApplyCuratorTransitions(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if memory.CuratorCount(counts, "archived") != 0 || cronStateOf(t, ctx, "quarterly-report") != "active" {
		t.Fatalf("counts %v", counts)
	}
}

func TestCuratorCronReferencedSkillByAbsolutePathSurvivesInactivity(t *testing.T) {
	ctx, h := memory.CurTestHome(t, nil)
	dir := filepath.Join(h, "skills")
	memory.CurWriteSkill(t, dir, "quarterly-report", "")
	memory.CurBackdate(t, ctx, "quarterly-report", 200, 1, nil)
	cronWriteJobs(t, h, cronJob(filepath.Join(dir, "quarterly-report")))
	counts, err := memory.ApplyCuratorTransitions(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if memory.CuratorCount(counts, "archived") != 0 || cronStateOf(t, ctx, "quarterly-report") != "active" {
		t.Fatalf("counts %v", counts)
	}
}

func TestCuratorUnresolvableCronReferenceIsKeptVerbatim(t *testing.T) {
	_, h := memory.CurTestHome(t, nil)
	outside := filepath.Join(t.TempDir(), "elsewhere", "some-skill")
	cronWriteJobs(t, h, cronJob(outside))
	names, err := memory.CuratorHooks.ReferencedSkillNames(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || !names[strings.TrimLeft(strings.TrimSpace(outside), "/")] {
		t.Fatalf("names %v", names)
	}
}

func TestCuratorUnreferencedSkillIsStillArchived(t *testing.T) {
	ctx, h := memory.CurTestHome(t, nil)
	dir := filepath.Join(h, "skills")
	for _, n := range []string{"quarterly-report", "orphan"} {
		memory.CurWriteSkill(t, dir, n, "")
		memory.CurBackdate(t, ctx, n, 200, 1, nil)
	}
	cronWriteJobs(t, h, cronJob(filepath.Join(dir, "quarterly-report")))
	if _, err := memory.ApplyCuratorTransitions(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if cronStateOf(t, ctx, "quarterly-report") != "active" || cronStateOf(t, ctx, "orphan") != "archived" {
		t.Fatalf("states %v %v", cronStateOf(t, ctx, "quarterly-report"), cronStateOf(t, ctx, "orphan"))
	}
}

func TestCuratorRewritesCronSkillsWhenSkillConsolidated(t *testing.T) {
	ctx, h := memory.CurTestHome(t, nil)
	cronWriteJobs(t, h, map[string]any{"jobs": []map[string]any{{"id": "foo1", "name": "foo-watcher", "prompt": "", "skills": []string{"foo"}, "skill": "foo", "enabled": true,
		"schedule": map[string]any{"kind": "interval", "minutes": 60, "display": "every 60m"}, "schedule_display": "every 60m"}}, "updated_at": "2026-05-01T00:00:00Z"})
	meta := memory.CurMetaWith("Consolidated foo into foo-umbrella.", []any{memory.CurToolCall(map[string]any{"action": "write_file", "name": "foo-umbrella", "file_path": "references/foo.md", "file_content": "from foo"})})
	runDir := memory.CurWriteReport(t, ctx, `{"checked": 1, "marked_stale": 0, "archived": 0, "reactivated": 0}`, memory.CurRows("foo", "active"), memory.CurRows("foo-umbrella", "active"), map[string]bool{"foo": true}, meta)
	job := cronLoadJobs(t, h)[0]
	if cronAsStrings(job["skills"]) != `["foo-umbrella"]` || job["skill"] != "foo-umbrella" {
		t.Fatalf("job %v", job)
	}
	payload, md := memory.CurRunJSON(t, runDir)
	rw := payload["cron_rewrites"].(map[string]any)
	rewrites := rw["rewrites"].([]any)
	if rw["jobs_updated"] != float64(1) || payload["counts"].(map[string]any)["cron_jobs_rewritten"] != float64(1) || len(rewrites) != 1 ||
		cronAsStrings(rewrites[0].(map[string]any)["mapped"]) != `{"foo":"foo-umbrella"}` {
		t.Fatalf("payload %v", payload)
	}
	raw, err := os.ReadFile(filepath.Join(runDir, "cron_rewrites.json"))
	if err != nil {
		t.Fatal(err)
	}
	var detail map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil || detail["jobs_updated"] != float64(1) {
		t.Fatalf("cron_rewrites.json %s", raw)
	}
	if !strings.Contains(md, "foo-watcher") || !strings.Contains(md, "foo-umbrella") {
		t.Fatalf("REPORT.md:\n%s", md)
	}
}

func TestCuratorRollbackRestoresCronSkillLinks(t *testing.T) {
	ctx, dir := memory.CurBackupHome(t, nil)
	h := filepath.Dir(dir)
	for _, n := range []string{"alpha", "beta", "umbrella"} {
		memory.CurWriteSkill(t, dir, n, "body")
	}
	cronWriteJobs(t, h, map[string]any{"jobs": []map[string]any{{"id": "weekly1", "name": "weekly", "prompt": "p", "skills": []string{"alpha", "beta"}, "skill": "alpha", "enabled": true,
		"schedule": map[string]any{"kind": "interval", "minutes": 10080, "display": "every 10080m"}}}, "updated_at": "2026-05-01T00:00:00Z"})
	snap, ok, err := memory.SnapshotSkills(ctx, "pre-curator-run", nil)
	if !ok || err != nil {
		t.Fatal(err)
	}
	consolidated := pyjson.NewObject()
	consolidated.Set("alpha", "umbrella")
	consolidated.Set("beta", "umbrella")
	if _, err := memory.CuratorHooks.RewriteSkillRefs(ctx, h, consolidated, nil); err != nil {
		t.Fatal(err)
	}
	if got := cronAsStrings(cronLoadJobs(t, h)[0]["skills"]); got != `["umbrella"]` {
		t.Fatalf("after curator %s", got)
	}
	if ok, msg, _ := memory.RollbackCuratorSnapshot(ctx, filepath.Base(snap)); !ok {
		t.Fatal(msg)
	}
	if got := cronAsStrings(cronLoadJobs(t, h)[0]["skills"]); got != `["alpha","beta"]` {
		t.Fatalf("after rollback %s", got)
	}
}

func TestCuratorRollbackLeavesNewJobsUntouched(t *testing.T) {
	ctx, dir := memory.CurBackupHome(t, nil)
	h := filepath.Dir(dir)
	memory.CurWriteSkill(t, dir, "alpha", "body")
	cronWriteJobs(t, h, map[string]any{"jobs": []map[string]any{{"id": "original", "name": "o", "schedule": "every 1h", "skills": []string{"alpha"}}}, "updated_at": "2026-05-01T00:00:00Z"})
	snap, ok, err := memory.SnapshotSkills(ctx, "pre-curator-run", nil)
	if !ok || err != nil {
		t.Fatal(err)
	}
	cronWriteJobs(t, h, map[string]any{"jobs": []map[string]any{{"id": "original", "name": "o", "schedule": "every 1h", "skills": []string{"alpha"}},
		{"id": "new-after-snapshot", "name": "new", "schedule": "every 15m", "skills": []string{"brand-new-skill"}}}, "updated_at": "2026-05-01T00:00:00Z"})
	if ok, _, _ := memory.RollbackCuratorSnapshot(ctx, filepath.Base(snap)); !ok {
		t.Fatal("rollback failed")
	}
	for _, j := range cronLoadJobs(t, h) {
		if j["id"] == "new-after-snapshot" {
			if cronAsStrings(j["skills"]) != `["brand-new-skill"]` || !strings.Contains(cronAsStrings(j["schedule"]), "15") {
				t.Fatalf("new job changed %v", j)
			}
			return
		}
	}
	t.Fatal("new job dropped by the rollback")
}

func TestCuratorRestoreCronSkillLinksStandalone(t *testing.T) {
	ctx, dir := memory.CurBackupHome(t, nil)
	h := filepath.Dir(dir)
	backups := filepath.Join(dir, ".curator_backups", "fake-id")
	if err := os.MkdirAll(backups, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal([]map[string]any{{"id": "job-1", "name": "one", "skills": []string{"narrow-a", "narrow-b"}}, {"id": "job-2", "name": "two", "skill": "legacy-single"}, {"id": "job-gone", "name": "deleted", "skills": []string{"whatever"}}})
	if err := os.WriteFile(filepath.Join(backups, memory.CuratorCronJobsFilename), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	cronWriteJobs(t, h, map[string]any{"jobs": []map[string]any{{"id": "job-1", "name": "one", "skills": []string{"umbrella"}, "schedule": "every 1h"}, {"id": "job-2", "name": "two", "skill": "legacy-single", "schedule": "every 1h"},
		{"id": "job-new", "name": "new", "skills": []string{"x"}, "schedule": "every 1h"}}, "updated_at": "2026-05-01T00:00:00Z"})
	report := memory.CuratorRestoreCronSkillLinks(ctx, backups)
	restored, _ := memory.CurGet(report, "restored").([]any)
	missing, _ := memory.CurGet(report, "skipped_missing").([]any)
	if memory.CurGet(report, "attempted") != true || memory.CurGet(report, "error") != nil || memory.CurGet(report, "unchanged") != 1 || len(restored) != 1 || len(missing) != 1 {
		t.Fatalf("report %v", report)
	}
	r0 := restored[0].(*pyjson.Object)
	to := memory.CurGet(r0, "to").(*pyjson.Object)
	if memory.CurGet(r0, "job_id") != "job-1" || cronAsStrings(memory.CurGet(to, "skills")) != `["narrow-a","narrow-b"]` || memory.CurGet(missing[0].(*pyjson.Object), "job_id") != "job-gone" {
		t.Fatalf("report %v", report)
	}
}
