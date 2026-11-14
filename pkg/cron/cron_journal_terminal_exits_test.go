package cron

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestFirerBookedExitIsNotReplayedByReceiptRecovery(t *testing.T) {
	exits := []struct {
		name string
		exit func(t *testing.T, s *Store, job *Job, fire string)
	}{
		{"ownership-lost", func(t *testing.T, s *Store, job *Job, fire string) {
			if err := s.recordFireOwnershipLost(context.Background(), job, nil, fire); err != nil {
				t.Fatal(err)
			}
		}},
		{"shutdown-interrupted", func(t *testing.T, s *Store, job *Job, fire string) {
			if err := s.finishInterruptedRun(context.Background(), job, fire, nil); err != nil {
				t.Fatal(err)
			}
		}},
		{"crashed-tail", func(t *testing.T, s *Store, job *Job, fire string) {
			run := job.Clone()
			run.ExecutionID = strPtr(fire)
			if ok, err := s.runOneJobBody(context.Background(), run, runOneJobOptions{}, nil, nil, nil, nil); err != nil || ok {
				t.Fatalf("body = %v, %v", ok, err)
			}
		}},
	}
	for _, tc := range exits {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestStore(t)
			ctx := context.Background()
			var calls []string
			var job *Job
			receiptPeer(t, func(method string, _ map[string]any) (map[string]any, error) {
				calls = append(calls, method)
				return map[string]any{"status": "terminal", "job": job, "result": []any{true, "document", "answer", nil}}, nil
			})
			prev := runJobSeam
			t.Cleanup(func() { runJobSeam = prev })
			runJobSeam = func(context.Context, *Store, *Job, runJobOptions) (jobRunOutcome, error) {
				return jobRunOutcome{}, errors.New("the firing tail crashed after the owner finished")
			}
			job = mustCreate(t, s, CreateJobOptions{Prompt: prompt("report"), Schedule: "every 1h", Deliver: prompt("local")})
			ledger := openExecutionsLedger(s)
			created, err := ledger.Create(ctx, job.ID, "test", nil)
			if err != nil {
				t.Fatal(err)
			}
			fire := created["id"].(string)
			if tc.name != "crashed-tail" {
				if _, err := ledger.MarkRunning(ctx, fire); err != nil {
					t.Fatal(err)
				}
			}
			journal := s.journalPath(job.ID, fire)
			writeTestFile(t, journal, `{"params": {"job_id": "`+job.ID+`", "request_id": "`+fire+`", "extra_prompt": null}, "receipt": {"session_id": "s", "admission_id": "a"}}`)
			tc.exit(t, s, mustGet(t, s, job.ID), fire)
			if row := executionRow(t, s, fire); row["status"] != "failed" {
				t.Fatalf("status = %v", row["status"])
			}
			booked := mustGet(t, s, job.ID)
			mustReconcile(t, s)
			after := mustGet(t, s, job.ID)
			if len(calls) != 0 {
				t.Fatalf("calls = %v", calls)
			}
			dir, err := s.jobOutputDir(job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if outputs, _ := filepath.Glob(filepath.Join(dir, "*.md")); len(outputs) != 0 {
				t.Fatalf("outputs = %v", outputs)
			}
			if deref(after.LastRunAt) != deref(booked.LastRunAt) || deref(after.LastStatus) != deref(booked.LastStatus) || *after.Repeat != *booked.Repeat {
				t.Fatalf("after %q %q %+v booked %q %q %+v", deref(after.LastRunAt), deref(after.LastStatus), *after.Repeat,
					deref(booked.LastRunAt), deref(booked.LastStatus), *booked.Repeat)
			}
			if row := executionRow(t, s, fire); row["status"] != "failed" {
				t.Fatalf("status after recovery = %v", row["status"])
			}
			if journalExists(journal) {
				t.Fatal("journal kept")
			}
		})
	}
}
