package cron

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"runtime/debug"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	emfileBackoffMaxSeconds    = 15 * 60
	DefaultMisfireGraceMinutes = 10
)

// One profile a multiplex ticker serves, the name nil for a bare home entry
type ProfileHome struct {
	Name *string
	Home string
}

// Inputs of a ticker start, ProfileHomesFn being the live enumerator a multiplex gateway passes
type StartOptions struct {
	Adapters            any
	Loop                any
	Interval            *float64
	CanDispatch         func() bool
	ProfileHomes        []ProfileHome
	ProfileHomesFn      func() ([]ProfileHome, error)
	ProfileAdapters     map[string]any
	DefaultProfile      *string
	ProfileGate         func(name *string, home string) bool
	ProfileBotPlatforms map[string]any
}

// Inputs of one fire, the keywords fire_due and fire_claimed take
type FireOptions struct {
	Adapters any
	Loop     any
	Force    bool
	Manual   bool
	Cancel   <-chan struct{}
}

// Inputs the ticker passes to one tick of a store
type tickOptions struct {
	Verbose     bool
	Adapters    any
	Loop        any
	Sync        bool
	CanDispatch func() bool
	Headless    bool
}

// Inputs the shared run path takes for one claimed fire
type runOneJobOptions struct {
	Adapters any
	Loop     any
	Cancel   <-chan struct{}
	// Per fire context appended to the stored prompt, never persisted
	ExtraPrompt *string
	Verbose     bool
}

// Decides when a due cron job fires, only the name and start are required
type CronScheduler interface {
	Name() string
	Start(ctx context.Context, stop <-chan struct{}, o StartOptions)
}

// Optional provider hooks, each with the default the dispatcher below applies when absent
type (
	availabilityHook interface{ IsAvailable() bool }
	stopHook         interface{ Stop() }
	jobsChangedHook  interface{ OnJobsChanged() error }
	registerJobHook  interface{ RegisterJob(job *Job) error }
	recoverHook      interface {
		RecoverInterrupted(ctx context.Context) (int, error)
	}
	reconcileHook interface{ Reconcile() error }
	fireDueHook   interface {
		FireDue(ctx context.Context, jobID string, o FireOptions) (bool, error)
	}
	claimFireHook interface {
		ClaimFire(ctx context.Context, jobID string, force, manual bool) (*Job, error)
	}
	fireClaimedHook interface {
		FireClaimed(ctx context.Context, job *Job, o FireOptions) (bool, error)
	}
	fireDueKeywordsHook interface {
		FireDueKeywords() (names []string, varKeyword bool)
	}
)

type cronStoreKey struct{}

// Binds the cron store a block operates on, the use_cron_store scope
func withCronStore(ctx context.Context, s *Store) context.Context {
	return context.WithValue(ctx, cronStoreKey{}, s)
}

// Store bound by the enclosing scope, else the store of the ambient home
func cronStoreFrom(ctx context.Context) *Store {
	if s, ok := ctx.Value(cronStoreKey{}).(*Store); ok && s != nil {
		return s
	}
	return openCronStore(home.Dir(ctx), nil)
}

// Plain interval while healthy, doubled per fd exhaustion failure and capped
func backoffWaitSeconds(interval float64, failures int) float64 {
	if failures <= 0 {
		return interval
	}
	return math.Min(interval*math.Pow(2, float64(failures-1)), emfileBackoffMaxSeconds)
}

// Reclaims fds and bumps the backoff on fd exhaustion, any other failure resets it
func noteTickFailure(err error, failures int) int {
	if isFDExhaustion(err) {
		reclaimFDs()
		return failures + 1
	}
	return 0
}

// Runs a ticker marker write so a failing store degrades to a warning instead of ending the ticker
func (s *Store) guardedStoreWrite(description string, action func()) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Warn(fmt.Sprintf("Cron %s write failed: %v", description, r))
		}
	}()
	action()
}

// Profile homes still on disk, a failing live enumerator yielding none for this cycle
func (s *Store) existingProfileHomes(list []ProfileHome, fn func() ([]ProfileHome, error)) []ProfileHome {
	if fn != nil {
		var err error
		list, err = func() (out []ProfileHome, err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("%v", r)
				}
			}()
			return fn()
		}()
		if err != nil {
			s.log.Warn(fmt.Sprintf("cron profile enumeration failed; skipping this cycle: %v", err))
			return []ProfileHome{}
		}
	}
	out := []ProfileHome{}
	for _, entry := range list {
		if st, err := os.Stat(entry.Home); err == nil && st.IsDir() {
			out = append(out, entry)
		}
	}
	return out
}

// True when a fire runs for a profile other than the launch profile of this process
func routedProfileFire(ctx context.Context, h *string) bool {
	target := home.Dir(ctx)
	if h != nil {
		target = *h
	}
	return home.Key(target) != home.Key(home.RoutingProcess())
}

// Scopes a block to one profile home and its cron store
func (s *Store) profileCronScope(ctx context.Context, h string) (context.Context, *Store) {
	var now func() time.Time
	if s.clk != nil {
		now = s.clk.nowFn
	}
	ps := openCronStore(h, now)
	ps.log = s.log
	return withCronStore(home.WithOverride(ctx, h), ps), ps
}

// Whether the provider can run here, true unless it says otherwise
func schedulerIsAvailable(p CronScheduler) bool {
	if h, ok := p.(availabilityHook); ok {
		return h.IsAvailable()
	}
	return true
}

// Eager teardown when the provider offers one
func schedulerStop(p CronScheduler) {
	if h, ok := p.(stopHook); ok {
		h.Stop()
	}
}

// Store mutation notice for providers that reconcile an external registry
func schedulerOnJobsChanged(p CronScheduler) error {
	if h, ok := p.(jobsChangedHook); ok {
		return h.OnJobsChanged()
	}
	return nil
}

// External trigger registration of a newly persisted job, nothing for the built in ticker
func schedulerRegisterJob(p CronScheduler, job *Job) error {
	if h, ok := p.(registerJobHook); ok {
		return h.RegisterJob(job)
	}
	return nil
}

// Profile local attempt recovery, the executions ledger recovery by default
func schedulerRecoverInterrupted(ctx context.Context, p CronScheduler) (int, error) {
	if h, ok := p.(recoverHook); ok {
		return h.RecoverInterrupted(ctx)
	}
	return recoverInterruptedExecutions(ctx, cronStoreFrom(ctx))
}

// Converges the external registry toward jobs.json, nothing by default
func schedulerReconcile(p CronScheduler) error {
	if h, ok := p.(reconcileHook); ok {
		return h.Reconcile()
	}
	return nil
}

// Claims then runs one job now, false when the claim was lost or the job is gone
func schedulerFireDue(ctx context.Context, p CronScheduler, jobID string, o FireOptions) (bool, error) {
	if h, ok := p.(fireDueHook); ok {
		return h.FireDue(ctx, jobID, o)
	}
	claimed, err := schedulerClaimFire(ctx, p, jobID, o.Force, o.Manual)
	if err != nil || claimed == nil {
		return false, err
	}
	return schedulerFireClaimed(ctx, p, claimed, FireOptions{Adapters: o.Adapters, Loop: o.Loop})
}

// Durably claims one fire and creates its audit attempt before the transport acknowledges
func schedulerClaimFire(ctx context.Context, p CronScheduler, jobID string, force, manual bool) (*Job, error) {
	if h, ok := p.(claimFireHook); ok {
		return h.ClaimFire(ctx, jobID, force, manual)
	}
	return baseClaimFire(ctx, p, jobID, force, manual)
}

// Base class claim_fire and fire_claimed, the module attributes provider tests patch
var (
	baseClaimFire   = defaultClaimFire
	baseFireClaimed = defaultFireClaimed
)

// Store claim and ledger attempt the base claim reaches, the cron.jobs and cron.executions attributes tests patch
var (
	providerClaimJobForFire = (*Store).claimJobForFire
	providerCreateExecution = func(ctx context.Context, s *Store, jobID, source string) (map[string]any, error) {
		return openExecutionsLedger(s).Create(ctx, jobID, source, nil)
	}
)

func defaultClaimFire(ctx context.Context, p CronScheduler, jobID string, force, manual bool) (*Job, error) {
	return claimFireForSource(ctx, p.Name(), jobID, force, manual)
}

// Error form of a recovered panic value, the value itself when it already is one
func recoveredError(r any) error {
	if err, ok := r.(error); ok {
		return err
	}
	return fmt.Errorf("%v", r)
}

// Creates the audit attempt under source, claims the fire and stamps the attempt on the snapshot
func claimFireForSource(ctx context.Context, source, jobID string, force, manual bool) (*Job, error) {
	s := cronStoreFrom(ctx)
	ledger := openExecutionsLedger(s)
	execution, err := providerCreateExecution(ctx, s, jobID, source)
	if err != nil {
		return nil, err
	}
	executionID := config.PyStr(execution["id"])
	var claimed *Job
	var ok bool
	var claimErr error
	panicked, raised := func() (panicked bool, raised any) {
		defer func() {
			if r := recover(); r != nil {
				panicked, raised = true, r
			}
		}()
		claimed, ok, claimErr = providerClaimJobForFire(s, ctx, jobID, FireClaimOptions{Force: force, Manual: manual})
		if claimErr == nil && ok && claimed != nil {
			claimErr = ledger.SetOccurrence(ctx, executionID, claimed.ScheduledInstant)
		}
		return false, nil
	}()
	if panicked || claimErr != nil {
		cause := claimErr
		if panicked {
			cause = recoveredError(raised)
		}
		reason := fmt.Sprintf("Fire claim failed before dispatch: %s: %v", cronExceptionName(cause), cause)
		if _, err := ledger.Finish(ctx, executionID, false, &reason, nil, false); err != nil {
			return nil, err
		}
		if panicked {
			panic(raised)
		}
		return nil, claimErr
	}
	if !ok || claimed == nil {
		reason := "Fire claim was not acquired"
		_, err := ledger.Finish(ctx, executionID, false, &reason, nil, false)
		return nil, err
	}
	claimed.ExecutionID = &executionID
	return claimed, nil
}

// Runs an exact claim snapshot through the shared run path
func schedulerFireClaimed(ctx context.Context, p CronScheduler, job *Job, o FireOptions) (bool, error) {
	if h, ok := p.(fireClaimedHook); ok {
		return h.FireClaimed(ctx, job, o)
	}
	return baseFireClaimed(ctx, p, job, o)
}

func defaultFireClaimed(ctx context.Context, p CronScheduler, job *Job, o FireOptions) (bool, error) {
	if _, err := runOneJobSeam(ctx, cronStoreFrom(ctx), job, runOneJobOptions{Adapters: o.Adapters, Loop: o.Loop, Cancel: o.Cancel}); err != nil {
		return false, err
	}
	return true, nil
}

// Whether fire_due can safely receive the force keyword
func providerSupportsForceFire(p CronScheduler) bool { return providerFireDueAccepts(p, "force") }

// Whether the provider fire_due takes the keyword, read from a hosted signature when it reports one
func providerFireDueAccepts(p CronScheduler, name string) bool {
	if h, ok := p.(fireDueKeywordsHook); ok {
		names, varKeyword := h.FireDueKeywords()
		if varKeyword {
			return true
		}
		for _, n := range names {
			if n == name {
				return true
			}
		}
		return false
	}
	switch name {
	case "job_id", "adapters", "loop", "force", "manual":
		return true
	}
	return false
}

// Whether the provider implements the two phase fire contract instead of a legacy fire_due only override
func providerSupportsSplitFire(p CronScheduler) bool {
	if h, ok := p.(interface{ SupportsSplitFire() bool }); ok {
		return h.SupportsSplitFire()
	}
	_, claim := p.(claimFireHook)
	_, fired := p.(fireClaimedHook)
	if claim || fired {
		return true
	}
	_, due := p.(fireDueHook)
	return !due
}

// Configured misfire grace in minutes, the default when unset or unreadable
func (s *Store) misfireGraceMinutes() float64 {
	cfg, err := s.loadConfig()
	if err != nil {
		return DefaultMisfireGraceMinutes
	}
	raw, ok := cfg.Lookup("cron.misfire_grace_minutes")
	if !ok {
		return DefaultMisfireGraceMinutes
	}
	v, err := pyint.Float(raw)
	if err != nil {
		return DefaultMisfireGraceMinutes
	}
	return v
}

// Estop check the agent package registers, unset reading as the Python import guard that skips the check
var estopCheckPaused func(ctx context.Context, component string, log *slog.Logger) bool

// Installs the estop check every cron entry point consults before dispatching
func RegisterEstopCheck(fn func(ctx context.Context, component string, log *slog.Logger) bool) {
	estopCheckPaused = fn
}

// Fires jobs whose external fire never arrived once the misfire grace passed, returning how many dispatched
func fireOverdueJobs(ctx context.Context, p CronScheduler, o FireOptions, now *time.Time) (int, error) {
	s := cronStoreFrom(ctx)
	if estopCheckPaused != nil && estopCheckPaused(ctx, "cron-misfire", s.log) {
		return 0, nil
	}
	if _, builtin := p.(*InProcessCronScheduler); builtin {
		return 0, nil
	}
	grace := s.misfireGraceMinutes()
	if grace <= 0 {
		return 0, nil
	}
	at := s.clk.now()
	if now != nil {
		at = *now
	}
	jobs, err := s.LoadJobs(home.WithOverride(ctx, s.home))
	if err != nil {
		return 0, err
	}
	fired := 0
	for _, job := range jobs {
		if !isJobRunnable(job) || job.NextRunAt == nil || *job.NextRunAt == "" {
			continue
		}
		due, ok := s.clk.parseAware(*job.NextRunAt, nil)
		if !ok {
			continue
		}
		overdue := elapsedSeconds(at, due)
		if overdue < grace*60 {
			continue
		}
		name := job.Name
		if name == "" {
			name = "unnamed"
		}
		if job.scheduleOrEmpty().Kind == "once" && overdue > oneshotGraceSeconds {
			s.log.Warn(fmt.Sprintf("Misfire catch-up: one-shot job %s (%s) was due %s (%s min overdue) — outside the %ds one-shot grace window, not firing.", job.ID, name, *job.NextRunAt, pyFormatFixed0(overdue/60), oneshotGraceSeconds))
			continue
		}
		s.log.Warn(fmt.Sprintf("Misfire catch-up: job %s (%s) was due %s (%s min overdue) and no external fire arrived — firing locally.", job.ID, name, *job.NextRunAt, pyFormatFixed0(overdue/60)))
		claimed, err := schedulerClaimFire(ctx, p, job.ID, false, false)
		if err != nil {
			s.log.Warn(fmt.Sprintf("Misfire catch-up failed for job %s: %s: %v", job.ID, cronExceptionName(err), err))
			continue
		}
		if claimed == nil {
			continue
		}
		go runMisfireFire(context.WithoutCancel(ctx), p, claimed, o, "cron-misfire-"+string([]rune(job.ID)[:min(12, len([]rune(job.ID)))]))
		fired++
	}
	return fired, nil
}

// Runs one claimed misfire fire off the caller, an escaping failure printed as threading.excepthook prints it
func runMisfireFire(ctx context.Context, p CronScheduler, claimed *Job, o FireOptions, thread string) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "Exception in thread %s:\n%v\n%s", thread, r, debug.Stack())
		}
	}()
	if _, err := schedulerFireClaimed(ctx, p, claimed, FireOptions{Adapters: o.Adapters, Loop: o.Loop}); err != nil {
		fmt.Fprintf(os.Stderr, "Exception in thread %s:\n%s: %v\n", thread, cronExceptionName(err), err)
	}
}

// Python format of a float with zero decimals, round half to even like %.0f
func pyFormatFixed0(v float64) string {
	out, err := pyrepr.Format(v, ".0f")
	if err != nil {
		panic(err)
	}
	return out
}

// Configured cron.provider, the built in ticker when unset, unknown, unavailable or failing to load
func resolveCronScheduler(s *Store) CronScheduler {
	name := ""
	if cfg, err := s.loadConfig(); err == nil {
		if raw, ok := cfg.Lookup("cron.provider"); ok {
			if text, isStr := raw.(string); isStr {
				name = pystr.Strip(text)
			}
		}
	}
	if name == "" || name == "builtin" || name == "in-process" || name == "inprocess" {
		return &InProcessCronScheduler{}
	}
	provider, warning := s.loadConfiguredScheduler(name)
	if provider != nil {
		return provider
	}
	s.log.Warn(warning)
	return &InProcessCronScheduler{}
}

// Loaded, available provider of the name, else the warning naming why the built in ticker stays
func (s *Store) loadConfiguredScheduler(name string) (provider CronScheduler, warning string) {
	failed := func(err any) (CronScheduler, string) {
		return nil, fmt.Sprintf("Failed to load cron.provider '%s' (%v); using built-in ticker", name, err)
	}
	defer func() {
		if r := recover(); r != nil {
			provider, warning = failed(r)
		}
	}()
	provider, err := loadCronScheduler(s, name)
	if err != nil {
		return failed(err)
	}
	if provider == nil {
		return nil, fmt.Sprintf("cron.provider '%s' not found; using built-in ticker", name)
	}
	checked, hosted := provider.(checkedScheduler)
	available := false
	if hosted {
		if available, err = checked.checkAvailable(); err != nil {
			return failed(err)
		}
	} else {
		available = schedulerIsAvailable(provider)
	}
	if !available {
		return nil, fmt.Sprintf("cron.provider '%s' not available; using built-in ticker", name)
	}
	label := ""
	if hosted {
		if label, err = checked.checkName(); err != nil {
			return failed(err)
		}
	} else {
		label = provider.Name()
	}
	s.log.Info("Using cron scheduler provider: " + label)
	return provider, ""
}

// Resolver the create and notify paths go through, swapped by tests for a recording provider
var schedulerResolver = resolveCronScheduler

// External providers cannot reconcile several profile stores, so multiplex falls back to the built in ticker
func (s *Store) schedulerForProfileMode(p CronScheduler, multiplexProfiles bool) CronScheduler {
	if _, builtin := p.(*InProcessCronScheduler); !multiplexProfiles || builtin {
		return p
	}
	s.log.Warn(fmt.Sprintf("cron.provider '%s' does not support multiplex_profiles; using built-in ticker", p.Name()))
	return &InProcessCronScheduler{}
}

// Default in process ticker whose start blocks until the stop channel closes
type InProcessCronScheduler struct{}

func (*InProcessCronScheduler) Name() string { return "builtin" }

// Python class name and text of a tick failure as the ticker error marker records it
func tickErrorText(err error) string {
	return fmt.Sprintf("%s: %v", cronExceptionName(err), err)
}

// Waits until the next tick instant or the stop signal, reporting whether stop arrived
func waitTick(stop <-chan struct{}, d float64) bool {
	if d <= 0 {
		select {
		case <-stop:
			return true
		default:
			return false
		}
	}
	t := time.NewTimer(time.Duration(d * float64(time.Second)))
	defer t.Stop()
	select {
	case <-stop:
		return true
	case <-t.C:
		return false
	}
}

// Wait between ticker cycles, the stop event wait hermes tests replace to run one cycle
var tickerWait = waitTick

func stopped(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

// Runs one tick under a recover so a panicking tick counts as a failed tick
func containedTick(ctx context.Context, s *Store, o tickOptions) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = recoveredError(r)
		}
	}()
	_, err = tickHook(ctx, s, o)
	return err
}

// Seam over the tick the ticker drives, swapped by tests like a patched scheduler.tick
var tickHook = schedulerTick

// Seam over the monotonic clock the ticker schedules against
var tickerMonotonic = func() float64 { return time.Since(tickerClockStart).Seconds() }

// Origin of the ticker monotonic clock
var tickerClockStart = time.Now()

// Ticker liveness marker write, the cron.jobs record_ticker_heartbeat attribute tests patch
var recordTickerHeartbeat = (*Store).RecordTickerHeartbeat

// Ticks the process home every interval until stopped, recording heartbeat, error and backoff
func (p *InProcessCronScheduler) Start(ctx context.Context, stop <-chan struct{}, o StartOptions) {
	s := cronStoreFrom(ctx)
	interval := 60.0
	if o.Interval != nil {
		interval = *o.Interval
	}
	s.log.Info(fmt.Sprintf("In-process cron scheduler started (interval=%ds)", int(interval)))
	if o.ProfileHomesFn != nil || len(o.ProfileHomes) > 0 {
		p.startMultiplex(ctx, s, stop, o, interval)
		return
	}
	registerTickedHomes([]string{home.Process()})
	func() {
		defer func() {
			if r := recover(); r != nil {
				err := recoveredError(r)
				s.log.Error(fmt.Sprintf("Cron startup recovery error: %v", err))
				s.guardedStoreWrite("startup error", func() { s.RecordTickerError(home.WithOverride(ctx, s.home), tickErrorText(err)) })
			}
		}()
		recovered, err := schedulerRecoverInterrupted(ctx, p)
		if err != nil {
			s.log.Error(fmt.Sprintf("Cron startup recovery error: %v", err))
			s.guardedStoreWrite("startup error", func() { s.RecordTickerError(home.WithOverride(ctx, s.home), tickErrorText(err)) })
			return
		}
		if recovered > 0 {
			s.log.Warn(fmt.Sprintf("Marked %d interrupted cron execution(s) unknown after restart", recovered))
		}
		recordTickerHeartbeat(s, home.WithOverride(ctx, s.home), false)
	}()
	failures := 0
	next := tickerMonotonic()
	for !stopped(stop) {
		ok := false
		err := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = recoveredError(r)
				}
			}()
			if o.CanDispatch != nil && !o.CanDispatch() {
				s.log.Debug("Cron dispatch paused while gateway drains existing work")
				return s.noteHeld(home.WithOverride(ctx, s.home), nil)
			}
			s.noteDispatching()
			return containedTick(ctx, s, tickOptions{Verbose: false, Adapters: o.Adapters, Loop: o.Loop, Sync: false, CanDispatch: o.CanDispatch})
		}()
		if err == nil {
			ok = true
		} else {
			var yielded *CronTickYielded
			if errors.As(err, &yielded) {
				s.log.Info(fmt.Sprintf("Cron tick yielded: %v", err))
			} else {
				s.log.Error(fmt.Sprintf("Cron tick error: %v", err))
			}
			s.guardedStoreWrite("tick error", func() { s.RecordTickerError(home.WithOverride(ctx, s.home), tickErrorText(err)) })
			failures = noteTickFailure(err, failures)
		}
		s.guardedStoreWrite("heartbeat", func() { recordTickerHeartbeat(s, home.WithOverride(ctx, s.home), ok) })
		if ok {
			s.guardedStoreWrite("error clear", s.ClearTickerError)
			failures = 0
		}
		wait := backoffWaitSeconds(interval, failures)
		next += wait
		now := tickerMonotonic()
		if next < now {
			next = now + wait
		}
		if tickerWait(stop, math.Max(0, next-now)) {
			return
		}
	}
}

// Adapters a profile ticks with, shared routes only for a secondary without its own credential platform
func (s *Store) tickAdaptersFor(ctx context.Context, o StartOptions, name *string) any {
	if name == nil || (o.DefaultProfile != nil && *name == *o.DefaultProfile) {
		return o.Adapters
	}
	var tickAdapters any = map[string]any{}
	if v, ok := o.ProfileAdapters[*name]; ok && pyTruthy(v) {
		tickAdapters = v
	}
	owned := o.ProfileBotPlatforms[*name]
	if !hasCredentialPlatform(tickAdapters, owned) && pyTruthy(o.Adapters) {
		return newSharedRouteAdapters(o.Adapters, primaryProfileRoutesForCurrentHome(ctx))
	}
	return tickAdapters
}

// Ticks every profile store each cycle in its own scope, isolating one profile failure from the rest
func (p *InProcessCronScheduler) startMultiplex(ctx context.Context, s *Store, stop <-chan struct{}, o StartOptions, interval float64) {
	initial := s.existingProfileHomes(o.ProfileHomes, o.ProfileHomesFn)
	homes := make([]string, 0, len(initial))
	labels := make([]any, 0, len(initial))
	for _, e := range initial {
		homes = append(homes, e.Home)
		if e.Name != nil {
			labels = append(labels, *e.Name)
		} else {
			labels = append(labels, e.Home)
		}
	}
	registerTickedHomes(homes)
	suffix := ""
	if o.ProfileHomesFn != nil {
		suffix = " (re-enumerated every cycle)"
	}
	s.log.Info(fmt.Sprintf("Multiplex cron scheduler started for %d profile(s): %s%s", len(initial), pyrepr.Repr(labels), suffix))
	for _, e := range initial {
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.log.Error(fmt.Sprintf("Cron startup recovery error for profile at %s: %v", e.Home, r))
				}
			}()
			pctx, ps := s.profileCronScope(ctx, e.Home)
			recovered, err := schedulerRecoverInterrupted(pctx, p)
			if err != nil {
				s.log.Error(fmt.Sprintf("Cron startup recovery error for profile at %s: %v", e.Home, err))
				return
			}
			if recovered > 0 {
				s.log.Warn(fmt.Sprintf("Marked %d interrupted cron execution(s) for profile at %s", recovered, e.Home))
			}
			recordTickerHeartbeat(ps, home.WithOverride(pctx, ps.home), false)
		}()
	}
	failures := 0
	next := tickerMonotonic()
	for !stopped(stop) {
		ok := false
		tickErr := ""
		profileErrors := map[string]string{}
		var cycleErr error
		cycle := []ProfileHome{}
		func() {
			defer func() {
				if r := recover(); r != nil {
					err := recoveredError(r)
					s.log.Error(fmt.Sprintf("Cron profile enumeration error: %v", err))
					tickErr = tickErrorText(err)
					failures = noteTickFailure(err, failures)
				}
			}()
			enumerated := s.existingProfileHomes(o.ProfileHomes, o.ProfileHomesFn)
			if o.ProfileGate != nil {
				gated := []ProfileHome{}
				for _, e := range enumerated {
					if o.ProfileGate(e.Name, e.Home) {
						gated = append(gated, e)
					}
				}
				enumerated = gated
			}
			cycle = enumerated
			hs := make([]string, 0, len(cycle))
			for _, e := range cycle {
				hs = append(hs, e.Home)
			}
			registerTickedHomes(hs)
		}()
		func() {
			defer func() {
				if r := recover(); r != nil {
					err := recoveredError(r)
					s.log.Error(fmt.Sprintf("Cron tick error: %v", err))
					tickErr = tickErrorText(err)
					failures = noteTickFailure(err, failures)
				}
			}()
			if o.CanDispatch != nil && !o.CanDispatch() {
				s.log.Debug("Cron dispatch paused while gateway drains existing work")
				hs := make([]string, 0, len(cycle))
				for _, e := range cycle {
					hs = append(hs, e.Home)
				}
				if err := noteHeldProfiles(home.WithOverride(ctx, s.home), hs); err != nil {
					s.log.Error(fmt.Sprintf("Cron tick error: %v", err))
					tickErr = tickErrorText(err)
					failures = noteTickFailure(err, failures)
				}
				return
			}
			for _, e := range cycle {
				err := func() (err error) {
					defer func() {
						if r := recover(); r != nil {
							err = recoveredError(r)
						}
					}()
					pctx, ps := s.profileCronScope(ctx, e.Home)
					ps.noteDispatching()
					return containedTick(pctx, ps, tickOptions{Verbose: false, Adapters: s.tickAdaptersFor(pctx, o, e.Name), Loop: o.Loop, Sync: false, CanDispatch: o.CanDispatch})
				}()
				if err == nil {
					continue
				}
				var yielded *CronTickYielded
				if errors.As(err, &yielded) {
					s.log.Info(fmt.Sprintf("Cron tick yielded for profile at %s: %v", e.Home, err))
				} else {
					s.log.Error(fmt.Sprintf("Cron tick error for profile at %s: %v", e.Home, err))
					if cycleErr == nil || isFDExhaustion(err) {
						cycleErr = err
					}
				}
				profileErrors[e.Home] = tickErrorText(err)
			}
			ok = len(profileErrors) == 0
			if cycleErr != nil {
				failures = noteTickFailure(cycleErr, failures)
			}
		}()
		for _, e := range cycle {
			_, ps := s.profileCronScope(ctx, e.Home)
			perr, failed := profileErrors[e.Home]
			homeOK := tickErr == "" && !failed
			ps.guardedStoreWrite("heartbeat", func() { recordTickerHeartbeat(ps, home.WithOverride(ctx, ps.home), homeOK) })
			switch {
			case homeOK:
				ps.guardedStoreWrite("error clear", ps.ClearTickerError)
			case failed:
				ps.guardedStoreWrite("tick error", func() { ps.RecordTickerError(home.WithOverride(ctx, ps.home), perr) })
			case tickErr != "":
				ps.guardedStoreWrite("tick error", func() { ps.RecordTickerError(home.WithOverride(ctx, ps.home), tickErr) })
			}
		}
		if ok {
			failures = 0
		}
		wait := backoffWaitSeconds(interval, failures)
		next += wait
		now := tickerMonotonic()
		if next < now {
			next = now + wait
		}
		if tickerWait(stop, math.Max(0, next-now)) {
			return
		}
	}
}
