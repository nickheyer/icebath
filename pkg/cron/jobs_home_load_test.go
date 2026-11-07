package cron

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadJobsForHomeReadsThatHomesStoreInLoadOrder(t *testing.T) {
	s, _ := newTestStore(t)
	first, err := s.createJob(CreateJobOptions{Prompt: strPtr("one"), Schedule: "every 1h", Name: strPtr("first")})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.createJob(CreateJobOptions{Prompt: strPtr("two"), Schedule: "every 2h"})
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(filepath.Join(other, "cron"), 0o700); err != nil {
		t.Fatal(err)
	}
	jobs, err := LoadJobsForHome(s.home)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].ID != first.ID || jobs[1].ID != second.ID {
		t.Fatalf("jobs = %+v", jobs)
	}
	if jobs[0].Name != "first" {
		t.Fatalf("name = %q", jobs[0].Name)
	}
	empty, err := LoadJobsForHome(other)
	if err != nil || len(empty) != 0 {
		t.Fatalf("other home jobs = %v, %v", empty, err)
	}
}
