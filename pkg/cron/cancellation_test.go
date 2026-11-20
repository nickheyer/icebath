package cron

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

type fenceStub bool

func (f fenceStub) operatorCancelled() bool { return bool(f) }

func TestCancelMarkersFirstReasonWinsAndIdsAreValidated(t *testing.T) {
	s := newClockStore(t, nil, time.UTC)
	if _, err := s.requestCancel("../x", nil); err == nil || err.Error() != "Not a usable execution id: '../x'" {
		t.Fatalf("bad id = %v", err)
	}
	first := "operator stop"
	path, err := s.requestCancel("exec1", &first)
	if err != nil {
		t.Fatal(err)
	}
	second := "later"
	s.requestCancel("exec1", &second)
	if data, _ := os.ReadFile(path); string(data) != "operator stop" {
		t.Fatalf("marker %q", data)
	}
	if !s.cancelRequested("exec1") || s.cancelRequested("exec2") || s.cancelRequested("") {
		t.Fatal("marker lookup")
	}
	if got := s.newCancelRequest("exec1").Reason(); got != "operator stop" {
		t.Fatalf("reason %q", got)
	}
	os.WriteFile(path, nil, 0o600)
	if got := s.newCancelRequest("exec1").Reason(); got != defaultCancelReason {
		t.Fatalf("empty marker reason %q", got)
	}
	old := time.Now().Add(-25 * time.Hour)
	os.Chtimes(path, old, old)
	s.requestCancel("exec3", nil)
	if s.cancelRequested("exec1") || !s.cancelRequested("exec3") {
		t.Fatal("stale marker not pruned")
	}
	s.clearCancel("exec3")
	if s.cancelRequested("exec3") {
		t.Fatal("marker not cleared")
	}
	combined := NewCombinedCancelEvent(nil, s.newCancelRequest("exec4"))
	if combined.IsSet() || cancelReason(combined) != lostOwnershipReason {
		t.Fatal("unset combined event")
	}
	s.requestCancel("exec4", nil)
	if !combined.IsSet() || cancelReason(combined) != defaultCancelReason {
		t.Fatal("combined event misses the marker")
	}
}

func TestCancelRunBooksTheLiveExecutionAsCancelled(t *testing.T) {
	s := newClockStore(t, nil, time.UTC)
	ctx := context.Background()
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("p"), Schedule: "every 1h"})
	idle, err := cancelRun(s, job.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := idle.Get("cancelled"); v != false {
		t.Fatalf("idle cancel %v", idle)
	}
	l := openExecutionsLedger(s)
	rec, _ := l.Create(ctx, job.ID, "cron", nil)
	id := recText(rec, "id")
	l.MarkRunning(ctx, id)
	res, err := cancelRun(s, job.ID, "stop it")
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := res.Get("execution_id"); v != id || res.Keys[len(res.Keys)-1] != "detail" {
		t.Fatalf("cancel result %v", res.Keys)
	}
	torn := 0
	_, ran, err := runJobUnlessCancelled(ctx, s, func() (string, error) { return "", errors.New("interrupted") },
		job, func() { torn++ }, fenceStub(true), id, nil)
	if err != nil || ran || torn != 1 {
		t.Fatalf("cancelled run = %v %v torn %d", ran, err, torn)
	}
	if got := latestStatus(t, l, job.ID); got != "cancelled" {
		t.Fatalf("ledger %s", got)
	}
	j := mustGet(t, s, job.ID)
	if deref(j.LastStatus) != "cancelled" || j.LastError != nil {
		t.Fatalf("job %v %v", deref(j.LastStatus), deref(j.LastError))
	}
	out, ran, err := runJobUnlessCancelled(ctx, s, func() (string, error) { return "ok", nil }, job, func() { torn++ }, fenceStub(false), id, nil)
	if out != "ok" || !ran || err != nil || torn != 1 {
		t.Fatalf("plain run = %q %v %v torn %d", out, ran, err, torn)
	}
	lost, _ := l.Create(ctx, job.ID, "cron", nil)
	if rec, _ := s.finishLostExecution(ctx, recText(lost, "id"), "claim lost"); recText(rec, "status") != "failed" || rec["error"] != "claim lost" {
		t.Fatalf("lost %v", rec)
	}
}
