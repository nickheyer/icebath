package cron

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/skills"
)

func addTestSuggestion(t *testing.T, s *Store, key, title, source, schedule string) *pyjson.Object {
	t.Helper()
	spec := pyjson.NewObject()
	spec.Set("prompt", "do it")
	spec.Set("schedule", schedule)
	spec.Set("name", title)
	spec.Set("deliver", "origin")
	rec, err := s.AddSuggestion(title, "desc", source, spec, key)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func pendingCount(t *testing.T, s *Store) int {
	t.Helper()
	p, err := s.ListPendingSuggestions()
	if err != nil {
		t.Fatal(err)
	}
	return len(p)
}

func TestSuggestionFileOverrideAndProfileRouting(t *testing.T) {
	root := t.TempDir()
	explicit := filepath.Join(root, "explicit", "suggestions.json")
	profileA, profileB := filepath.Join(root, "profile-a"), filepath.Join(root, "profile-b")
	suggestionsFileOverride = explicit
	ctx := home.WithOverride(context.Background(), profileA)
	if rec, err := skills.RegisterBlueprintSuggestion(ctx, &skills.BlueprintSpec{SkillName: "explicit-file", Schedule: "0 9 * * *"}); err != nil || rec == nil {
		t.Fatal(rec, err)
	}
	suggestionsFileOverride = ""
	if _, err := os.Stat(explicit); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(profileA, "cron", "suggestions.json")); err == nil {
		t.Fatal("explicit override leaked into the profile home")
	}
	rec, err := skills.RegisterBlueprintSuggestion(home.WithOverride(context.Background(), profileB), &skills.BlueprintSpec{SkillName: "morning-brief", Schedule: "0 8 * * *", Deliver: "telegram"})
	if err != nil || rec == nil {
		t.Fatal(rec, err)
	}
	spec := rec["job_spec"].(*pyjson.Object)
	if rec["source"] != "blueprint" || spec.Vals["schedule"] != "0 8 * * *" || len(spec.Vals["skills"].([]any)) != 1 || spec.Vals["skills"].([]any)[0] != "morning-brief" {
		t.Fatal(rec)
	}
	if _, err := os.Stat(filepath.Join(profileB, "cron", "suggestions.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(profileA, "cron", "suggestions.json")); err == nil {
		t.Fatal("profile a written")
	}
}

func TestSuggestionStoreLifecycle(t *testing.T) {
	s, _ := newTestStore(t)
	if rec := addTestSuggestion(t, s, "k1", "Test", "catalog", "0 9 * * *"); rec == nil || rec.Vals["status"] != "pending" {
		t.Fatal(rec)
	}
	p, _ := s.ListPendingSuggestions()
	if len(p) != 1 || p[0].(*pyjson.Object).Vals["title"] != "Test" {
		t.Fatal(p)
	}
	if addTestSuggestion(t, s, "k1", "Test", "catalog", "0 9 * * *") != nil || pendingCount(t, s) != 1 {
		t.Fatal("duplicate pending key accepted")
	}
	if ok, err := s.DismissSuggestion("1"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if pendingCount(t, s) != 0 || addTestSuggestion(t, s, "k1", "Test", "catalog", "0 9 * * *") != nil {
		t.Fatal("dismissed key offered again")
	}
	if _, err := s.AddSuggestion("x", "d", "bogus", pyjson.NewObject(), "k"); err == nil || err.Error() != "unknown suggestion source: 'bogus'" {
		t.Fatal(err)
	}
	if _, err := s.AddSuggestion("  ", "d", "catalog", pyjson.NewObject(), "k"); err == nil || err.Error() != "title and dedup_key are required" {
		t.Fatal(err)
	}
	rec := addTestSuggestion(t, s, "usage:weekly-summary", "Weekly project summary", "usage", "0 17 * * 5")
	if rec.Vals["source"] != "usage" || rec.Vals["status"] != "pending" || rec.Vals["job_spec"].(*pyjson.Object).Vals["schedule"] != "0 17 * * 5" {
		t.Fatal(rec)
	}
	p, _ = s.ListPendingSuggestions()
	if p[0].(*pyjson.Object).Vals["dedup_key"] != "usage:weekly-summary" {
		t.Fatal(p)
	}
	if info, err := os.Stat(s.suggestionsFile()); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal(info, err)
	}
}

func TestSuggestionPendingCap(t *testing.T) {
	s, _ := newTestStore(t)
	for i := 0; i < MaxPendingSuggestions; i++ {
		if addTestSuggestion(t, s, "k"+string(rune('0'+i)), "T", "catalog", "0 9 * * *") == nil {
			t.Fatal(i)
		}
	}
	if addTestSuggestion(t, s, "over", "T", "catalog", "0 9 * * *") != nil || pendingCount(t, s) != MaxPendingSuggestions {
		t.Fatal("cap not enforced")
	}
}

func TestAcceptSuggestionCreatesJobAndMarksAccepted(t *testing.T) {
	s, _ := newTestStore(t)
	testScheduler.registered = nil
	addTestSuggestion(t, s, "acc", "My Job", "catalog", "0 9 * * *")
	job, err := s.AcceptSuggestion("1", map[string]any{"platform": "telegram", "chat_id": "5"})
	if err != nil || job == nil {
		t.Fatal(job, err)
	}
	if job.Schedule.Expr != "0 9 * * *" || job.Origin["platform"] != "telegram" || job.Origin["chat_id"] != "5" || job.Name != "My Job" {
		t.Fatal(job.Schedule, job.Origin, job.Name)
	}
	if len(testScheduler.registered) != 1 || testScheduler.registered[0] != job.ID {
		t.Fatal(testScheduler.registered)
	}
	if pendingCount(t, s) != 0 {
		t.Fatal("still pending")
	}
	if again, err := s.AcceptSuggestion("acc", nil); again != nil || err != nil {
		t.Fatal(again, err)
	}
}

func TestSuggestionRegistrationFailureMarksAccepted(t *testing.T) {
	s, _ := newTestStore(t)
	rec := addTestSuggestion(t, s, "registration-failed", "My Job", "catalog", "0 9 * * *")
	failure := &CronSchedulerRegistrationError{Job: &Job{ID: "job123", Name: "My Job"}, Cause: errors.New("private detail")}
	prev := acceptCreateJob
	acceptCreateJob = func(*Store, *pyjson.Object) (*Job, error) { return nil, failure }
	t.Cleanup(func() { acceptCreateJob = prev })
	_, err := s.AcceptSuggestion(rec.Vals["id"].(string), nil)
	var regErr *CronSchedulerRegistrationError
	if !errors.As(err, &regErr) {
		t.Fatal(err)
	}
	if pendingCount(t, s) != 0 {
		t.Fatal("still pending")
	}
	if again, err := s.AcceptSuggestion(rec.Vals["id"].(string), nil); again != nil || err != nil {
		t.Fatal(again, err)
	}
}

func TestSchedulerRegistrationFailureKeepsSavedJob(t *testing.T) {
	s, _ := newTestStore(t)
	testScheduler.registerErr = errors.New("private detail")
	t.Cleanup(func() { testScheduler.registerErr = nil })
	spec := pyjson.NewObject()
	spec.Set("prompt", "p")
	spec.Set("schedule", "every 1h")
	spec.Set("name", "Reg")
	_, err := s.createJobWithSchedulerRegistration(spec)
	var regErr *CronSchedulerRegistrationError
	if !errors.As(err, &regErr) {
		t.Fatal(err)
	}
	if !strings.Contains(err.Error(), "first scheduler registration failed (RuntimeError). Do not create a duplicate.") || strings.Contains(err.Error(), "private detail") {
		t.Fatal(err)
	}
	if !strings.HasPrefix(regErr.UserMessage(), "Saved cron job 'Reg', but couldn't register") {
		t.Fatal(regErr.UserMessage())
	}
	d := regErr.ToDict()
	if d.Vals["job_id"] != regErr.Job.ID || d.Vals["job_saved"] != true || d.Vals["scheduler_registered"] != false || d.Vals["retry_create"] != false {
		t.Fatal(d)
	}
	jobs, _ := s.LoadJobs()
	if len(jobs) != 1 {
		t.Fatal(len(jobs))
	}
	bad := pyjson.NewObject()
	bad.Set("prompt", "p")
	bad.Set("bogus", 1)
	if _, err := s.createJobWithSchedulerRegistration(bad); err == nil || err.Error() != "TypeError: create_job() got an unexpected keyword argument 'bogus'" {
		t.Fatal(err)
	}
	if _, err := s.createJobWithSchedulerRegistration(pyjson.NewObject()); err == nil || err.Error() != "TypeError: create_job() missing 2 required positional arguments: 'prompt' and 'schedule'" {
		t.Fatal(err)
	}
}

func TestGetSuggestionByIDIndexTitleAndClearResolved(t *testing.T) {
	s, _ := newTestStore(t)
	rec := addTestSuggestion(t, s, "byref", "Findable", "catalog", "0 9 * * *")
	for _, ref := range []string{rec.Vals["id"].(string), "1", "findable"} {
		if got, err := s.GetSuggestion(ref); err != nil || got == nil || got.Vals["id"] != rec.Vals["id"] {
			t.Fatal(ref, got, err)
		}
	}
	if got, err := s.GetSuggestion("nope"); got != nil || err != nil {
		t.Fatal(got, err)
	}
	addTestSuggestion(t, s, "b", "B", "catalog", "0 9 * * *")
	if ok, _ := s.DismissSuggestion("2"); !ok {
		t.Fatal("dismiss b")
	}
	prev := acceptCreateJob
	acceptCreateJob = func(*Store, *pyjson.Object) (*Job, error) { return &Job{ID: "j"}, nil }
	t.Cleanup(func() { acceptCreateJob = prev })
	if job, err := s.AcceptSuggestion("1", nil); job == nil || err != nil {
		t.Fatal(job, err)
	}
	if removed, err := s.ClearResolvedSuggestions(); removed != 1 || err != nil {
		t.Fatal(removed, err)
	}
	if addTestSuggestion(t, s, "b", "B", "catalog", "0 9 * * *") != nil {
		t.Fatal("dismissed latch lost on clear")
	}
}

func TestSuggestionsFailedWriteKeepsOldBytesAndNoTemp(t *testing.T) {
	s, _ := newTestStore(t)
	dir := filepath.Join(t.TempDir(), "explicit")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := strings.Repeat("s", 245) + ".json"
	target := filepath.Join(dir, name)
	if err := os.WriteFile(target, []byte(`{"old": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	suggestionsFileOverride = target
	t.Cleanup(func() { suggestionsFileOverride = "" })
	rec := pyjson.NewObject()
	rec.Set("id", int64(1))
	if err := s.saveSuggestionsRaw([]any{rec}); err == nil {
		t.Fatal("write whose temp name exceeds the name limit succeeded")
	}
	if b, _ := os.ReadFile(target); string(b) != `{"old": true}` {
		t.Fatal(string(b))
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != name {
			t.Fatal("leftover", e.Name())
		}
	}
}
