package cron

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"syscall"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	yieldLogIntervalSeconds        = 3600.0
	deadOwnerReapIntervalSeconds   = 300.0
	worktreeMaintenanceIntervalSec = 6 * 3600.0
)

var (
	lastYieldLog = struct {
		sync.Mutex
		reason string
		at     float64
		set    bool
	}{}
	lastDeadOwnerReap = struct {
		sync.Mutex
		at map[string]float64
	}{at: map[string]float64{}}
	tickMonotonic = monoNow
)

// Errno set that marks the cron store read only, full or denied
func isUnwritableErrno(err error) bool {
	for _, e := range []syscall.Errno{syscall.EROFS, syscall.EACCES, syscall.EPERM, syscall.ENOSPC, syscall.EDQUOT} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// Logs the stale code yield at error level once per skew signature or once an hour
func (s *Store) logTickYieldOnce(reason string) {
	now := tickMonotonic()
	lastYieldLog.Lock()
	defer lastYieldLog.Unlock()
	if !lastYieldLog.set || lastYieldLog.reason != reason || now-lastYieldLog.at >= yieldLogIntervalSeconds {
		s.log.Error(fmt.Sprintf("Cron tick yielded: this process is running stale code (%s) and a "+
			"fresher gateway owns the runtime lock — jobs will fire from that "+
			"process. Restart this one to reclaim its ticks.", reason))
	}
	lastYieldLog.reason, lastYieldLog.at, lastYieldLog.set = reason, now, true
}

// Ledger receipt creation of a tick dispatch, the scheduler create_execution attribute tests patch
var tickCreateExecution = (*Ledger).Create

// Runs one tick inside retirement admission, zero when the profile is retiring
func schedulerTick(ctx context.Context, s *Store, o tickOptions) (int, error) {
	admitted, release := provider.Retirement.Work()
	defer release()
	if !admitted {
		return 0, nil
	}
	return s.tickAdmitted(withCronStore(ctx, s), o)
}

// Opens and nonblocking locks the tick file, nil on contention and the error for any real failure
func (s *Store) acquireTickLock(lockFile string) (*os.File, error) {
	f, err := os.OpenFile(lockFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err == nil {
		if err = tryFlockOnce(f); err == nil {
			return f, nil
		}
		f.Close()
		if isLockContentionErrno(err) {
			s.log.Info("Tick skipped — another instance holds the lock")
			return nil, nil
		}
	}
	if isFDExhaustion(err) {
		s.log.Error(fmt.Sprintf("Cron tick could not acquire tick lock: %s — scheduler will "+
			"attempt fd reclamation and retry with backoff", pypath.OSErrorText(err)))
	} else if !isUnwritableErrno(err) {
		s.log.Error(fmt.Sprintf("Cron tick could not acquire tick lock: %s", pypath.OSErrorText(err)))
	}
	return nil, err
}

// Unlocks and closes the tick lock
func releaseTickLock(f *os.File) {
	releaseFlock(f)
}

// Tick lock or nil when the tick must not run, degrading the store when the lock cannot be created
func (s *Store) acquireTickLockOrDegrade() (*os.File, error) {
	lockDir, lockFile := s.getLockPaths()
	if err := ensureCronDir(lockDir); err != nil {
		return nil, err
	}
	f, err := s.acquireTickLock(lockFile)
	if err != nil {
		if !isUnwritableErrno(err) {
			return nil, err
		}
		noteUnwritableAt(s.log, lockDir, err, "tick lock unavailable", "lock")
		s.sweepMCPOrphans()
		return nil, nil
	}
	return f, nil
}

// Persists the recurring advance before any run, dropping the recurring jobs when the store refuses it
func (s *Store) advanceOrDropRecurring(ctx context.Context, due []*Job) ([]*Job, error) {
	ids := make([]string, len(due))
	for i, j := range due {
		ids[i] = j.ID
	}
	_, err := s.advanceNextRuns(home.WithOverride(ctx, s.home), ids)
	if err == nil {
		return due, nil
	}
	if !isOSError(err) {
		return nil, err
	}
	var skipped, kept []*Job
	for _, j := range due {
		if isRecurring(j) {
			skipped = append(skipped, j)
		} else {
			kept = append(kept, j)
		}
	}
	storeHealthNoteUnwritable(s, err, fmt.Sprintf("skipped %d recurring job(s)", len(skipped)), "advance", skipped...)
	return kept, nil
}

// Admission reconcile, legacy pending drain and dead owner reap under the tick lock
func (s *Store) preDispatchHousekeeping(ctx context.Context, headless bool) error {
	if err := reconcilePending(ctx, s, !headless); err != nil {
		return err
	}
	if err := drainLegacyPending(ctx, s); err != nil {
		s.log.Warn(fmt.Sprintf("Legacy Bot Chat pending drain failed: %s", err))
	}
	s.maybeReapDeadOwners(ctx)
	return nil
}

// One process due result slot filled before the pool future completes
type tickRun struct {
	fut *poolFuture
	ok  bool
	err error
}

// Pointer identity of one in flight registration, never shared between submissions
type registrationOwner struct{ _ byte }

// Scans, advances and dispatches due jobs under the tick lock, returning how many ran
func (s *Store) tickAdmitted(ctx context.Context, o tickOptions) (int, error) {
	if boot, disk, ok := s.shouldYieldTickToFreshGateway(); ok {
		s.logTickYieldOnce(fmt.Sprintf("boot=%s disk=%s", boot, disk))
		return 0, &CronTickYielded{BootRev: boot, DiskRev: disk}
	}
	lockFD, err := s.acquireTickLockOrDegrade()
	if err != nil || lockFD == nil {
		return 0, err
	}
	defer releaseTickLock(lockFD)
	if estopCheckPaused != nil && estopCheckPaused("cron", s.log) {
		return 0, nil
	}
	if o.CanDispatch != nil && !o.CanDispatch() {
		s.log.Debug("Cron dispatch paused while gateway drains existing work")
		return 0, nil
	}
	if err := s.preDispatchHousekeeping(ctx, o.Headless); err != nil {
		return 0, err
	}
	s.maybeRunWorktreeMaintenance(ctx)
	due, err := s.getDueJobs(ctx)
	if err != nil {
		return 0, err
	}
	s.sweepStaleInflightForTick(home.WithOverride(ctx, s.home), due)
	if o.Headless {
		if due, err = refuseAgentJobsWithoutGateway(ctx, s, due); err != nil {
			return 0, err
		}
	}
	if len(due) == 0 {
		if o.Verbose {
			s.log.Info(fmt.Sprintf("%s - No jobs due", s.clk.now().Format("15:04:05")))
		}
		if err := s.recheckIdle(home.WithOverride(ctx, s.home)); err != nil {
			return 0, err
		}
		s.sweepMCPOrphans()
		return 0, nil
	}
	if s.dispatchBlocked(due) {
		s.sweepMCPOrphans()
		return 0, nil
	}
	if o.Verbose {
		s.log.Info(fmt.Sprintf("%s - %d job(s) due", s.clk.now().Format("15:04:05"), len(due)))
	}
	if due, err = s.advanceOrDropRecurring(home.WithOverride(ctx, s.home), due); err != nil {
		return 0, err
	}
	if len(due) == 0 {
		s.sweepMCPOrphans()
		return 0, nil
	}
	maxWorkers := s.resolveMaxParallelWorkers(ctx)
	if o.Verbose {
		cap := "unbounded"
		if maxWorkers != nil {
			cap = fmt.Sprint(*maxWorkers)
		}
		s.log.Info(fmt.Sprintf("Running %d job(s) in parallel (max_workers=%s)", len(due), cap))
	}
	if maxWorkers != nil && *maxWorkers <= 0 {
		return 0, errors.New("max_workers must be greater than 0")
	}
	process := func(j *Job) (bool, error) { return s.processDueJob(ctx, j, o.Adapters, o.Loop, o.Verbose) }
	pool := s.getParallelPool(maxWorkers)
	var runs []*tickRun
	count := 0
	for _, job := range due {
		run := s.submitWithGuard(ctx, job, pool, process)
		if run == nil {
			continue
		}
		runs = append(runs, run)
		if !o.Sync {
			count++
		}
	}
	if !o.Sync {
		s.sweepMCPOrphansWhenAllDone(runs)
		return count, nil
	}
	for _, run := range runs {
		<-run.fut.done
		if run.err != nil {
			s.log.Error(fmt.Sprintf("Cron job future failed: %s", run.err))
		} else if run.ok {
			count++
		}
	}
	s.sweepMCPOrphans()
	return count, nil
}

// Releases claims whose owner died before a terminal state, throttled per profile home
func (s *Store) maybeReapDeadOwners(ctx context.Context) {
	key := home.Key(s.Home())
	now := tickMonotonic()
	lastDeadOwnerReap.Lock()
	last, seen := lastDeadOwnerReap.at[key]
	if seen && now-last < deadOwnerReapIntervalSeconds {
		lastDeadOwnerReap.Unlock()
		return
	}
	lastDeadOwnerReap.at[key] = now
	lastDeadOwnerReap.Unlock()
	reclaimed, err := openExecutionsLedger(s).RecoverInterrupted(ctx)
	if err != nil {
		s.log.Debug(fmt.Sprintf("Dead-owner execution reclaim failed: %s", err))
		return
	}
	if reclaimed > 0 {
		s.log.Warn(fmt.Sprintf("Reclaimed %d cron execution(s) whose owner process died "+
			"before reaching a terminal state (marked unknown)", reclaimed))
	}
}

// Force releases leaked in flight claims before the dedup guard, every job when one in flight is not due
func (s *Store) sweepStaleInflightForTick(ctx context.Context, due []*Job) {
	local := home.Key(s.Home())
	inflight.Lock()
	inflightIDs := map[string]bool{}
	for k := range inflight.running {
		if k.Home == local {
			inflightIDs[k.JobID] = true
		}
	}
	inflight.Unlock()
	if len(inflightIDs) == 0 {
		return
	}
	sweep := due
	dueIDs := map[string]bool{}
	for _, j := range due {
		if j != nil {
			dueIDs[j.ID] = true
		}
	}
	for id := range inflightIDs {
		if !dueIDs[id] {
			if all, err := s.LoadJobs(home.WithOverride(ctx, s.home)); err == nil {
				sweep = all
			}
			break
		}
	}
	defer func() {
		if r := recover(); r != nil {
			s.log.Warn(fmt.Sprintf("Stale in-flight sweep failed: %v", r))
		}
	}()
	s.sweepStaleInflight(sweep)
}

// Integer value of a config scalar like Python int(), false when int() would raise
func pyIntCoerce(v any) (int, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, false
		}
		return int(math.Trunc(x)), true
	case string:
		n, ok := pyint.Parse(x)
		if !ok || !n.IsInt64() {
			return 0, false
		}
		return int(n.Int64()), true
	}
	n, ok := pyrepr.Int(v)
	if !ok || !n.IsInt64() {
		return 0, false
	}
	return int(n.Int64()), true
}

// Worker cap from the environment, then cron.max_parallel_jobs, nil for unbounded
func (s *Store) resolveMaxParallelWorkers(ctx context.Context) *int {
	if raw := pystr.Strip(s.cronEnvSetting(ctx, "ICEBATH_CRON_MAX_PARALLEL", "")); raw != "" {
		if n, ok := pyIntCoerce(raw); ok {
			if n == 0 {
				return nil
			}
			return &n
		}
		s.log.Warn("Invalid ICEBATH_CRON_MAX_PARALLEL value; defaulting to unbounded")
	}
	if v := s.cronConfigValue("max_parallel_jobs", nil); v != nil {
		if n, ok := pyIntCoerce(v); ok {
			if n == 0 {
				return nil
			}
			return &n
		}
	}
	return nil
}

// Reaps stdio MCP children flagged as orphans once the jobs that spawned them finished
func (s *Store) sweepMCPOrphans() {
	defer func() {
		if r := recover(); r != nil {
			s.log.Debug(fmt.Sprintf("Post-tick MCP orphan cleanup failed: %v", r))
		}
	}()
	mcp.KillOrphanedMCPChildren(false, "")
}

// Claims the fire when the worker starts and runs the claim snapshot through the shared run path
func (s *Store) processDueJob(ctx context.Context, job *Job, adapters, loop any, verbose bool) (bool, error) {
	ledger := openExecutionsLedger(s)
	execID := ""
	if job.ExecutionID != nil {
		execID = *job.ExecutionID
	}
	claimed, won, err := s.claimJobForFire(ctx, job.ID, FireClaimOptions{})
	if err != nil {
		if isOSError(err) {
			label := job.Name
			if label == "" {
				label = job.ID
			}
			storeHealthNoteUnwritable(s, err, fmt.Sprintf("skipped job '%s'", label), "claim", job)
			ledger.SettleUnstarted(ctx, execID, job.ID, "Cron store unwritable; not started: "+pypath.OSErrorText(err))
			return false, nil
		}
		ledger.SettleUnstarted(ctx, execID, job.ID, fmt.Sprintf("Fire claim failed: %s: %s", cronExceptionName(err), err))
		return false, err
	}
	if !won {
		msg := "Fire claim lost; execution was not started."
		if _, err := ledger.Finish(ctx, execID, false, &msg, nil, false); err != nil {
			return false, err
		}
		return true, nil
	}
	run := job.Clone()
	if claimed != nil {
		run = claimed.Clone()
	}
	run.ExecutionID = job.ExecutionID
	run.ScheduledInstant = job.ScheduledInstant
	return runOneJobSeam(ctx, s, run, runOneJobOptions{Adapters: adapters, Loop: loop, Verbose: verbose})
}

// Submits behind the in flight dedup guard, nil when a prior run is in flight or dispatch failed
func (s *Store) submitWithGuard(ctx context.Context, job *Job, pool *parallelPool, process func(*Job) (bool, error)) *tickRun {
	jobID := job.ID
	label := job.Name
	if label == "" {
		label = jobID
	}
	clearRunClaim := func() {
		if job.Kind() != "once" {
			return
		}
		if _, err := s.clearRunClaim(home.WithOverride(ctx, s.home), jobID); err != nil {
			s.log.Warn(fmt.Sprintf("Could not clear run_claim for job '%s' after dispatch "+
				"failure: %s (claim will expire at TTL)", label, err))
		}
	}
	notDispatchedShutdown := func() {
		s.log.Warn(fmt.Sprintf("Job '%s' not dispatched — interpreter is shutting down", label))
	}
	if interpreterShuttingDown(nil) {
		notDispatchedShutdown()
		clearRunClaim()
		return nil
	}
	owner := &registrationOwner{}
	if !s.tryRegisterRunningJob(jobID, owner, futurePending) {
		s.log.Info(fmt.Sprintf("Job '%s' already running — skipping", label))
		return nil
	}
	claimHome := s.Home()
	ledger := openExecutionsLedger(s)
	execution, err := tickCreateExecution(ledger, ctx, jobID, "builtin", job.ScheduledInstant)
	if err != nil {
		s.releaseRunningJob(jobID, claimHome, owner)
		clearRunClaim()
		s.log.Error(fmt.Sprintf("Job '%s' not dispatched: %s failed: %s", label, "execution creation", err))
		return nil
	}
	execID := recText(execution, "id")
	dispatched := job.Clone()
	dispatched.ExecutionID = &execID
	var deliver any
	if job.Deliver != nil {
		deliver = *job.Deliver
	}
	observability.NoteCronExecution(map[string]any{"execution_id": execID, "deliver": deliver})
	run := &tickRun{}
	fut, err := pool.Submit(func() {
		defer s.releaseRunningJob(jobID, claimHome, owner)
		defer func() {
			if r := recover(); r != nil {
				run.ok, run.err = false, fmt.Errorf("%v", r)
			}
		}()
		run.ok, run.err = process(dispatched)
	})
	if err != nil {
		s.releaseRunningJob(jobID, claimHome, owner)
		clearRunClaim()
		msg := fmt.Sprintf("Executor dispatch failed: %s", err)
		ledger.Finish(ctx, execID, false, &msg, nil, false)
		if interpreterShuttingDown(err) {
			notDispatchedShutdown()
		} else {
			s.log.Error(fmt.Sprintf("Job '%s' not dispatched: %s", label, err))
		}
		return nil
	}
	run.fut = fut
	inflight.Lock()
	if k := s.inflightKey(jobID, claimHome); inflight.owners[k] == any(owner) {
		inflight.futures[k] = fut
	}
	inflight.Unlock()
	return run
}

// Sweeps MCP orphans once the last dispatched run completes, inline when nothing was dispatched
func (s *Store) sweepMCPOrphansWhenAllDone(runs []*tickRun) {
	if len(runs) == 0 {
		s.sweepMCPOrphans()
		return
	}
	go func() {
		for _, run := range runs {
			<-run.fut.done
			if run.err != nil {
				s.log.Error(fmt.Sprintf("Cron job future failed in async mode: %s", run.err))
			}
		}
		s.sweepMCPOrphans()
	}()
}
