package cron

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestBoundedJobsLockReentrantNestingStillWorks(t *testing.T) {
	s, _ := newTestStore(t)
	var nested *Job
	err := s.withJobsLock(context.Background(), func(tx *jobsTx) error {
		var err error
		nested, err = s.createJobTx(context.Background(), tx, CreateJobOptions{Prompt: strPtr("x"), Schedule: "every 1h"})
		return err
	})
	if err != nil || nested == nil {
		t.Fatalf("nested section inside the held lock failed: %v", err)
	}
	if stored := mustGet(t, s, nested.ID); stored.ID != nested.ID {
		t.Fatalf("stored %+v", stored)
	}
}

// Stamps a fire claim at the given offset from now onto the stored job
func stampFireClaim(t *testing.T, s *Store, id string, offset time.Duration) {
	t.Helper()
	at := FormatISO(s.clk.now().Add(offset))
	editJobs(t, s, func(j *Job) {
		if j.ID == id {
			j.FireClaim = &Claim{At: at, By: "other-host:1"}
		}
	})
}

func claimStallJob(t *testing.T, s *Store) *Job {
	t.Helper()
	return mustCreate(t, s, CreateJobOptions{Name: strPtr("claim job"), Schedule: "0 7 * * *", Prompt: strPtr("x")})
}

func TestFutureDatedClaimsFutureFireClaimIsTreatedAsStale(t *testing.T) {
	s, _ := newTestStore(t)
	job := claimStallJob(t, s)
	stampFireClaim(t, s, job.ID, 6*time.Hour)
	if _, ok := tryClaimFire(t, s, job.ID, FireClaimOptions{}); !ok {
		t.Fatal("future-dated claim must be overwritable, not eternally fresh")
	}
}

func TestFutureDatedClaimsFreshPastFireClaimStillBlocks(t *testing.T) {
	s, _ := newTestStore(t)
	job := claimStallJob(t, s)
	if _, ok := tryClaimFire(t, s, job.ID, FireClaimOptions{}); !ok {
		t.Fatal("first claim refused")
	}
	if _, ok := tryClaimFire(t, s, job.ID, FireClaimOptions{}); ok {
		t.Fatal("fresh claim did not block")
	}
}

func TestFutureDatedClaimsExpiredFireClaimIsReclaimable(t *testing.T) {
	s, _ := newTestStore(t)
	job := claimStallJob(t, s)
	stampFireClaim(t, s, job.ID, -6*time.Hour)
	if _, ok := tryClaimFire(t, s, job.ID, FireClaimOptions{}); !ok {
		t.Fatal("expired claim not reclaimable")
	}
}

func TestHonestRunSkipMessagesPausedJobNotReportedAsAlreadyFiring(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Name: strPtr("paused job"), Schedule: "0 7 * * *", Prompt: strPtr("x")})
	if _, err := s.pauseJob(context.Background(), job.ID, nil); err != nil {
		t.Fatal(err)
	}
	res := s.executeJobNow(context.Background(), mustJob(t, s, job.ID), nil)
	errText := strings.ToLower(pyStrOf(res.Error))
	if res.Claimed || !strings.Contains(errText, "paused") || strings.Contains(errText, "already being fired") {
		t.Fatalf("paused %+v", res)
	}
}

func TestHonestRunSkipMessagesMissingJobNotReportedAsAlreadyFiring(t *testing.T) {
	s, _ := newTestStore(t)
	res := s.executeJobNow(context.Background(), &Job{ID: "does-not-exist-123"}, nil)
	if res.Claimed || !strings.Contains(strings.ToLower(pyStrOf(res.Error)), "no longer exists") {
		t.Fatalf("missing %+v", res)
	}
}
