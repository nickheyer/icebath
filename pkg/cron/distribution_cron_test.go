package cron_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
)

// Temp user home with the default profile home under it, the profile_env fixture
func profileEnv(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Cleanup(home.PatchUserHome(tmp))
	def := filepath.Join(tmp, ".icebath")
	if err := os.MkdirAll(def, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, def)
	profiles.ResetFileCache()
	profiles.ResetStandaloneMemo()
	home.ResetKeyCache()
	return tmp
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Local distribution staging tree with one skill, one cron job and its manifest
func makeStagingDir(t *testing.T, root, name string) string {
	t.Helper()
	staged := filepath.Join(root, "staging_"+name)
	writeFile(t, filepath.Join(staged, "SOUL.md"), "I am Source.\n")
	writeFile(t, filepath.Join(staged, "config.yaml"), "model:\n  model: gpt-4\n")
	writeFile(t, filepath.Join(staged, "mcp.json"), "{\"servers\": {}}\n")
	writeFile(t, filepath.Join(staged, "skills", "demo", "SKILL.md"), "---\nname: demo\ndescription: test\n---\n# Demo skill\n")
	if _, err := cron.ExportOpenStore(staged).CreateJob("daily task", "0 9 * * *", "daily"); err != nil {
		t.Fatal(err)
	}
	if _, err := profiles.WriteManifest(context.Background(), staged, profiles.NewDistributionManifest(name)); err != nil {
		t.Fatal(err)
	}
	return staged
}

func jobsByID(t *testing.T, dir string) map[string]*cron.Job {
	t.Helper()
	jobs, err := cron.ExportOpenStore(dir).ListJobs(true)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*cron.Job{}
	for _, j := range jobs {
		out[j.ID] = j
	}
	return out
}

func firstJob(t *testing.T, dir string) *cron.Job {
	t.Helper()
	jobs, err := cron.ExportOpenStore(dir).ListJobs(true)
	if err != nil || len(jobs) == 0 {
		t.Fatalf("no shipped job %v", err)
	}
	return jobs[0]
}

func scheduleJSON(t *testing.T, j *cron.Job) string {
	t.Helper()
	raw, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(rec["schedule"])
	return string(out)
}

func TestInstallPausesShippedCronJobsAndSkipsRuntimeState(t *testing.T) {
	env := profileEnv(t)
	staged := makeStagingDir(t, env, "cron_src")
	shipped := firstJob(t, staged)
	if _, err := cron.ExportOpenStore(staged).UpdateJob(shipped.ID, map[string]any{
		"next_run_at": time.Now().UTC().Add(-48 * time.Hour).Format("2006-01-02T15:04:05.000000+00:00"),
	}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(staged, "cron", "future-runtime.bin"), "runtime")
	writeFile(t, filepath.Join(staged, "skills", ".future-runtime"), "runtime")
	writeFile(t, filepath.Join(staged, "skills", "demo", ".authored-hidden"), "keep")

	plan, err := manage.InstallDistribution(context.Background(), staged, "cron_paused", false, false)
	if err != nil {
		t.Fatal(err)
	}
	installed := jobsByID(t, plan.TargetDir)
	due, err := cron.ExportOpenStore(plan.TargetDir).DueJobs()
	if err != nil {
		t.Fatal(err)
	}
	job := installed[shipped.ID]
	if job == nil || cron.ExportIsJobRunnable(job) || job.State != "paused" {
		t.Fatalf("installed job %+v", job)
	}
	if len(due) != 0 {
		t.Fatalf("shipped job due on install %d", len(due))
	}
	for _, rel := range []string{"cron/future-runtime.bin", "skills/.future-runtime"} {
		if _, err := os.Stat(filepath.Join(plan.TargetDir, rel)); !os.IsNotExist(err) {
			t.Fatalf("runtime file %s copied: %v", rel, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(plan.TargetDir, "skills", "demo", ".authored-hidden")); err != nil || string(got) != "keep" {
		t.Fatalf("authored hidden file %q %v", got, err)
	}
}

func TestUpdateMergesCronJobsWithoutLosingLocalState(t *testing.T) {
	env := profileEnv(t)
	ctx := context.Background()
	staged := makeStagingDir(t, env, "cron_update")
	shipped := firstJob(t, staged)
	plan, err := manage.InstallDistribution(ctx, staged, "cron_merge", false, false)
	if err != nil {
		t.Fatal(err)
	}
	target := cron.ExportOpenStore(plan.TargetDir)
	if _, err := target.ResumeJob(shipped.ID); err != nil {
		t.Fatal(err)
	}
	mine, err := target.CreateJob("water the plants", "0 9 * * *", "mine")
	if err != nil {
		t.Fatal(err)
	}
	reason := "my choice"
	if _, err := target.PauseJob(mine.ID, &reason); err != nil {
		t.Fatal(err)
	}
	oldNextRun := jobsByID(t, plan.TargetDir)[shipped.ID].NextRunAt
	if _, err := cron.ExportOpenStore(staged).UpdateJob(shipped.ID, map[string]any{"prompt": "updated upstream definition", "schedule": "every 7d"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manage.UpdateDistribution(ctx, "cron_merge", false); err != nil {
		t.Fatal(err)
	}
	jobs := jobsByID(t, plan.TargetDir)
	if m := jobs[mine.ID]; m == nil || m.State != "paused" || m.PausedReason == nil || *m.PausedReason != "my choice" {
		t.Fatalf("local job %+v", m)
	}
	merged := jobs[shipped.ID]
	if merged.Prompt != "updated upstream definition" || merged.Schedule.Minutes != 7*24*60 || !merged.Enabled {
		t.Fatalf("merged job %+v", merged)
	}
	if merged.NextRunAt == nil || oldNextRun != nil && *merged.NextRunAt == *oldNextRun {
		t.Fatal("schedule change did not move next_run_at")
	}

	writeFile(t, filepath.Join(staged, "SOUL.md"), "upstream v3\n")
	storePath := filepath.Join(staged, "cron", "jobs.json")
	raw, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	data["jobs"].([]any)[0].(map[string]any)["schedule"] = map[string]any{"kind": "once", "run_at": "2020-01-01T00:00:00+00:00", "display": "once"}
	raw, _ = json.Marshal(data)
	writeFile(t, storePath, string(raw))
	_, err = manage.UpdateDistribution(ctx, "cron_merge", false)
	var distErr *profiles.DistributionError
	if !errors.As(err, &distErr) || !strings.Contains(err.Error(), "cron job 'daily'") {
		t.Fatalf("past one shot update %v", err)
	}
	if soul, _ := os.ReadFile(filepath.Join(plan.TargetDir, "SOUL.md")); string(soul) == "upstream v3\n" {
		t.Fatal("files replaced before the cron merge was refused")
	}
	if got := scheduleJSON(t, jobsByID(t, plan.TargetDir)[shipped.ID]); got != scheduleJSON(t, merged) {
		t.Fatalf("refused merge changed the schedule to %s", got)
	}

	writeFile(t, filepath.Join(plan.TargetDir, "cron", "jobs.json"), "42")
	_, err = manage.UpdateDistribution(ctx, "cron_merge", false)
	if !errors.As(err, &distErr) || !strings.Contains(err.Error(), "Could not merge cron jobs") {
		t.Fatalf("corrupt target store %v", err)
	}
}
