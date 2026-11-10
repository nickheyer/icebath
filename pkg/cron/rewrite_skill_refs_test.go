package cron

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func skillJob(t *testing.T, s *Store, o CreateJobOptions) *Job {
	t.Helper()
	if o.Prompt == nil {
		o.Prompt = prompt("")
	}
	o.Schedule = "every 1h"
	return mustCreate(t, s, o)
}

func rewriteRefs(t *testing.T, s *Store, consolidated *pyjson.Object, pruned ...string) SkillRefRewriteReport {
	t.Helper()
	report, err := s.rewriteSkillRefs(context.Background(), consolidated, pruned)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func storedSkills(t *testing.T, s *Store, id string) (any, any) {
	t.Helper()
	for _, row := range rawStoredRows(t, s) {
		if row["id"] == id {
			skill, present := row["skill"]
			if !present {
				t.Fatalf("job %s stores no skill key", id)
			}
			return row["skills"], skill
		}
	}
	t.Fatalf("job %s is not stored", id)
	return nil, nil
}

func wantSkills(t *testing.T, s *Store, id string, want ...string) {
	t.Helper()
	list := []any{}
	for _, name := range want {
		list = append(list, name)
	}
	if got, _ := storedSkills(t, s, id); !reflect.DeepEqual(got, list) {
		t.Fatalf("skills %v want %v", got, list)
	}
	if loaded := mustGet(t, s, id); !reflect.DeepEqual(append([]string{}, loaded.Skills...), append([]string{}, want...)) {
		t.Fatalf("loaded skills %v want %v", loaded.Skills, want)
	}
}

func TestRewriteSkillRefsConsolidationSingleSkillReplaced(t *testing.T) {
	s, _ := newTestStore(t)
	job := skillJob(t, s, CreateJobOptions{Skills: []any{"legacy-skill"}})
	report := rewriteRefs(t, s, pyjson.ObjectOf("legacy-skill", "umbrella-skill"))
	if report.JobsUpdated != 1 {
		t.Fatalf("report %+v", report)
	}
	wantSkills(t, s, job.ID, "umbrella-skill")
	if _, skill := storedSkills(t, s, job.ID); skill != "umbrella-skill" {
		t.Fatalf("skill %v", skill)
	}
}

func TestRewriteSkillRefsConsolidationMultipleSkillsOneConsolidated(t *testing.T) {
	s, _ := newTestStore(t)
	job := skillJob(t, s, CreateJobOptions{Skills: []any{"keep-a", "legacy", "keep-b"}})
	rewriteRefs(t, s, pyjson.ObjectOf("legacy", "umbrella"))
	wantSkills(t, s, job.ID, "keep-a", "umbrella", "keep-b")
}

func TestRewriteSkillRefsConsolidationUmbrellaAlreadyInListDedupes(t *testing.T) {
	s, _ := newTestStore(t)
	job := skillJob(t, s, CreateJobOptions{Skills: []any{"umbrella", "legacy"}})
	rewriteRefs(t, s, pyjson.ObjectOf("legacy", "umbrella"))
	wantSkills(t, s, job.ID, "umbrella")
}

func TestRewriteSkillRefsConsolidationRewriteReportRecordsMapping(t *testing.T) {
	s, _ := newTestStore(t)
	job := skillJob(t, s, CreateJobOptions{Skills: []any{"a", "b"}, Name: strPtr("my-job")})
	report := rewriteRefs(t, s, pyjson.ObjectOf("a", "umbrella-a", "b", "umbrella-b"))
	if len(report.Rewrites) != 1 {
		t.Fatalf("rewrites %+v", report.Rewrites)
	}
	entry := report.Rewrites[0]
	mapped := map[string]any{}
	for _, key := range entry.Mapped.Keys {
		mapped[key] = entry.Mapped.Vals[key]
	}
	if entry.JobID != job.ID || entry.JobName != "my-job" || !reflect.DeepEqual(entry.Before, []string{"a", "b"}) ||
		!reflect.DeepEqual(entry.After, []string{"umbrella-a", "umbrella-b"}) || !reflect.DeepEqual(mapped, map[string]any{"a": "umbrella-a", "b": "umbrella-b"}) ||
		len(entry.Dropped) != 0 {
		t.Fatalf("entry %+v mapped %v", entry, mapped)
	}
}

func TestRewriteSkillRefsPruningPrunedSkillDropped(t *testing.T) {
	s, _ := newTestStore(t)
	job := skillJob(t, s, CreateJobOptions{Skills: []any{"keep", "stale"}})
	report := rewriteRefs(t, s, pyjson.ObjectOf(), "stale")
	if report.JobsUpdated != 1 {
		t.Fatalf("report %+v", report)
	}
	wantSkills(t, s, job.ID, "keep")
	if _, skill := storedSkills(t, s, job.ID); skill != "keep" {
		t.Fatalf("skill %v", skill)
	}
}

func TestRewriteSkillRefsPruningAllSkillsPrunedLeavesEmptyList(t *testing.T) {
	s, _ := newTestStore(t)
	job := skillJob(t, s, CreateJobOptions{Skills: []any{"gone"}})
	rewriteRefs(t, s, pyjson.ObjectOf(), "gone")
	wantSkills(t, s, job.ID)
	if _, skill := storedSkills(t, s, job.ID); skill != nil {
		t.Fatalf("skill %v", skill)
	}
}

func TestRewriteSkillRefsMixedMixedConsolidationAndPruning(t *testing.T) {
	s, _ := newTestStore(t)
	job := skillJob(t, s, CreateJobOptions{Skills: []any{"keep", "legacy", "stale"}})
	rewriteRefs(t, s, pyjson.ObjectOf("legacy", "umbrella"), "stale")
	wantSkills(t, s, job.ID, "keep", "umbrella")
}

func TestRewriteSkillRefsMixedSkillInBothMapsWinsAsConsolidated(t *testing.T) {
	s, _ := newTestStore(t)
	job := skillJob(t, s, CreateJobOptions{Skills: []any{"ambiguous"}})
	rewriteRefs(t, s, pyjson.ObjectOf("ambiguous", "umbrella"), "ambiguous")
	wantSkills(t, s, job.ID, "umbrella")
}

func TestRewriteSkillRefsMultipleJobsOnlyAffectedJobsReported(t *testing.T) {
	s, _ := newTestStore(t)
	first := skillJob(t, s, CreateJobOptions{Skills: []any{"legacy"}})
	second := skillJob(t, s, CreateJobOptions{Skills: []any{"untouched"}})
	third := skillJob(t, s, CreateJobOptions{Prompt: prompt("no skills here"), Skills: []any{}})
	report := rewriteRefs(t, s, pyjson.ObjectOf("legacy", "umbrella"))
	if report.JobsUpdated != 1 || report.JobsScanned != 3 || len(report.Rewrites) != 1 || report.Rewrites[0].JobID != first.ID {
		t.Fatalf("report %+v", report)
	}
	wantSkills(t, s, second.ID, "untouched")
	wantSkills(t, s, third.ID)
}

func TestRewriteSkillRefsMultipleJobsLegacySkillFieldAlsoRewritten(t *testing.T) {
	s, _ := newTestStore(t)
	job := skillJob(t, s, CreateJobOptions{Skill: strPtr("legacy")})
	rewriteRefs(t, s, pyjson.ObjectOf("legacy", "umbrella"))
	wantSkills(t, s, job.ID, "umbrella")
	if _, skill := storedSkills(t, s, job.ID); skill != "umbrella" {
		t.Fatalf("skill %v", skill)
	}
}

func TestRewriteSkillRefsPersistenceNoopDoesNotRewriteFile(t *testing.T) {
	s, _ := newTestStore(t)
	skillJob(t, s, CreateJobOptions{Skills: []any{"keep"}})
	before, err := os.Stat(s.JobsFile())
	if err != nil {
		t.Fatal(err)
	}
	report := rewriteRefs(t, s, pyjson.ObjectOf("unrelated", "umbrella"), "other")
	after, err := os.Stat(s.JobsFile())
	if err != nil {
		t.Fatal(err)
	}
	if report.JobsUpdated != 0 || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("report %+v mtime %v then %v", report, before.ModTime(), after.ModTime())
	}
}
