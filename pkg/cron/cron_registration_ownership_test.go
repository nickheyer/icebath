package cron

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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

func TestBoundedJobsLockTimesOutAndDegrades(t *testing.T) {
	s, buf := newTestStore(t)
	s.ensureDirs()
	shortLockTimeout(t, time.Second)
	f, err := os.OpenFile(filepath.Join(s.cronDir, ".jobs.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	entered := false
	s.withJobsLock(func(tx *jobsTx) error { entered = true; return nil })
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if !entered || time.Since(start) > 10*time.Second || !strings.Contains(buf.String(), "level=ERROR") {
		t.Fatalf("entered %v elapsed %v log %q", entered, time.Since(start), buf.String())
	}
	var nested *Job
	s.withJobsLock(func(tx *jobsTx) error {
		var err error
		nested, err = s.createJobTx(tx, CreateJobOptions{Prompt: strPtr("x"), Schedule: "every 1h"})
		return err
	})
	if nested == nil {
		t.Fatal("nested section inside the held lock failed")
	}
}

// Stamps a fire claim at the given offset from now onto the stored job
func stampFireClaim(t *testing.T, s *Store, id string, offset time.Duration) {
	t.Helper()
	at := FormatISO(s.clk.now().Add(offset))
	err := s.withJobsLock(func(tx *jobsTx) error {
		jobs, err := s.loadJobs(tx)
		if err != nil {
			return err
		}
		for _, j := range jobs {
			if j.ID == id {
				j.FireClaim = &Claim{At: at, By: "other-host:1"}
			}
		}
		return s.saveJobsLocked(tx, jobs, nil, false)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFutureDatedClaims(t *testing.T) {
	s, _ := newTestStore(t)
	mk := func() *Job {
		return mustCreate(t, s, CreateJobOptions{Name: strPtr("claim job"), Schedule: "0 7 * * *", Prompt: strPtr("x")})
	}
	future := mk()
	stampFireClaim(t, s, future.ID, 6*time.Hour)
	if _, ok := tryClaimFire(t, s, future.ID, FireClaimOptions{}); !ok {
		t.Fatal("future dated claim stayed fresh")
	}
	fresh := mk()
	if _, ok := tryClaimFire(t, s, fresh.ID, FireClaimOptions{}); !ok {
		t.Fatal("first claim")
	}
	if _, ok := tryClaimFire(t, s, fresh.ID, FireClaimOptions{}); ok {
		t.Fatal("fresh claim did not block")
	}
	expired := mk()
	stampFireClaim(t, s, expired.ID, -6*time.Hour)
	if _, ok := tryClaimFire(t, s, expired.ID, FireClaimOptions{}); !ok {
		t.Fatal("expired claim not reclaimable")
	}
}

func TestHonestRunSkipMessages(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Name: strPtr("paused job"), Schedule: "0 7 * * *", Prompt: strPtr("x")})
	s.pauseJob(job.ID, nil)
	res := s.executeJobNow(context.Background(), mustJob(t, s, job.ID), nil)
	errText := strings.ToLower(pyStrOf(res.Error))
	if res.Claimed || !strings.Contains(errText, "paused") || strings.Contains(errText, "already being fired") {
		t.Fatalf("paused %+v", res)
	}
	res = s.executeJobNow(context.Background(), &Job{ID: "does-not-exist-123"}, nil)
	if res.Claimed || !strings.Contains(strings.ToLower(pyStrOf(res.Error)), "no longer exists") {
		t.Fatalf("missing %+v", res)
	}
}
