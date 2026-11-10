package cron

import (
	"testing"
	"time"
)

func watchedJob(t *testing.T, s *Store, text string) *Job {
	t.Helper()
	return mustCreate(t, s, CreateJobOptions{Prompt: prompt(text), Schedule: "every 1h"})
}

func markRun(t *testing.T, s *Store, id string, success bool, o MarkOptions) {
	t.Helper()
	if ok, err := s.markJobRun(t.Context(), id, success, o); err != nil || !ok {
		t.Fatalf("markJobRun %v %v", ok, err)
	}
}

func parsesAsISO(t *testing.T, text string) {
	t.Helper()
	for _, layout := range []string{"2006-01-02T15:04:05.999999999-07:00", "2006-01-02T15:04:05.999999999"} {
		if _, err := time.Parse(layout, text); err == nil {
			return
		}
	}
	t.Fatalf("%q is not an ISO timestamp", text)
}

func TestLastFailureStampFailedRunStampsLastFailure(t *testing.T) {
	s, _ := newTestStore(t)
	job := watchedJob(t, s, "Watch the fleet")
	markRun(t, s, job.ID, false, MarkOptions{Error: strPtr("Script exited with code 1")})
	stamp := mustGet(t, s, job.ID).LastFailure
	if stamp == nil || stamp.Detail != "Script exited with code 1" {
		t.Fatalf("stamp %+v", stamp)
	}
	parsesAsISO(t, stamp.At)
}

func TestLastFailureStampSuccessResetsStatusButKeepsStamp(t *testing.T) {
	s, _ := newTestStore(t)
	job := watchedJob(t, s, "Watch the fleet")
	markRun(t, s, job.ID, false, MarkOptions{Error: strPtr("Script exited with code 1")})
	if failed := mustGet(t, s, job.ID); deref(failed.LastStatus) != "error" || failed.FailureStreak != 1 {
		t.Fatalf("failed %v %d", deref(failed.LastStatus), failed.FailureStreak)
	}
	markRun(t, s, job.ID, true, MarkOptions{})
	healed := mustGet(t, s, job.ID)
	if deref(healed.LastStatus) != "ok" || healed.FailureStreak != 0 || healed.LastFailure == nil || healed.LastFailure.Detail != "Script exited with code 1" {
		t.Fatalf("healed %v %d %+v", deref(healed.LastStatus), healed.FailureStreak, healed.LastFailure)
	}
}

func TestLastFailureStampLatestFailureWins(t *testing.T) {
	s, _ := newTestStore(t)
	job := watchedJob(t, s, "Watch the fleet")
	markRun(t, s, job.ID, false, MarkOptions{Error: strPtr("first failure")})
	markRun(t, s, job.ID, true, MarkOptions{})
	markRun(t, s, job.ID, false, MarkOptions{Error: strPtr("second failure")})
	if stamp := mustGet(t, s, job.ID).LastFailure; stamp == nil || stamp.Detail != "second failure" {
		t.Fatalf("stamp %+v", stamp)
	}
}

func TestLastFailureStampSuccessWithoutPriorFailureLeavesNoStamp(t *testing.T) {
	s, _ := newTestStore(t)
	job := watchedJob(t, s, "Watch the fleet")
	markRun(t, s, job.ID, true, MarkOptions{})
	if stamp := mustGet(t, s, job.ID).LastFailure; stamp != nil {
		t.Fatalf("stamp %+v", stamp)
	}
}

func TestLastFailureStampFailureWithoutErrorTextUsesStatus(t *testing.T) {
	s, _ := newTestStore(t)
	job := watchedJob(t, s, "Watch the fleet")
	markRun(t, s, job.ID, false, MarkOptions{Status: strPtr("blocked_config")})
	if stamp := mustGet(t, s, job.ID).LastFailure; stamp == nil || stamp.Detail != "blocked_config" {
		t.Fatalf("stamp %+v", stamp)
	}
}

func TestLastFailureStampDeliveryFailureKeepsItsOwnChannel(t *testing.T) {
	s, _ := newTestStore(t)
	job := watchedJob(t, s, "Watch the fleet")
	markRun(t, s, job.ID, true, MarkOptions{DeliveryError: strPtr("webhook unreachable")})
	record := mustGet(t, s, job.ID)
	if record.LastFailure != nil || deref(record.LastDeliveryError) != "webhook unreachable" {
		t.Fatalf("record %+v %v", record.LastFailure, deref(record.LastDeliveryError))
	}
}

func noteForwardFailure(t *testing.T, s *Store, id, detail string) bool {
	t.Helper()
	ok, err := s.noteFireForwardFailure(id, detail)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestNoteFireForwardFailureStampsLastFireError(t *testing.T) {
	s, _ := newTestStore(t)
	job := watchedJob(t, s, "Daily invoice triage")
	if !noteForwardFailure(t, s, job.ID, "gateway unreachable") {
		t.Fatal("the stamp was refused")
	}
	stamp := mustGet(t, s, job.ID).LastFireError
	if stamp == nil || stamp.Detail != "gateway unreachable" {
		t.Fatalf("stamp %+v", stamp)
	}
	parsesAsISO(t, stamp.At)
}

func TestNoteFireForwardFailureUnknownJobReturnsFalse(t *testing.T) {
	s, _ := newTestStore(t)
	if noteForwardFailure(t, s, "nope", "gateway unreachable") {
		t.Fatal("an unknown job was stamped")
	}
}

func TestNoteFireForwardFailureRepeatedFailuresOverwriteLatestWins(t *testing.T) {
	s, _ := newTestStore(t)
	job := watchedJob(t, s, "Daily invoice triage")
	noteForwardFailure(t, s, job.ID, "first miss")
	noteForwardFailure(t, s, job.ID, "second miss")
	if stamp := mustGet(t, s, job.ID).LastFireError; stamp == nil || stamp.Detail != "second miss" {
		t.Fatalf("stamp %+v", stamp)
	}
}

func TestNoteFireForwardFailureSuccessfulRunClearsStamp(t *testing.T) {
	s, _ := newTestStore(t)
	job := watchedJob(t, s, "Daily invoice triage")
	noteForwardFailure(t, s, job.ID, "gateway unreachable")
	if mustGet(t, s, job.ID).LastFireError == nil {
		t.Fatal("no stamp before the run")
	}
	markRun(t, s, job.ID, true, MarkOptions{})
	if stamp := mustGet(t, s, job.ID).LastFireError; stamp != nil {
		t.Fatalf("stamp %+v survived a successful run", stamp)
	}
}

func TestNoteFireForwardFailureFailedRunKeepsStamp(t *testing.T) {
	s, _ := newTestStore(t)
	job := watchedJob(t, s, "Daily invoice triage")
	noteForwardFailure(t, s, job.ID, "gateway unreachable")
	markRun(t, s, job.ID, false, MarkOptions{Error: strPtr("boom")})
	if stamp := mustGet(t, s, job.ID).LastFireError; stamp == nil || stamp.Detail != "gateway unreachable" {
		t.Fatalf("stamp %+v", stamp)
	}
}

func TestFormatJobSurfacesFireErrorCronjobListCarriesLastFireError(t *testing.T) {
	s, _ := newTestStore(t)
	job := watchedJob(t, s, "Daily invoice triage")
	noteForwardFailure(t, s, job.ID, "gateway unreachable")
	formatted := s.formatJob(mustGet(t, s, job.ID))
	stamp, _ := formatted.Get("last_fire_error")
	detail := ""
	switch v := stamp.(type) {
	case map[string]any:
		detail, _ = v["detail"].(string)
	case *Stamp:
		detail = v.Detail
	case Stamp:
		detail = v.Detail
	default:
		if obj, ok := stamp.(interface{ Get(string) (any, bool) }); ok {
			raw, _ := obj.Get("detail")
			detail, _ = raw.(string)
		}
	}
	if detail != "gateway unreachable" {
		t.Fatalf("formatted last_fire_error %#v", stamp)
	}
}
