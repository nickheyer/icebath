package cron

import (
	"bufio"
	"bytes"
	"context"
	"maps"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLiveFirerJournalSurvivesUntilItsExecutionCanSettle(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("report"), Schedule: "every 1h", Deliver: prompt("local")})
	child := ledgerChildCommand(s.Home(), "", "create-running-hold", job.ID)
	var stderr bytes.Buffer
	child.Stderr = &stderr
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	var waitErr error
	exited := make(chan struct{})
	go func() { waitErr = child.Wait(); close(exited) }()
	defer func() {
		select {
		case <-exited:
		default:
			_ = child.Process.Kill()
			<-exited
		}
	}()
	fire := strings.TrimSpace(line)
	if err != nil || fire == "" {
		t.Fatalf("fire %q err %v", fire, err)
	}
	select {
	case <-exited:
		t.Fatalf("firer exited early: %v %s", waitErr, stderr.String())
	default:
	}
	if row := executionRow(t, s, fire); row["status"] != "running" {
		t.Fatalf("status = %v", row["status"])
	}
	params := map[string]any{"job_id": job.ID, "request_id": fire, "extra_prompt": nil}
	journal := writeFireJournal(t, s, job.ID, fire)
	receiptPeer(t, func(method string, got map[string]any) (map[string]any, error) {
		if method != "cron.recover" || !maps.Equal(got, params) {
			t.Errorf("rpc %s %v", method, got)
		}
		return map[string]any{"status": "terminal", "job": job, "result": []any{true, "report", "answer", nil}}, nil
	})
	for range 2 {
		mustReconcile(t, s)
	}
	if !journalExists(journal) {
		t.Fatal("the live firer left running execution without its recovery receipt")
	}
	if row := executionRow(t, s, fire); row["status"] != "running" {
		t.Fatalf("status = %v", row["status"])
	}
	dir, err := s.jobOutputDir(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if outputs, _ := filepath.Glob(filepath.Join(dir, "*.md")); len(outputs) != 0 {
		t.Fatalf("outputs = %v", outputs)
	}
	if last := mustGet(t, s, job.ID).LastRunAt; last != nil {
		t.Fatalf("last_run_at = %v", *last)
	}
	stdin.Close()
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("firer did not exit")
	}
	if err := s.reconcilePendingJournals(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if journalExists(journal) {
		t.Fatal("journal kept")
	}
	if row := executionRow(t, s, fire); row["status"] != "completed" {
		t.Fatalf("status = %v", row["status"])
	}
	if completed := mustGet(t, s, job.ID).Repeat.Completed; completed != 1 {
		t.Fatalf("completed = %d", completed)
	}
}
