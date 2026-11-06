package cron

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func fireCanonical(t *testing.T, s *Store, jobID, eid string, retire bool) {
	t.Helper()
	journal := s.journalPath(jobID, eid)
	if err := os.MkdirAll(filepath.Dir(journal), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.markJobRun(context.Background(), jobID, true, MarkOptions{ExecutionID: strp(eid)}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if retire {
		if err := os.Remove(journal); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCompletionIDsRetireWithTheirJournal(t *testing.T) {
	s, _ := newTestStore(t)
	job, err := s.createJob(CreateJobOptions{Prompt: prompt("report"), Schedule: "every 1m", Deliver: strp("local")})
	if err != nil {
		t.Fatal(err)
	}
	fireCanonical(t, s, job.ID, "pending", false)
	for n := 0; n < 64; n++ {
		fireCanonical(t, s, job.ID, "run-"+strconv.Itoa(n), true)
	}
	got, _ := s.getJob(job.ID)
	if !reflect.DeepEqual(got.CanonicalCompletions, []string{"pending", "run-63"}) {
		t.Fatal(got.CanonicalCompletions)
	}
	completed := got.Repeat.Completed
	if ok, err := s.markJobRun(context.Background(), job.ID, true, MarkOptions{ExecutionID: strp("pending")}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if again, _ := s.getJob(job.ID); again.Repeat.Completed != completed {
		t.Fatal(again.Repeat.Completed, completed)
	}
}

func TestJournalPathHashesTheJSONPairUnderTheHome(t *testing.T) {
	s, _ := newTestStore(t)
	if got, want := s.journalPath("job", "req"), filepath.Join(s.home, "cron", "admissions", "aadc90dc2eb6797525bda641149fe10a79d48b48f5628a2b6d8178453cef631a.json"); got != want {
		t.Fatal(got, want)
	}
}

func TestPauseForUnknownRecordsAndLiftPauseResumesOnlyThatPause(t *testing.T) {
	s, _ := newTestStore(t)
	job, _ := s.createJob(CreateJobOptions{Prompt: prompt("x"), Schedule: "every 1h"})
	journal := s.journalPath(job.ID, "e1")
	_ = os.MkdirAll(filepath.Dir(journal), 0o700)
	_ = os.WriteFile(journal, []byte("\ufeff{\"params\": {\"job_id\": \""+job.ID+"\"}, \"receipt\": null}"), 0o600)
	if err := s.pauseForUnknown(job.ID, "e1", "unverified admission"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(journal)
	decoded, _ := pyjson.Loads(string(raw))
	rec := decoded.(*pyjson.Object)
	pause, ok := rec.Get("pause_recorded")
	if !ok || !strings.Contains(string(raw), "\"reason\": \"unverified admission\"") || !strings.HasPrefix(string(raw), "{\n  \"params\"") {
		t.Fatal(string(raw))
	}
	if st, _ := os.Stat(journal); st.Mode().Perm() != 0o600 {
		t.Fatal(st.Mode())
	}
	if err := s.liftPause(job.ID, true); err != nil {
		t.Fatal(err)
	}
	if j, _ := s.getJob(job.ID); j.State != "paused" {
		t.Fatal("a non dict identity never lifts")
	}
	reason := "operator hold"
	if _, err := s.pauseJob(job.ID, &reason); err != nil {
		t.Fatal(err)
	}
	if err := s.liftPause(job.ID, pause); err != nil {
		t.Fatal(err)
	}
	if j, _ := s.getJob(job.ID); j.State != "paused" {
		t.Fatal("an operator pause must not be lifted")
	}
	r2 := "unverified admission"
	if err := s.pauseForUnknown(job.ID, "e1", r2); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(journal)
	decoded, _ = pyjson.Loads(string(raw))
	pause, _ = decoded.(*pyjson.Object).Get("pause_recorded")
	if err := s.liftPause(job.ID, pause); err != nil {
		t.Fatal(err)
	}
	if j, _ := s.getJob(job.ID); j.State != "scheduled" || j.PausedReason != nil {
		t.Fatalf("%+v", j)
	}
	if err := s.pauseForUnknown(job.ID, "no-journal", "r"); err != nil {
		t.Fatal(err)
	}
}
