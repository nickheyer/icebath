package cron

import (
	"reflect"
	"testing"
)

func TestJobContextFromFieldCreateJobWithContextFromString(t *testing.T) {
	s, _ := newTestStore(t)
	jobA := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Find news"), Schedule: "every 1h"})
	jobB := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Summarize findings"), Schedule: "every 2h", ContextFrom: jobA.ID})
	if !reflect.DeepEqual(jobB.ContextFrom, []string{jobA.ID}) {
		t.Fatalf("created context_from %v", jobB.ContextFrom)
	}
	if loaded := mustGet(t, s, jobB.ID); !reflect.DeepEqual(loaded.ContextFrom, []string{jobA.ID}) {
		t.Fatalf("loaded context_from %v", loaded.ContextFrom)
	}
}
