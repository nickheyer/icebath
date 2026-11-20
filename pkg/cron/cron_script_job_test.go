package cron

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJobScriptFieldCreateJobWithScript(t *testing.T) {
	s := scriptStore(t)
	job, err := s.createJob(context.Background(), CreateJobOptions{Prompt: strPtr("Analyze the data"), Schedule: "every 30m", Script: "/path/to/monitor.py"})
	if err != nil {
		t.Fatal(err)
	}
	if deref(job.Script) != "/path/to/monitor.py" {
		t.Fatalf("created %v", job.Script)
	}
	if loaded := mustJob(t, s, job.ID); deref(loaded.Script) != "/path/to/monitor.py" {
		t.Fatalf("loaded %v", loaded.Script)
	}
}

func TestJobScriptFieldUpdateJobAddScript(t *testing.T) {
	s := scriptStore(t)
	job, err := s.createJob(context.Background(), CreateJobOptions{Prompt: strPtr("Hello"), Schedule: "every 1h"})
	if err != nil {
		t.Fatal(err)
	}
	if job.Script != nil {
		t.Fatalf("fresh job carries a script %q", *job.Script)
	}
	updated, err := s.updateJob(context.Background(), job.ID, map[string]any{"script": "/new/script.py"})
	if err != nil || updated == nil || deref(updated.Script) != "/new/script.py" {
		t.Fatalf("got %v %v", updated, err)
	}
}

func TestCronjobToolRejectsStalePastOneShot(t *testing.T) {
	s := scriptStore(t)
	now := time.Date(2026, 3, 18, 4, 30, 0, 0, time.UTC)
	setNow(s, now)
	stale := now.Add(-5 * time.Minute).Format("2006-01-02T15:04:05-07:00")
	result := runCronjob(t, s, &cronjobArgs{Action: "create", Prompt: "Too late", Schedule: stale})
	if result["success"] != false || !strings.Contains(result["error"].(string), "past and cannot be scheduled") {
		t.Fatalf("got %v", result)
	}
}

func TestCronjobToolScriptClearScript(t *testing.T) {
	t.Setenv("ICEBATH_INTERACTIVE", "1")
	s := scriptStore(t)
	writeScript(t, s, "some_script.py", "print('hi')\n")
	created := runCronjob(t, s, &cronjobArgs{Action: "create", Schedule: "every 1h", Prompt: "Monitor things", Script: "some_script.py"})
	jobID, _ := created["job_id"].(string)
	if jobID == "" {
		t.Fatalf("create %v", created)
	}
	updated := runCronjob(t, s, &cronjobArgs{Action: "update", JobID: jobID, Script: ""})
	job, _ := updated["job"].(map[string]any)
	if _, has := job["script"]; updated["success"] != true || job == nil || has {
		t.Fatalf("update %v", updated)
	}
}

func TestCronjobToolScriptListShowsScript(t *testing.T) {
	t.Setenv("ICEBATH_INTERACTIVE", "1")
	s := scriptStore(t)
	writeScript(t, s, "data_collector.py", "print('hi')\n")
	runCronjob(t, s, &cronjobArgs{Action: "create", Schedule: "every 1h", Prompt: "Monitor things", Script: "data_collector.py"})
	listed := runCronjob(t, s, &cronjobArgs{Action: "list"})
	jobs, _ := listed["jobs"].([]any)
	if listed["success"] != true || len(jobs) != 1 || jobs[0].(map[string]any)["script"] != "data_collector.py" {
		t.Fatalf("list %v", listed)
	}
}

func TestCronjobToolScriptValidationCreateWithTraversalScriptRejected(t *testing.T) {
	t.Setenv("ICEBATH_INTERACTIVE", "1")
	s := scriptStore(t)
	result := runCronjob(t, s, &cronjobArgs{Action: "create", Schedule: "every 1h", Prompt: "Monitor things", Script: "../../etc/passwd"})
	message, _ := result["error"].(string)
	low := strings.ToLower(message)
	if result["success"] != false || !(strings.Contains(low, "escapes") || strings.Contains(low, "traversal")) {
		t.Fatalf("got %v", result)
	}
}

func scriptPromptStore(t *testing.T) *Store {
	t.Helper()
	s := promptStore(t)
	if err := os.MkdirAll(filepath.Join(s.Home(), "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Dir(s.Home()))
	return s
}

func TestBuildJobPromptWithScriptScriptOutputInjected(t *testing.T) {
	s := scriptPromptStore(t)
	script := writeScript(t, s, "data.py", "print(\"new PR: #123 fix typo\")\n")
	prompt := mustPrompt(t, s, &Job{Prompt: "Report any notable changes.", Script: &script}, buildPromptOptions{})
	for _, want := range []string{"## Script Output", "new PR: #123 fix typo", "Report any notable changes."} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("%q missing from %q", want, prompt)
		}
	}
}

func TestBuildJobPromptWithScriptScriptErrorInjected(t *testing.T) {
	s := scriptPromptStore(t)
	prompt := mustPrompt(t, s, &Job{Prompt: "Report status.", Script: strPtr("nonexistent_monitor.py")}, buildPromptOptions{})
	if !strings.Contains(prompt, "## Script Error") || !strings.Contains(strings.ToLower(prompt), "not found") || !strings.Contains(prompt, "Report status.") {
		t.Fatalf("got %q", prompt)
	}
}

func TestBuildJobPromptWithScriptNoScriptUnchanged(t *testing.T) {
	s := scriptPromptStore(t)
	prompt := mustPrompt(t, s, &Job{Prompt: "Simple job."}, buildPromptOptions{})
	if strings.Contains(prompt, "## Script Output") || !strings.Contains(prompt, "Simple job.") {
		t.Fatalf("got %q", prompt)
	}
}

func TestRunJobEnvVarCleanupEnvVarsCleanedOnEarlyError(t *testing.T) {
	keys := []string{"ICEBATH_SESSION_PLATFORM", "ICEBATH_SESSION_CHAT_ID", "ICEBATH_SESSION_CHAT_NAME"}
	for _, key := range keys {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	s, _ := newTestStore(t)
	resetDeliver(t)
	job := &Job{ID: "test-envleak", Name: "env-leak-test", Prompt: "test", ScheduleDisplay: "every 1h",
		Origin: map[string]any{"platform": "telegram", "chat_id": "12345", "chat_name": "Test Chat"}}
	ctx, execID := ownerRun(t, s, job, &fakeCronAgent{})
	installFakeAgent(t, func(context.Context, AgentOptions) (CronAgent, error) {
		return nil, errors.New("no model or API key is configured")
	})
	out, err := s.runJob(ctx, job, runJobOptions{ExecutionID: &execID})
	if err == nil && out.Success {
		t.Fatalf("the run succeeded without a model %+v", out)
	}
	for _, key := range keys {
		if value, set := os.LookupEnv(key); set {
			t.Fatalf("%s leaked as %q", key, value)
		}
	}
}
