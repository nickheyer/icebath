package cron

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLiveDirectRunSurvivesStaleSweep(t *testing.T) {
	s, _ := newTestStore(t)
	prevRun := runOneJobSeam
	runOneJobSeam = runOneJob
	t.Cleanup(func() { runOneJobSeam = prevRun })
	resetDeliver(t)
	dir := t.TempDir()
	started, release := filepath.Join(dir, "started"), filepath.Join(dir, "release")
	script := writeScript(t, s, "live.sh", "touch '"+started+"'\nfor i in $(seq 1 3000); do [ -e '"+release+"' ] && break; sleep 0.01; done\necho direct-run-ok\n")
	job := mustCreate(t, s, CreateJobOptions{Schedule: "every 15m", Script: strPtr(script), NoAgent: true, Deliver: strPtr("local")})
	ctx := context.Background()
	var result *manualRunResult
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); result = s.executeJobNow(ctx, job, nil) }()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(started); err != nil {
		os.WriteFile(release, nil, 0o600)
		wg.Wait()
		t.Fatalf("script never started %+v", result)
	}
	k := s.inflightKey(job.ID, s.home)
	inflight.Lock()
	inflight.since[k] -= 86400
	inflight.Unlock()
	if swept := s.sweepStaleInflight([]*Job{mustJob(t, s, job.ID)}); len(swept) != 0 {
		t.Errorf("swept %v", swept)
	}
	if again := s.executeJobNow(ctx, mustJob(t, s, job.ID), nil); again.Claimed {
		t.Errorf("second run claimed %+v", again)
	}
	os.WriteFile(release, nil, 0o600)
	wg.Wait()
	if isJobRunning(s, job.ID) || result == nil || !result.Success {
		t.Fatalf("result %+v running %v", result, isJobRunning(s, job.ID))
	}
	saved := mustJob(t, s, job.ID)
	if deref(saved.LastStatus) != "ok" || saved.FireClaim != nil {
		t.Fatalf("saved %+v", saved)
	}
	found := false
	filepath.WalkDir(filepath.Join(s.home, "cron", "output"), func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, ".md") {
			b, _ := os.ReadFile(p)
			found = found || strings.Contains(string(b), "direct-run-ok")
		}
		return nil
	})
	latest, _ := openExecutionsLedger(s).LatestMany(ctx, []string{job.ID})
	if !found || latest[job.ID]["status"] != "completed" {
		t.Fatalf("output found %v latest %v", found, latest[job.ID])
	}
}

func TestDirectDispatchCleanupPreservesSuccessor(t *testing.T) {
	for _, path := range []string{"direct", "direct-error"} {
		s, _ := newTestStore(t)
		job := mustCreate(t, s, CreateJobOptions{Prompt: strPtr("offline"), Schedule: "every 15m", Deliver: strPtr("local")})
		k := s.inflightKey(job.ID, s.home)
		var successor *poolFuture
		prev := runOneJobSeam
		runOneJobSeam = func(ctx context.Context, s *Store, j *Job, o runOneJobOptions) (bool, error) {
			s.releaseRunningJob(j.ID, s.home, nil)
			if !s.tryRegisterRunningJob(j.ID, nil, nil) {
				t.Fatal("successor registration refused")
			}
			inflight.Lock()
			successor = inflight.futures[k]
			inflight.Unlock()
			if strings.HasSuffix(path, "error") {
				panic("old dispatch failed")
			}
			return true, nil
		}
		func() {
			defer func() { recover() }()
			s.runClaimedJob(context.Background(), job, nil)
		}()
		runOneJobSeam = prev
		inflight.Lock()
		current := inflight.futures[k]
		inflight.Unlock()
		if !isJobRunning(s, job.ID) || current != successor {
			t.Errorf("%s running %v successor kept %v", path, isJobRunning(s, job.ID), current == successor)
		}
		s.releaseRunningJob(job.ID, s.home, nil)
	}
}
