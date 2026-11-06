package cron

import (
	"context"
	"sync"
	"testing"
)

var testHooks = struct{ missed []string }{}

var testRun = struct {
	ran       []string
	runOne    func(ctx context.Context, s *Store, job *Job, o runOneJobOptions) (bool, error)
	providers map[string]CronScheduler
	loadErr   error
}{providers: map[string]CronScheduler{}}

var testRunMu sync.Mutex

var testTick = struct {
	sync.Mutex
	reconciled []bool
	pruned     []string
}{}

var testDeliver = struct {
	sync.Mutex
	sent    []string
	err     *string
	targets []Target
}{}

// Real delivery seams saved before the recorders replace them
var (
	realResolveDeliveryTargets = resolveDeliveryTargets
	realDeliverResult          = deliverResultSeam
	realDrainLegacyPending     = drainLegacyPending
	realReconcilePending       = reconcilePending
	realWorktreePrune          = worktreePruneStaleWorktrees
	realLoadCronScheduler      = loadCronScheduler
	realRunOneJob              = runOneJobSeam
)

// Recorders every test sees unless it opts into the real seams
func init() {
	runOneJobSeam = recordedRunOneJob
	reconcilePending = func(ctx context.Context, s *Store, allowConnect bool) error {
		testTick.Lock()
		defer testTick.Unlock()
		testTick.reconciled = append(testTick.reconciled, allowConnect)
		return nil
	}
	drainLegacyPending = func(ctx context.Context, s *Store) error { return nil }
	worktreePruneStaleWorktrees = func(ctx context.Context, repo string) error {
		testTick.Lock()
		defer testTick.Unlock()
		testTick.pruned = append(testTick.pruned, repo)
		return nil
	}
	loadCronScheduler = func(s *Store, name string) (CronScheduler, error) {
		return testRun.providers[name], testRun.loadErr
	}
	deliverResultSeam = func(s *Store, ctx context.Context, job *Job, content string, adapters, loop any, forFailure bool) (*string, error) {
		testDeliver.Lock()
		defer testDeliver.Unlock()
		testDeliver.sent = append(testDeliver.sent, content)
		return testDeliver.err, nil
	}
	resolveDeliveryTargets = func(ctx context.Context, s *Store, job *Job, forFailure bool) []Target {
		testDeliver.Lock()
		defer testDeliver.Unlock()
		return testDeliver.targets
	}
}

func recordedRunOneJob(ctx context.Context, s *Store, job *Job, o runOneJobOptions) (bool, error) {
	testRunMu.Lock()
	defer testRunMu.Unlock()
	testRun.ran = append(testRun.ran, job.ID)
	if testRun.runOne != nil {
		return testRun.runOne(ctx, s, job, o)
	}
	return true, nil
}

func ranJobs() []string {
	testRunMu.Lock()
	defer testRunMu.Unlock()
	return append([]string{}, testRun.ran...)
}

func resetRan() {
	testRunMu.Lock()
	defer testRunMu.Unlock()
	testRun.ran = nil
}

// Real target resolution and delivery for one test, the recorders restored after it
func useRealDelivery(t *testing.T) {
	t.Helper()
	savedTargets, savedDeliver := resolveDeliveryTargets, deliverResultSeam
	resolveDeliveryTargets, deliverResultSeam = realResolveDeliveryTargets, realDeliverResult
	t.Cleanup(func() { resolveDeliveryTargets, deliverResultSeam = savedTargets, savedDeliver })
}

// Real legacy drain, reconcile, worktree prune, provider loader and run seams for one test
func useRealTickSeams(t *testing.T) {
	t.Helper()
	d, r, w, l, o := drainLegacyPending, reconcilePending, worktreePruneStaleWorktrees, loadCronScheduler, runOneJobSeam
	drainLegacyPending, reconcilePending, worktreePruneStaleWorktrees, loadCronScheduler, runOneJobSeam = realDrainLegacyPending, realReconcilePending, realWorktreePrune, realLoadCronScheduler, realRunOneJob
	t.Cleanup(func() {
		drainLegacyPending, reconcilePending, worktreePruneStaleWorktrees, loadCronScheduler, runOneJobSeam = d, r, w, l, o
	})
}
