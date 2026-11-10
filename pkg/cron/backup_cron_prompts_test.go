package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func seedCronDoc(t *testing.T, path string, jobs []map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"jobs": jobs})
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Copies the live jobs.json into a pre update quick snapshot of the home
func snapshotCronJobs(t *testing.T, h, id string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h, "cron", "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(h, quickSnapshotsDir, id, "cron", "jobs.json")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return id
}

func liveCronJobs(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct{ Jobs []map[string]any }
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Jobs
}

func backupHome(t *testing.T) (string, string) {
	h := filepath.Join(t.TempDir(), ".icebath")
	return h, filepath.Join(h, "cron", "jobs.json")
}

func TestRestoresPromptsWhenCountUnchangedButClobbered(t *testing.T) {
	h, jobsPath := backupHome(t)
	var orig, clobbered []map[string]any
	for i := range 6 {
		orig = append(orig, map[string]any{"id": fmt.Sprintf("job-%d", i), "name": fmt.Sprintf("Morning brief %d", i), "prompt": fmt.Sprintf("Read the news and write brief %d.", i)})
		clobbered = append(clobbered, map[string]any{"id": fmt.Sprintf("job-%d", i), "name": fmt.Sprintf("Morning brief %d", i), "prompt": fmt.Sprintf("Morning brief %d", i)})
	}
	seedCronDoc(t, jobsPath, orig)
	snap := snapshotCronJobs(t, h, "20260501-000000-pre-update")
	seedCronDoc(t, jobsPath, clobbered)
	result, err := RestoreCronPromptFieldsIfDegraded(context.Background(), h, snap)
	if err != nil || result == nil || !result.Restored || result.Prompts != 6 || result.SnapshotID != snap {
		t.Fatalf("result %+v %v", result, err)
	}
	for i, job := range liveCronJobs(t, jobsPath) {
		if job["prompt"] != fmt.Sprintf("Read the news and write brief %d.", i) || job["name"] != fmt.Sprintf("Morning brief %d", i) {
			t.Fatalf("job %d %v", i, job)
		}
	}
}

func TestBlankPromptIsRestoredToo(t *testing.T) {
	h, jobsPath := backupHome(t)
	seedCronDoc(t, jobsPath, []map[string]any{{"id": "a", "name": "Cleanup", "prompt": "Clean the tmp dir."}})
	snap := snapshotCronJobs(t, h, "snap")
	seedCronDoc(t, jobsPath, []map[string]any{{"id": "a", "name": "Cleanup", "prompt": ""}})
	result, err := RestoreCronPromptFieldsIfDegraded(context.Background(), h, snap)
	if err != nil || result == nil || result.Prompts != 1 || liveCronJobs(t, jobsPath)[0]["prompt"] != "Clean the tmp dir." {
		t.Fatalf("result %+v %v", result, err)
	}
}

func TestNoAgentJobsAreUntouched(t *testing.T) {
	h, jobsPath := backupHome(t)
	seedCronDoc(t, jobsPath, []map[string]any{{"id": "agent", "name": "Brief", "prompt": "Write the brief."}, {"id": "script", "name": "Backup", "no_agent": true, "script": "echo hi", "prompt": "Backup"}})
	snap := snapshotCronJobs(t, h, "snap")
	seedCronDoc(t, jobsPath, []map[string]any{{"id": "agent", "name": "Brief", "prompt": "Brief"}, {"id": "script", "name": "Backup", "no_agent": true, "script": "echo hi", "prompt": "Backup"}})
	result, err := RestoreCronPromptFieldsIfDegraded(context.Background(), h, snap)
	if err != nil || result == nil || result.Prompts != 1 || !reflect.DeepEqual(result.JobIDs, []string{"agent"}) {
		t.Fatalf("result %+v %v", result, err)
	}
	jobs := liveCronJobs(t, jobsPath)
	if jobs[0]["prompt"] != "Write the brief." || jobs[1]["prompt"] != "Backup" {
		t.Fatalf("jobs %v", jobs)
	}
}

func TestDegradedPromptRestoreLeavesHealthyOrUnmatchedJobs(t *testing.T) {
	cases := []struct {
		name       string
		snap, live []map[string]any
	}{
		{"a legitimate user edit is not stomped", []map[string]any{{"id": "a", "name": "Old", "prompt": "Old prompt text."}}, []map[string]any{{"id": "a", "name": "New name", "prompt": "Deliberately rewritten prompt."}}},
		{"a legit prompt equal to name stays", []map[string]any{{"id": "a", "name": "Ping", "prompt": ""}}, []map[string]any{{"id": "a", "name": "Ping", "prompt": "Ping"}}},
		{"unknown job ids are left alone", []map[string]any{{"id": "a", "name": "A", "prompt": "A prompt."}}, []map[string]any{{"id": "a", "name": "A", "prompt": "A prompt."}, {"id": "new", "name": "New", "prompt": "New"}}},
		{"healthy file is untouched", []map[string]any{{"id": "a", "name": "A", "prompt": "A prompt."}}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, jobsPath := backupHome(t)
			seedCronDoc(t, jobsPath, c.snap)
			snap := snapshotCronJobs(t, h, "snap")
			if c.live != nil {
				seedCronDoc(t, jobsPath, c.live)
			}
			before, _ := os.ReadFile(jobsPath)
			result, err := RestoreCronPromptFieldsIfDegraded(context.Background(), h, snap)
			after, _ := os.ReadFile(jobsPath)
			if err != nil || result != nil || string(before) != string(after) {
				t.Fatalf("result %+v %v\n%s\n%s", result, err, before, after)
			}
		})
	}
}

func TestConcurrentCronEditsSurviveRecovery(t *testing.T) {
	h, jobsPath := backupHome(t)
	sched := map[string]any{"kind": "interval", "minutes": 60, "display": "every 60m"}
	job := func(id, prompt string) map[string]any {
		return map[string]any{"id": id, "name": map[string]string{"a": "A", "b": "B", "c": "C"}[id], "prompt": prompt, "schedule": sched, "enabled": true}
	}
	seedCronDoc(t, jobsPath, []map[string]any{job("a", "A real prompt."), job("b", "B real prompt."), job("c", "C real prompt.")})
	snap := snapshotCronJobs(t, h, "snap")
	seedCronDoc(t, jobsPath, []map[string]any{job("a", "A"), job("b", "B"), job("c", "C real prompt.")})
	prev := loadCronJobsDoc
	t.Cleanup(func() { loadCronJobsDoc = prev })
	var fired []string
	loadCronJobsDoc = func(path string) (any, error) {
		doc, err := prev(path)
		if path == jobsPath && len(fired) == 0 {
			fired = append(fired, "fired")
			s := openCronStore(h, nil)
			prompt, name := "New prompt.", "new"
			created, cerr := s.createJob(context.Background(), CreateJobOptions{Prompt: &prompt, Schedule: "every 1h", Name: &name})
			if cerr != nil {
				t.Fatal(cerr)
			}
			fired = append(fired, created.ID)
			if ok, rerr := s.removeJob(context.Background(), "c"); rerr != nil || !ok {
				t.Fatalf("remove %v %v", ok, rerr)
			}
			every2h, perr := s.parseSchedule("every 2h")
			if perr != nil {
				t.Fatal(perr)
			}
			if _, uerr := s.updateJob(context.Background(), "a", map[string]any{"schedule": every2h}); uerr != nil {
				t.Fatal(uerr)
			}
			if _, uerr := s.updateJob(context.Background(), "b", map[string]any{"prompt": "User rewrote b."}); uerr != nil {
				t.Fatal(uerr)
			}
		}
		return doc, err
	}
	result, err := RestoreCronPromptFieldsIfDegraded(context.Background(), h, snap)
	if len(fired) != 2 || err != nil || result == nil || !reflect.DeepEqual(result.JobIDs, []string{"a"}) {
		t.Fatalf("fired %v result %+v %v", fired, result, err)
	}
	final := map[string]map[string]any{}
	for _, j := range liveCronJobs(t, jobsPath) {
		final[j["id"].(string)] = j
	}
	if len(final) != 3 || final["a"] == nil || final["b"] == nil || final[fired[1]] == nil {
		t.Fatalf("final %v", final)
	}
	if final["a"]["prompt"] != "A real prompt." || final["a"]["schedule"].(map[string]any)["minutes"] != float64(120) ||
		final["b"]["prompt"] != "User rewrote b." || final[fired[1]]["prompt"] != "New prompt." {
		t.Fatalf("final %v", final)
	}
}
