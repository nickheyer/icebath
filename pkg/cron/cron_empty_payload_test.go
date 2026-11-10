package cron

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Store whose home holds scripts/w.sh, the hermes_env fixture with a script planted
func wScriptStore(t *testing.T) *Store {
	t.Helper()
	s, _ := newTestStore(t)
	if err := os.MkdirAll(filepath.Join(s.home, "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.home, "scripts", "w.sh"), []byte("echo hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return s
}

func mustRejectCreate(t *testing.T, s *Store, o CreateJobOptions, want string) {
	t.Helper()
	if job, err := s.createJob(context.Background(), o); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("create %+v = %+v %v, want %q", o, job, err, want)
	}
}

func mustRejectUpdate(t *testing.T, s *Store, id string, updates map[string]any, want string) {
	t.Helper()
	if job, err := s.updateJob(context.Background(), id, updates); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("update %v = %+v %v, want %q", updates, job, err, want)
	}
}

func mustUpdate(t *testing.T, s *Store, id string, updates map[string]any) *Job {
	t.Helper()
	job, err := s.updateJob(context.Background(), id, updates)
	if err != nil || job == nil {
		t.Fatalf("update %v = %+v %v", updates, job, err)
	}
	return job
}

func TestCreateJobRejectsEmptyPayload(t *testing.T) {
	for _, p := range []*string{nil, prompt(""), prompt("   "), prompt("\n\t ")} {
		s, _ := newTestStore(t)
		mustRejectCreate(t, s, CreateJobOptions{Prompt: p, Schedule: "every 5m"}, "nothing to run")
	}
}

func TestCreateJobRejectsBlankScriptAndBlankSkills(t *testing.T) {
	s, _ := newTestStore(t)
	mustRejectCreate(t, s, CreateJobOptions{Prompt: prompt("  "), Schedule: "every 5m", Script: "   ", Skills: []any{"", "  "}}, "nothing to run")
}

func TestCreateJobNoAgentErrorStillWins(t *testing.T) {
	s, _ := newTestStore(t)
	mustRejectCreate(t, s, CreateJobOptions{Schedule: "every 5m", NoAgent: true}, "no_agent=True requires a script")
}

func TestValidShapesAreAccepted(t *testing.T) {
	s := wScriptStore(t)
	noAgent := mustCreate(t, s, CreateJobOptions{Schedule: "every 5m", Script: "w.sh", NoAgent: true, Deliver: prompt("local")})
	if !noAgent.NoAgent {
		t.Fatalf("no_agent job %+v", noAgent)
	}
	scriptOnly := mustCreate(t, s, CreateJobOptions{Schedule: "every 5m", Script: "w.sh", Deliver: prompt("local")})
	if deref(scriptOnly.Script) != "w.sh" || scriptOnly.NoAgent {
		t.Fatalf("script only job %+v", scriptOnly)
	}
	if job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("check the news"), Schedule: "every 5m", Deliver: prompt("local")}); job.Prompt != "check the news" {
		t.Fatalf("prompt job %+v", job)
	}
	if job := mustCreate(t, s, CreateJobOptions{Schedule: "every 5m", Skills: []any{"daily-report"}, Deliver: prompt("local")}); !reflect.DeepEqual(job.Skills, []string{"daily-report"}) {
		t.Fatalf("skills job %+v", job.Skills)
	}
	if job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("  "), Schedule: "every 5m", Skill: prompt("daily-report"), Deliver: prompt("local")}); deref(job.Skill) != "daily-report" {
		t.Fatalf("legacy skill job %v", deref(job.Skill))
	}
}

func TestUpdateJobRejectsClearingTheOnlyPayload(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("check the news"), Schedule: "every 5m", Deliver: prompt("local")})
	mustRejectUpdate(t, s, job.ID, map[string]any{"prompt": "   "}, "nothing to run")
	if got := mustGet(t, s, job.ID).Prompt; got != "check the news" {
		t.Fatalf("prompt persisted %q", got)
	}
}

func TestUpdateJobRejectsTogglingNoAgentOnWithoutAScript(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("check the news"), Schedule: "every 5m", Deliver: prompt("local")})
	mustRejectUpdate(t, s, job.ID, map[string]any{"no_agent": true}, "no_agent=True requires a script")
	if mustGet(t, s, job.ID).NoAgent {
		t.Fatal("no_agent persisted")
	}
}

func TestUpdateJobRejectsRemovingScriptFromANoAgentJob(t *testing.T) {
	for _, blank := range []any{nil, "", "   "} {
		s := wScriptStore(t)
		job := mustCreate(t, s, CreateJobOptions{Schedule: "every 5m", Script: "w.sh", NoAgent: true, Deliver: prompt("local")})
		mustRejectUpdate(t, s, job.ID, map[string]any{"script": blank}, "no_agent=True requires a script")
		if got := deref(mustGet(t, s, job.ID).Script); got != "w.sh" {
			t.Fatalf("script %q after %v", got, blank)
		}
	}
}

func TestUpdateJobRejectsSwappingScriptForPromptOnANoAgentJob(t *testing.T) {
	s := wScriptStore(t)
	job := mustCreate(t, s, CreateJobOptions{Schedule: "every 5m", Script: "w.sh", NoAgent: true, Deliver: prompt("local")})
	mustRejectUpdate(t, s, job.ID, map[string]any{"script": "", "prompt": "do it yourself"}, "no_agent=True requires a script")
}

func TestUpdateJobAllowsDroppingScriptWhenNoAgentIsTurnedOff(t *testing.T) {
	s := wScriptStore(t)
	job := mustCreate(t, s, CreateJobOptions{Schedule: "every 5m", Script: "w.sh", NoAgent: true, Deliver: prompt("local")})
	mustUpdate(t, s, job.ID, map[string]any{"no_agent": false, "script": nil, "prompt": "check the news"})
	if stored := mustGet(t, s, job.ID); stored.NoAgent || stored.Prompt != "check the news" {
		t.Fatalf("stored %+v", stored)
	}
}

func TestUpdateJobAllowsSwappingTheScriptOfANoAgentJob(t *testing.T) {
	s := wScriptStore(t)
	job := mustCreate(t, s, CreateJobOptions{Schedule: "every 5m", Script: "w.sh", NoAgent: true, Deliver: prompt("local")})
	mustUpdate(t, s, job.ID, map[string]any{"script": "other.sh"})
	if got := deref(mustGet(t, s, job.ID).Script); got != "other.sh" {
		t.Fatalf("script %q", got)
	}
}

func TestUpdateJobAllowsClearingPromptWhenScriptRemains(t *testing.T) {
	s := wScriptStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("summarize this"), Schedule: "every 5m", Script: "w.sh", Deliver: prompt("local")})
	mustUpdate(t, s, job.ID, map[string]any{"prompt": ""})
	if got := deref(mustGet(t, s, job.ID).Script); got != "w.sh" {
		t.Fatalf("script %q", got)
	}
}

func TestUpdateJobAllowsSwappingPromptForSkill(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("check the news"), Schedule: "every 5m", Deliver: prompt("local")})
	mustUpdate(t, s, job.ID, map[string]any{"prompt": "", "skills": []any{"daily-report"}})
	stored := mustGet(t, s, job.ID)
	if !reflect.DeepEqual(stored.Skills, []string{"daily-report"}) || strings.TrimSpace(stored.Prompt) != "" {
		t.Fatalf("stored skills %v prompt %q", stored.Skills, stored.Prompt)
	}
}

// Record created before the payload guard and hand blanked on disk, the legacy empty job
func legacyEmptyJob(t *testing.T, s *Store) *Job {
	t.Helper()
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("used to say something"), Schedule: "every 5m", Deliver: prompt("local")})
	editJobs(t, s, func(j *Job) {
		if j.ID == job.ID {
			j.Prompt = "   "
		}
	})
	job.Prompt = "   "
	return job
}

func TestPauseJobStillWorksOnAnAlreadyEmptyJob(t *testing.T) {
	s, _ := newTestStore(t)
	job := legacyEmptyJob(t, s)
	if paused, err := s.pauseJob(context.Background(), job.ID, prompt("empty payload")); err != nil || paused == nil {
		t.Fatalf("pause = %+v %v", paused, err)
	}
	if stored := mustGet(t, s, job.ID); stored.Enabled || stored.State != "paused" {
		t.Fatalf("stored %+v", stored)
	}
}
