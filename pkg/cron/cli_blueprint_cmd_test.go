package cron

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
)

type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func TestBlueprintCommandFillCreatesJob(t *testing.T) {
	s, _ := newTestStore(t)
	res, err := HandleBlueprintCommand(context.Background(), s.home, "morning-brief time=07:30 deliver=telegram", nil, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "Scheduled") || res.AgentSeed != nil {
		t.Fatalf("%+v", res)
	}
	jobs, err := s.LoadJobs(context.Background())
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%v err=%v", jobs, err)
	}
	shown := jobs[0].ScheduleDisplay
	if shown == "" {
		shown = jobs[0].Schedule.Display
	}
	if shown != "30 7 * * *" || jobs[0].Deliver == nil || *jobs[0].Deliver != "telegram" {
		t.Fatalf("schedule %q deliver %v", shown, jobs[0].Deliver)
	}
}
