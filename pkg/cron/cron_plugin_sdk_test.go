package cron

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/plugins"
)

func TestPluginSDKRecoverInterruptedReturnsRecoveredCount(t *testing.T) {
	s := newClockStore(t, nil, time.UTC)
	l := openExecutionsLedger(s)
	seedForeignRunning(t, l, "job-dead", 178864182760)
	pinLedgerProcess(t, false, func(int) (int64, bool) { return 0, false })
	ctx := context.Background()
	out, handled, err := ServePluginSDK(ctx, s.home, "cron.recover_interrupted", nil, map[string]any{})
	if err != nil || !handled || out != 1 {
		t.Fatalf("out %v handled %v err %v", out, handled, err)
	}
	if latestStatus(t, l, "job-dead") != "unknown" {
		t.Fatal("dead owner attempt left running")
	}
	_, _, err = ServePluginSDK(ctx, s.home, "cron.recover_interrupted", []any{"extra"}, nil)
	var typeErr *plugins.HostBuiltinError
	if !errors.As(err, &typeErr) || typeErr.Type != "TypeError" {
		t.Fatalf("extra positional %v", err)
	}
	if _, handled, _ := ServePluginSDK(ctx, s.home, "chat_catalog_ids", nil, nil); handled {
		t.Fatal("non cron op handled")
	}
}

func TestPluginSDKClaimFireStampsAttemptUnderProviderSource(t *testing.T) {
	s, _ := newTestStore(t)
	job := cliCreate(t, s, CreateJobOptions{Prompt: strPtr("p"), Schedule: "every 1h"})
	ctx := context.Background()
	out, handled, err := ServePluginSDK(ctx, s.home, "cron.claim_fire", []any{job.ID}, map[string]any{"source": "chrono", "force": false, "manual": true})
	if err != nil || !handled {
		t.Fatalf("claim %v %v", handled, err)
	}
	claimed, _ := out.(map[string]any)
	if claimed == nil || claimed["id"] != job.ID || claimed["_scheduled_instant"] != nil {
		t.Fatalf("claimed %v", out)
	}
	execID, _ := claimed["execution_id"].(string)
	rec, err := openExecutionsLedger(s).Get(ctx, execID)
	if err != nil || recText(rec, "source") != "chrono" || recText(rec, "job_id") != job.ID {
		t.Fatalf("attempt %v %v", rec, err)
	}
	missing, _, err := ServePluginSDK(ctx, s.home, "cron.claim_fire", nil, map[string]any{"job_id": "nope", "source": "chrono"})
	if err != nil || missing != nil {
		t.Fatalf("missing job claim %v %v", missing, err)
	}
	if recText(mustLatest(t, s, "nope"), "error") != "Fire claim was not acquired" {
		t.Fatal("missed claim attempt left open")
	}
	_, _, err = ServePluginSDK(ctx, s.home, "cron.claim_fire", []any{job.ID}, nil)
	var typeErr *plugins.HostBuiltinError
	if !errors.As(err, &typeErr) || typeErr.Message != "claim_fire() missing 1 required keyword-only argument: 'source'" {
		t.Fatalf("missing source %v", err)
	}
}

func TestPluginSDKFireClaimedRunsSnapshotWithStartTransport(t *testing.T) {
	s, _ := newTestStore(t)
	job := cliCreate(t, s, CreateJobOptions{Prompt: strPtr("p"), Schedule: "every 1h"})
	ctx := context.Background()
	out, _, err := ServePluginSDK(ctx, s.home, "cron.claim_fire", []any{job.ID}, map[string]any{"source": "chrono", "manual": true})
	if err != nil || out == nil {
		t.Fatalf("claim %v %v", out, err)
	}
	type liveAdapters struct{ name string }
	live := &liveAdapters{"gateway"}
	rememberHostedStart(s.home, StartOptions{Adapters: live, Loop: "loop"})
	t.Cleanup(func() { hostedStarts.Delete(s.home) })
	var ran *Job
	var opts runOneJobOptions
	prev := runOneJobSeam
	runOneJobSeam = func(_ context.Context, st *Store, j *Job, o runOneJobOptions) (bool, error) {
		ran, opts = j, o
		if st.home != s.home {
			t.Errorf("ran on store %s", st.home)
		}
		return true, nil
	}
	t.Cleanup(func() { runOneJobSeam = prev })
	claimed := out.(map[string]any)
	res, _, err := ServePluginSDK(ctx, s.home, "cron.fire_claimed", []any{claimed}, map[string]any{"adapters": &plugins.Opaque{TypeName: "dict"}, "loop": nil, "cancel_event": nil})
	if err != nil || res != nil {
		t.Fatalf("fire %v %v", res, err)
	}
	if ran == nil || ran.ID != job.ID || ran.ExecutionID == nil || *ran.ExecutionID != claimed["execution_id"] || ran.ScheduledInstant != nil {
		t.Fatalf("ran %+v", ran)
	}
	if opts.Adapters != live || opts.Loop != nil || !opts.Verbose {
		t.Fatalf("opts %+v", opts)
	}
	_, _, err = ServePluginSDK(ctx, s.home, "cron.fire_claimed", []any{claimed}, map[string]any{"cancel_event": &plugins.Opaque{TypeName: "Event"}})
	var attrErr *plugins.HostBuiltinError
	if !errors.As(err, &attrErr) || attrErr.Type != "AttributeError" {
		t.Fatalf("cancel event %v", err)
	}
	failure := errors.New("run failed")
	runOneJobSeam = func(context.Context, *Store, *Job, runOneJobOptions) (bool, error) { return false, failure }
	if _, _, err := ServePluginSDK(ctx, s.home, "cron.fire_claimed", nil, map[string]any{"claimed_job": claimed}); !errors.Is(err, failure) {
		t.Fatalf("run failure %v", err)
	}
}

// Latest attempt of the job in the store ledger
func mustLatest(t *testing.T, s *Store, jobID string) map[string]any {
	t.Helper()
	rec, err := openExecutionsLedger(s).Latest(context.Background(), jobID)
	if err != nil || rec == nil {
		t.Fatalf("latest %v %v", rec, err)
	}
	return rec
}
