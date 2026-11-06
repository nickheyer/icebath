package cron

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func reasoningJob(t *testing.T, s *Store, effort any) (*Job, error) {
	t.Helper()
	return s.createJob(CreateJobOptions{Prompt: strPtr("say hi"), Schedule: "every 1h", ReasoningEffort: effort})
}

func storedJobs(t *testing.T, s *Store) []*Job {
	t.Helper()
	jobs, err := s.listJobs(true)
	if err != nil {
		t.Fatal(err)
	}
	return jobs
}

func TestJobStoreReasoningEffort(t *testing.T) {
	for _, c := range [][2]string{{"none", "none"}, {"xhigh", "xhigh"}, {"HIGH", "high"}, {"  high ", "high"}, {"None", "none"}} {
		s, _ := newTestStore(t)
		job, err := reasoningJob(t, s, c[0])
		if err != nil || deref(job.ReasoningEffort) != c[1] || deref(storedJobs(t, s)[0].ReasoningEffort) != c[1] {
			t.Fatalf("%q stored %v %v", c[0], job, err)
		}
	}
	for _, garbage := range []string{"turbo", "medium-plus"} {
		s, _ := newTestStore(t)
		if _, err := reasoningJob(t, s, garbage); err == nil || !strings.Contains(err.Error(), garbage) || len(storedJobs(t, s)) != 0 {
			t.Fatalf("%q accepted %v", garbage, err)
		}
	}
	for _, empty := range []any{nil, ""} {
		s, _ := newTestStore(t)
		if job, err := reasoningJob(t, s, empty); err != nil || job.ReasoningEffort != nil {
			t.Fatalf("%v stored %v", empty, job.ReasoningEffort)
		}
	}
	s, _ := newTestStore(t)
	job, _ := reasoningJob(t, s, nil)
	if up, err := s.updateJob(job.ID, map[string]any{"reasoning_effort": "xhigh"}); err != nil || deref(up.ReasoningEffort) != "xhigh" || deref(storedJobs(t, s)[0].ReasoningEffort) != "xhigh" {
		t.Fatalf("update %v %v", up, err)
	}
	pinned, _ := reasoningJob(t, s, "high")
	if up, err := s.updateJob(pinned.ID, map[string]any{"reasoning_effort": ""}); err != nil || up.ReasoningEffort != nil {
		t.Fatalf("clear %v %v", up, err)
	}
	kept, _ := reasoningJob(t, s, "high")
	if _, err := s.updateJob(kept.ID, map[string]any{"reasoning_effort": "warp9"}); err == nil {
		t.Fatal("garbage update accepted")
	}
	if got := mustJob(t, s, kept.ID).ReasoningEffort; deref(got) != "high" {
		t.Fatalf("stored %v", got)
	}
}

var reasoningCfg = config.Tree{
	"model": map[string]any{"default": "anthropic/claude-opus-4.5"},
	"agent": map[string]any{"reasoning_effort": "low", "reasoning_overrides": map[string]any{"anthropic/claude-opus-4.5": "xhigh"}},
}

func TestSchedulerJobReasoningPrecedence(t *testing.T) {
	s, buf := newTestStore(t)
	if got := s.resolveJobReasoningConfig(&Job{ReasoningEffort: strPtr("high")}, reasoningCfg, "anthropic/claude-opus-4.5"); !reflect.DeepEqual(got, map[string]any{"enabled": true, "effort": "high"}) {
		t.Fatalf("job effort %v", got)
	}
	if got := s.resolveJobReasoningConfig(&Job{ReasoningEffort: strPtr("none")}, reasoningCfg, "anthropic/claude-opus-4.5"); !reflect.DeepEqual(got, map[string]any{"enabled": false}) {
		t.Fatalf("job none %v", got)
	}
	for _, model := range []string{"anthropic/claude-opus-4.5", "gpt-5", ""} {
		want := config.ResolveReasoningConfig(map[string]any(reasoningCfg), model)
		if got := s.resolveJobReasoningConfig(&Job{}, reasoningCfg, model); !reflect.DeepEqual(got, want) {
			t.Fatalf("absent %q %v want %v", model, got, want)
		}
	}
	if got := s.resolveJobReasoningConfig(&Job{ID: "abc123", ReasoningEffort: strPtr("turbo")}, reasoningCfg, "gpt-5"); !reflect.DeepEqual(got, config.ResolveReasoningConfig(map[string]any(reasoningCfg), "gpt-5")) || !strings.Contains(buf.String(), "turbo") {
		t.Fatalf("garbage %v log %q", got, buf.String())
	}
	for _, model := range []string{"gpt-5.6-sol", "x-ai/grok-4", "gemini-3-pro", ""} {
		if got := s.resolveJobReasoningConfig(&Job{ReasoningEffort: strPtr("ultra")}, reasoningCfg, model); !reflect.DeepEqual(got, map[string]any{"enabled": true, "effort": "ultra"}) {
			t.Fatalf("ultra %q %v", model, got)
		}
	}
}

func TestCronjobToolReasoningEffort(t *testing.T) {
	s, _ := newTestStore(t)
	reasoningJob(t, s, "high")
	listed := runCronjob(t, s, &cronjobArgs{Action: "list"})["jobs"].([]any)[0].(map[string]any)
	if listed["reasoning_effort"] != "high" {
		t.Fatalf("listed %v", listed)
	}
	plain, _ := newTestStore(t)
	reasoningJob(t, plain, nil)
	listed = runCronjob(t, plain, &cronjobArgs{Action: "list"})["jobs"].([]any)[0].(map[string]any)
	if _, ok := listed["reasoning_effort"]; ok {
		t.Fatalf("listed unset %v", listed)
	}
	fresh, _ := newTestStore(t)
	out := dispatchCronjob(t, fresh, map[string]any{"action": "create", "prompt": "daily digest", "schedule": "every 1h", "reasoning_effort": "max"})
	if out["success"] != true || storedJobs(t, fresh)[0].ReasoningEffort != nil {
		t.Fatalf("dispatch %v", out)
	}
}

func TestGuardJobCredentialExfilRealValidator(t *testing.T) {
	s, _ := newTestStore(t)
	saved := cronProviderResolver
	t.Cleanup(func() { cronProviderResolver = saved })
	cronProviderResolver.hasNamed = func(context.Context, string, string) bool { return false }
	cronProviderResolver.registryURL = func(id string) (string, bool) {
		if id == "anthropic" {
			return "https://api.anthropic.com", true
		}
		return "", false
	}
	err := s.guardJobCredentialExfil(&Job{ID: "j1", Provider: strPtr("anthropic"), BaseURL: strPtr("https://evil.example/v1")})
	if err == nil || !strings.Contains(err.Error(), "blocked for safety") {
		t.Fatalf("registry off host %v", err)
	}
	for _, job := range []*Job{
		{ID: "j4", Provider: strPtr("custom"), BaseURL: strPtr("https://anything.example/v1")},
		{ID: "j5", Provider: strPtr("anthropic")},
		{ID: "j6"},
	} {
		if err := s.guardJobCredentialExfil(job); err != nil {
			t.Fatalf("%s blocked %v", job.ID, err)
		}
	}
	prevValidator := cronBaseURLValidator
	t.Cleanup(func() { cronBaseURLValidator = prevValidator })
	cronBaseURLValidator = func(*Store, *string, *string) (string, error) { return "", errors.New("validator blew up") }
	err = s.guardJobCredentialExfil(&Job{ID: "j7", Provider: strPtr("custom:legit"), BaseURL: strPtr("https://evil.example/v1")})
	if err == nil || !strings.Contains(err.Error(), "blocked for safety") {
		t.Fatalf("validator failure %v", err)
	}
}
