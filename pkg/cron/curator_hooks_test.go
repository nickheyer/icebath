package cron

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/memory"
)

func TestCuratorHooksReadRewriteAndUpdateJobs(t *testing.T) {
	s, _ := newTestStore(t)
	keep := mustCreate(t, s, CreateJobOptions{Prompt: prompt("A"), Schedule: "1h", Name: prompt("keep"), Skills: []any{"old-skill", "gone-skill"}})
	drop := mustCreate(t, s, CreateJobOptions{Prompt: prompt("B"), Schedule: "1h", Name: prompt("drop")})
	names, err := memory.CuratorHooks.ReferencedSkillNames(s.home)
	if err != nil || !names["old-skill"] || !names["gone-skill"] || len(names) != 2 {
		t.Fatal(names, err)
	}
	consolidated := pyjson.NewObject()
	consolidated.Set("old-skill", "umbrella")
	report, err := memory.CuratorHooks.RewriteSkillRefs(s.home, consolidated, []string{"gone-skill"})
	if err != nil || report.JobsUpdated != 1 || report.JobsScanned != 2 || len(report.Rewrites) != 1 || report.Rewrites[0].JobID != keep.ID {
		t.Fatal(report, err)
	}
	if after := report.Rewrites[0].After; len(after) != 1 || after[0] != "umbrella" {
		t.Fatal(after)
	}
	if err := memory.CuratorHooks.UpdateJobs(s.home, func(jobs []any) (bool, error) {
		if len(jobs) != 2 {
			t.Fatal(len(jobs))
		}
		jobs[0].(*pyjson.Object).Set("prompt", "restored prompt")
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.getJob(keep.ID); got.Prompt != "A" {
		t.Fatal("unchanged update saved", got.Prompt)
	}
	if err := memory.CuratorHooks.UpdateJobs(s.home, func(jobs []any) (bool, error) {
		for _, item := range jobs {
			if obj := item.(*pyjson.Object); obj.Vals["id"] == keep.ID {
				obj.Set("prompt", "restored prompt")
			}
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if other, _ := s.getJob(drop.ID); other == nil || other.Prompt != "B" {
		t.Fatal("untouched job lost")
	}
	got, _ := s.getJob(keep.ID)
	if got.Prompt != "restored prompt" || len(got.Skills) != 1 || got.Skills[0] != "umbrella" {
		t.Fatal(got.Prompt, got.Skills)
	}
}
