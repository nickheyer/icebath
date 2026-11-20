package cron

import (
	"context"
	"testing"
)

func TestCreatePersistsExplicitEmptyAllowlistAndSurvivesReload(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("daily digest"), Schedule: "every 1h", EnabledToolsets: []any{}})
	if job.EnabledToolsets == nil || len(job.EnabledToolsets) != 0 {
		t.Fatalf("created enabled_toolsets %#v", job.EnabledToolsets)
	}
	reloaded, err := openCronStore(s.home, nil).getJob(context.Background(), job.ID)
	if err != nil || reloaded == nil || reloaded.EnabledToolsets == nil || len(reloaded.EnabledToolsets) != 0 {
		t.Fatalf("reloaded enabled_toolsets %#v %v", reloaded, err)
	}
}

func TestUpdatePathPreservesExplicitEmptyAllowlist(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("daily digest"), Schedule: "every 1h"})
	if job.EnabledToolsets != nil {
		t.Fatalf("absent allowlist %#v", job.EnabledToolsets)
	}
	updated := mustUpdate(t, s, job.ID, map[string]any{"enabled_toolsets": []any{}})
	if updated.EnabledToolsets == nil || len(updated.EnabledToolsets) != 0 {
		t.Fatalf("updated enabled_toolsets %#v", updated.EnabledToolsets)
	}
}
