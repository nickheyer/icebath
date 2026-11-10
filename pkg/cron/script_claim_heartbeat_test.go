package cron

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func claimStoreAt(t *testing.T, home string) *Store {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, "cron"), 0o700); err != nil {
		t.Fatal(err)
	}
	s := openCronStore(home, nil)
	s.log = slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return s
}

func swapScriptHeartbeatSeams(t *testing.T, run func(s *Store, ctx context.Context, scriptPath, workdir string, cancel CancelSource, interpreter *string) (bool, string, error),
	beat func(s *Store, ctx context.Context, jobID, owner string) (bool, error)) {
	t.Helper()
	prevRun, prevBeat, prevEvery := cronRunJobScript, cronHeartbeatRunClaim, runClaimHeartbeatSeconds
	t.Cleanup(func() {
		cronRunJobScript, cronHeartbeatRunClaim, runClaimHeartbeatSeconds = prevRun, prevBeat, prevEvery
	})
	cronRunJobScript, cronHeartbeatRunClaim, runClaimHeartbeatSeconds = run, beat, 0.01
}

func TestLongRunningScriptRefreshesOwnedClaimInProfileStore(t *testing.T) {
	cases := []struct {
		name    string
		noAgent bool
		output  string
	}{
		{"script-only-job", true, "watchdog complete"},
		{"pre-agent-script", false, `{"wakeAgent": false}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			installCronOwner(t)
			resetDeliver(t)
			root := t.TempDir()
			profileHome := filepath.Join(root, "profile")
			t.Setenv("ICEBATH_HOME", profileHome)
			profile, fallback := claimStoreAt(t, profileHome), claimStoreAt(t, filepath.Join(root, "default"))
			const originalTimestamp = "2026-07-12T12:00:00+00:00"
			originalTime := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
			ttl := time.Duration(profile.oneshotRunClaimTTL(context.Background()) * float64(time.Second))
			var clock sync.Mutex
			current := originalTime.Add(ttl - 60*time.Second)
			profile.clk.nowFn = func() time.Time {
				clock.Lock()
				defer clock.Unlock()
				return current
			}
			record := map[string]any{"jobs": []any{map[string]any{
				"id": "long-script", "name": "long script", "prompt": "inspect the script output", "script": "watchdog.py", "no_agent": c.noAgent,
				"schedule":    map[string]any{"kind": "once", "run_at": originalTimestamp},
				"next_run_at": originalTimestamp, "enabled": true,
				"run_claim": map[string]any{"at": originalTimestamp, "by": "dispatch-owner"},
			}}}
			writeJobsFile(t, fallback, record)
			writeJobsFile(t, profile, record)
			claimed := mustJob(t, profile, "long-script")
			heartbeatSeen := make(chan struct{})
			var once sync.Once
			var scanDue []*Job
			var scanErr error
			recordPresent := false
			swapScriptHeartbeatSeams(t,
				func(*Store, context.Context, string, string, CancelSource, *string) (bool, string, error) {
					select {
					case <-heartbeatSeen:
					case <-time.After(2 * time.Second):
						t.Error("claim was not refreshed while script blocked")
					}
					return true, c.output, nil
				},
				func(s *Store, _ context.Context, jobID, owner string) (bool, error) {
					updated, err := s.heartbeatRunClaim(context.Background(), jobID, owner)
					once.Do(func() {
						clock.Lock()
						current = originalTime.Add(ttl + 10*time.Second)
						clock.Unlock()
						scanDue, scanErr = s.getDueJobs(context.Background())
						job, _ := s.getJob(context.Background(), jobID)
						recordPresent = job != nil
						close(heartbeatSeen)
					})
					return updated, err
				})
			out, err := profile.runJob(context.Background(), claimed, runJobOptions{})
			if err != nil || !out.Success || out.Error != nil {
				t.Fatalf("outcome %+v %v", out, err)
			}
			claim := mustJob(t, profile, "long-script").RunClaim
			if claim == nil || claim.At == originalTimestamp || claim.By != "dispatch-owner" {
				t.Fatalf("profile claim %+v", claim)
			}
			if scanErr != nil || len(scanDue) != 0 || !recordPresent {
				t.Fatalf("second scheduler scan due=%v present=%v err=%v", jobIDs(scanDue), recordPresent, scanErr)
			}
			if untouched := mustJob(t, fallback, "long-script").RunClaim; untouched == nil || *untouched != (Claim{At: originalTimestamp, By: "dispatch-owner"}) {
				t.Fatalf("the other store's claim moved %+v", untouched)
			}
		})
	}
}

func TestScriptHeartbeatUsesCapturedClaimOwner(t *testing.T) {
	s := claimStoreAt(t, filepath.Join(t.TempDir(), "profile"))
	const originalTimestamp, replacementTimestamp = "2026-07-12T12:00:00+00:00", "2026-07-12T12:00:30+00:00"
	job := &Job{ID: "reclaimed-script", Script: strPtr("watchdog.py"), Schedule: &Schedule{Kind: "once", RunAt: originalTimestamp},
		RunClaim: &Claim{At: originalTimestamp, By: "original-owner"}}
	writeJobsFile(t, s, map[string]any{"jobs": []any{map[string]any{
		"id": "reclaimed-script", "script": "watchdog.py", "schedule": map[string]any{"kind": "once", "run_at": originalTimestamp},
		"run_claim": map[string]any{"at": replacementTimestamp, "by": "replacement-owner"},
	}}})
	heartbeatSeen := make(chan struct{})
	var once sync.Once
	var owners []string
	var refreshed []bool
	var seen sync.Mutex
	swapScriptHeartbeatSeams(t,
		func(_ *Store, _ context.Context, scriptPath, _ string, _ CancelSource, _ *string) (bool, string, error) {
			if scriptPath != "watchdog.py" {
				t.Errorf("script path %q", scriptPath)
			}
			select {
			case <-heartbeatSeen:
			case <-time.After(2 * time.Second):
				t.Error("the heartbeat never ran while the script blocked")
			}
			return true, "done", nil
		},
		func(s *Store, _ context.Context, jobID, owner string) (bool, error) {
			updated, err := s.heartbeatRunClaim(context.Background(), jobID, owner)
			seen.Lock()
			owners, refreshed = append(owners, owner), append(refreshed, updated)
			seen.Unlock()
			once.Do(func() { close(heartbeatSeen) })
			return updated, err
		})
	ok, out, err := s.runJobScriptWithClaimHeartbeat(context.Background(), job, "watchdog.py", "", nil)
	if err != nil || !ok || out != "done" {
		t.Fatalf("got %v %q %v", ok, out, err)
	}
	if claim := mustJob(t, s, "reclaimed-script").RunClaim; claim == nil || *claim != (Claim{At: replacementTimestamp, By: "replacement-owner"}) {
		t.Fatalf("the replacement owner's claim moved %+v", claim)
	}
	seen.Lock()
	defer seen.Unlock()
	for i, owner := range owners {
		if owner != "original-owner" || refreshed[i] {
			t.Fatalf("heartbeat %d ran as %q refreshed=%v", i, owner, refreshed[i])
		}
	}
	if len(owners) == 0 {
		t.Fatal("no heartbeat recorded")
	}
}

func TestScriptRunWithoutAnOwnedOneShotClaimStartsNoHeartbeat(t *testing.T) {
	s := claimStoreAt(t, filepath.Join(t.TempDir(), "profile"))
	beats := 0
	var mu sync.Mutex
	swapScriptHeartbeatSeams(t,
		func(*Store, context.Context, string, string, CancelSource, *string) (bool, string, error) {
			time.Sleep(60 * time.Millisecond)
			return true, "ran", nil
		},
		func(*Store, context.Context, string, string) (bool, error) {
			mu.Lock()
			beats++
			mu.Unlock()
			return true, nil
		})
	for _, job := range []*Job{
		{ID: "recurring", Schedule: &Schedule{Kind: "interval"}, RunClaim: &Claim{At: "2026-07-12T12:00:00+00:00", By: "owner"}},
		{ID: "unclaimed", Schedule: &Schedule{Kind: "once"}},
		{ID: "ownerless", Schedule: &Schedule{Kind: "once"}, RunClaim: &Claim{At: "2026-07-12T12:00:00+00:00"}},
		{ID: "unscheduled", RunClaim: &Claim{At: "2026-07-12T12:00:00+00:00", By: "owner"}},
	} {
		ok, out, err := s.runJobScriptWithClaimHeartbeat(context.Background(), job, "watchdog.py", "", nil)
		if err != nil || !ok || out != "ran" {
			t.Fatalf("%s got %v %q %v", job.ID, ok, out, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if beats != 0 {
		t.Fatalf("%d heartbeats for runs that hold no one shot claim", beats)
	}
}
