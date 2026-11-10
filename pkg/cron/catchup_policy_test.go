package cron

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestMissedPolicyPreservesGraceAndManual(t *testing.T) {
	for _, catchUp := range []bool{true, false} {
		t.Run(map[bool]string{true: "true", false: "false"}[catchUp], func(t *testing.T) {
			s, _ := newTestStore(t)
			text := "cron:\n  catch_up_missed: false\n"
			if catchUp {
				text = "cron:\n  catch_up_missed: true\n"
			}
			os.WriteFile(filepath.Join(s.home, "config.yaml"), []byte(text), 0o600)
			now := s.clk.now()
			for _, tc := range []struct {
				name string
				lag  time.Duration
			}{{"stale", 14400 * time.Second}, {"grace", 30 * time.Second}, {"manual", 14400 * time.Second}} {
				job := mustCreate(t, s, CreateJobOptions{Prompt: prompt(tc.name), Schedule: "every 1h", Model: "fixture", Deliver: prompt("local")})
				editJobs(t, s, func(j *Job) {
					if j.ID == job.ID {
						j.NextRunAt = strPtr(FormatISO(now.Add(-tc.lag)))
						if tc.name == "manual" {
							j.ManualRunAt = strPtr(*j.NextRunAt)
						}
					}
				})
			}
			var due []string
			for _, j := range mustDue(t, s) {
				due = append(due, j.Prompt)
			}
			sort.Strings(due)
			want := []string{"grace", "manual"}
			if catchUp {
				want = []string{"grace", "manual", "stale"}
			}
			if !reflect.DeepEqual(due, want) {
				t.Fatalf("due %v want %v", due, want)
			}
			jobs, _ := s.LoadJobs()
			for _, j := range jobs {
				if j.Prompt == "stale" {
					if at, ok := s.clk.parseAware(deref(j.NextRunAt), nil); !ok || !at.After(now) {
						t.Fatalf("stale next run %v not fast forwarded past %s", deref(j.NextRunAt), FormatISO(now))
					}
				}
			}
		})
	}
}

func TestDefaultAndUncomputableStillCatchUp(t *testing.T) {
	for _, uncomputable := range []bool{false, true} {
		t.Run(map[bool]string{true: "true", false: "false"}[uncomputable], func(t *testing.T) {
			s, _ := newTestStore(t)
			if uncomputable {
				os.WriteFile(filepath.Join(s.home, "config.yaml"), []byte("cron:\n  catch_up_missed: false\n"), 0o600)
			}
			job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("default"), Schedule: "every 1h", Model: "fixture", Deliver: prompt("local")})
			editJobs(t, s, func(j *Job) { j.NextRunAt = strPtr(FormatISO(s.clk.now().Add(-4 * time.Hour))) })
			if uncomputable {
				patchChronos(t, &computeNextRunFn, func(*Store, *Schedule, *string, *string) (*string, error) { return nil, nil })
			}
			if due := jobIDs(mustDue(t, s)); !reflect.DeepEqual(due, []string{job.ID}) {
				t.Fatalf("due %v", due)
			}
		})
	}
}
