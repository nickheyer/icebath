package cron

import (
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestRawJobsUpdateKeepsAbsentSkillKeysAbsent(t *testing.T) {
	s, _ := newTestStore(t)
	doc := `{"jobs": [{"id": "job-1", "name": "one", "skills": ["umbrella"], "schedule": "every 1h"}, {"id": "job-2", "name": "two", "skill": "legacy-single", "schedule": "every 1h"}], "updated_at": "2026-05-01T00:00:00Z"}`
	if err := os.WriteFile(s.jobsFile, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	err := s.updateRawJobs(func(jobs []any) (bool, error) {
		one, two := jobs[0].(*pyjson.Object), jobs[1].(*pyjson.Object)
		if _, ok := one.Get("skill"); ok {
			t.Errorf("job-1 handed a skill key %v", one.Keys)
		}
		if _, ok := two.Get("skills"); ok {
			t.Errorf("job-2 handed a skills key %v", two.Keys)
		}
		one.Set("skills", []any{"narrow-a", "narrow-b"})
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.jobsFile)
	if err != nil {
		t.Fatal(err)
	}
	v, err := pyjson.LoadsBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	jobs, _ := field(v.(*pyjson.Object), "jobs", nil).([]any)
	if len(jobs) != 2 {
		t.Fatalf("jobs %s", raw)
	}
	one, two := jobs[0].(*pyjson.Object), jobs[1].(*pyjson.Object)
	if _, ok := one.Get("skill"); ok {
		t.Fatalf("restore wrote a skill key on job-1: %s", raw)
	}
	if _, ok := two.Get("skills"); ok {
		t.Fatalf("restore wrote a skills key on job-2: %s", raw)
	}
	if got, _ := pyjson.Dumps(field(one, "skills", nil)); got != `["narrow-a", "narrow-b"]` || field(two, "skill", nil) != "legacy-single" {
		t.Fatalf("restored records %s", raw)
	}
}
