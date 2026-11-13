package cron

import (
	"context"
	"math"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Floor in minutes of the stale in flight allowance
const inflightMinAllowanceMinutes = 30.0

// Identity of one in flight run, the home key and the job id
type inflightKey struct {
	Home  string
	JobID string
}

// Fire owner registered by a run, its owner token and the home it runs under
type fireOwnerEntry struct {
	Owner *string
	Home  string
}

// Future slot of a registration whose pool future does not exist yet
var futurePending = &poolFuture{}

// Clock of the in flight registry, swapped by tests
var inflightNow = time.Now

// Process wide registry of in flight cron runs across every ticked home
var inflight = struct {
	sync.Mutex
	homePaths     map[string]string
	running       map[inflightKey]bool
	fireOwners    map[inflightKey]map[any]fireOwnerEntry
	workerPIDs    map[inflightKey]int
	scopeIsolated map[inflightKey]bool
	since         map[inflightKey]float64
	allowance     map[inflightKey]float64
	futures       map[inflightKey]*poolFuture
	owners        map[inflightKey]any
}{
	homePaths:     map[string]string{},
	running:       map[inflightKey]bool{},
	fireOwners:    map[inflightKey]map[any]fireOwnerEntry{},
	workerPIDs:    map[inflightKey]int{},
	scopeIsolated: map[inflightKey]bool{},
	since:         map[inflightKey]float64{},
	allowance:     map[inflightKey]float64{},
	futures:       map[inflightKey]*poolFuture{},
	owners:        map[inflightKey]any{},
}

// Seconds since the epoch on the registry clock
func inflightEpoch() float64 {
	return float64(inflightNow().UnixNano()) / 1e9
}

// Key of a run of the job in a home, the store home when none is given
func (s *Store) inflightKey(jobID string, h string) inflightKey {
	if h == "" {
		h = s.Home()
	}
	return inflightKey{Home: home.Key(h), JobID: jobID}
}

// Records a home under its key so a claim maps back to a usable path
func rememberInflightHome(h string) string {
	inflight.Lock()
	inflight.homePaths[home.Key(h)] = h
	inflight.Unlock()
	return h
}

// Real home path of an in flight key's home half, the key itself when unknown
func inflightHomePath(homeKey string) string {
	inflight.Lock()
	defer inflight.Unlock()
	if p, ok := inflight.homePaths[homeKey]; ok && p != "" {
		return p
	}
	return homeKey
}

// Registered and fire owned keys, the caller holding the lock
func inflightKeysLocked() map[inflightKey]bool {
	keys := make(map[inflightKey]bool, len(inflight.running)+len(inflight.fireOwners))
	for k := range inflight.running {
		keys[k] = true
	}
	for k := range inflight.fireOwners {
		keys[k] = true
	}
	return keys
}

// Job ids executing in this process across every home, for the shutdown drain
func getRunningJobIDs() map[string]bool {
	inflight.Lock()
	defer inflight.Unlock()
	out := map[string]bool{}
	for k := range inflightKeysLocked() {
		out[k.JobID] = true
	}
	return out
}

// RunningJobDetail describes one in flight run for the drain report
type RunningJobDetail struct {
	JobID       string
	ElapsedS    *float64
	WorkerPID   *int
	RestartSafe bool
}

// Per in flight run its job id, elapsed seconds, external worker pid and restart safety, sorted by key
func getRunningJobDetails() []RunningJobDetail {
	now := inflightEpoch()
	inflight.Lock()
	defer inflight.Unlock()
	keys := sortedInflightKeys(inflightKeysLocked())
	out := make([]RunningJobDetail, 0, len(keys))
	for _, k := range keys {
		d := RunningJobDetail{JobID: k.JobID, RestartSafe: inflight.scopeIsolated[k]}
		if started, ok := inflight.since[k]; ok {
			v := pyint.Round(now-started, 1)
			d.ElapsedS = &v
		}
		if pid, ok := inflight.workerPIDs[k]; ok {
			d.WorkerPID = &pid
		}
		out = append(out, d)
	}
	return out
}

// Keys in tuple order, home key then job id
func sortedInflightKeys(set map[inflightKey]bool) []inflightKey {
	keys := make([]inflightKey, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Home != keys[j].Home {
			return keys[i].Home < keys[j].Home
		}
		return keys[i].JobID < keys[j].JobID
	})
	return keys
}

// Job ids in flight longer than their stale allowance, the runs a restart drain skips
func (s *Store) getWedgedJobIDs(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	for k := range s.wedgedInflightKeys(home.WithOverride(ctx, s.home)) {
		out[k.JobID] = true
	}
	return out
}

// In flight keys older than the larger of twice their interval and the configured floor
func (s *Store) wedgedInflightKeys(ctx context.Context) map[inflightKey]bool {
	now := inflightEpoch()
	inflight.Lock()
	ages := map[inflightKey]float64{}
	allowances := map[inflightKey]float64{}
	for k, started := range inflight.since {
		if inflight.running[k] {
			ages[k] = now - started
		}
	}
	for k := range ages {
		if a, ok := inflight.allowance[k]; ok {
			allowances[k] = a
		}
	}
	inflight.Unlock()
	if len(ages) == 0 {
		return map[inflightKey]bool{}
	}
	floorSeconds := s.inflightMinAllowanceMinutes() * 60.0
	var unresolved []inflightKey
	for k := range ages {
		if _, ok := allowances[k]; !ok {
			unresolved = append(unresolved, k)
		}
	}
	if len(unresolved) > 0 {
		byID := map[string]*Job{}
		if jobs, err := s.LoadJobs(home.WithOverride(ctx, s.home)); err == nil {
			for _, j := range jobs {
				byID[j.ID] = j
			}
		}
		localHome := home.Key(s.Home())
		inflight.Lock()
		for _, k := range unresolved {
			allowance := floorSeconds
			var localJob *Job
			if k.Home == localHome {
				localJob = byID[k.JobID]
			}
			if minutes, ok := s.jobIntervalMinutes(localJob); ok {
				allowance = math.Max(allowance, 2.0*minutes*60.0)
			}
			allowances[k] = allowance
			if inflight.running[k] {
				inflight.allowance[k] = allowance
			}
		}
		inflight.Unlock()
	}
	out := map[inflightKey]bool{}
	for k, age := range ages {
		if age >= math.Max(allowances[k], floorSeconds) {
			out[k] = true
		}
	}
	return out
}

// RestartWaitCounts splits in flight runs into awaitable, wedged and restart safe for the restart wait
type RestartWaitCounts struct {
	Awaitable   int
	Wedged      int
	RestartSafe int
}

// Disjoint per key counts of runs a restart waits on, skips as wedged, or leaves to their own scope
func (s *Store) getRestartWaitCronCounts(ctx context.Context) RestartWaitCounts {
	wedged := s.wedgedInflightKeys(home.WithOverride(ctx, s.home))
	inflight.Lock()
	all := inflightKeysLocked()
	restartSafe := map[inflightKey]bool{}
	for k := range inflight.scopeIsolated {
		if all[k] && !wedged[k] {
			restartSafe[k] = true
		}
	}
	inflight.Unlock()
	counts := RestartWaitCounts{RestartSafe: len(restartSafe)}
	for k := range all {
		switch {
		case wedged[k]:
			counts.Wedged++
		case !restartSafe[k]:
			counts.Awaitable++
		}
	}
	return counts
}

// Whether this process has an in flight run of the job for the store home
func isJobRunning(s *Store, jobID string) bool {
	return s.isJobRunningIn(jobID, "")
}

// Whether this process has an in flight run of the job for a home, the store home when empty
func (s *Store) isJobRunningIn(jobID, h string) bool {
	k := s.inflightKey(jobID, h)
	inflight.Lock()
	defer inflight.Unlock()
	_, owned := inflight.fireOwners[k]
	return inflight.running[k] || owned
}

// Records the external worker pid holding a run and whether its own scope isolates it
func (s *Store) recordExternalCronWorker(jobID string, pid any, scopeIsolated bool) {
	k := s.inflightKey(jobID, "")
	inflight.Lock()
	defer inflight.Unlock()
	if n, err := pyint.Of(pid); err == nil && n.IsInt64() {
		inflight.workerPIDs[k] = int(n.Int64())
	}
	if scopeIsolated {
		inflight.scopeIsolated[k] = true
	}
}

// Adds the job to the in flight set unless retirement refuses work or a run is already registered
func (s *Store) tryRegisterRunningJob(jobID string, owner any, future *poolFuture) bool {
	k := s.inflightKey(jobID, rememberInflightHome(s.Home()))
	admitted, release := provider.Retirement.Work()
	defer release()
	inflight.Lock()
	defer inflight.Unlock()
	if !admitted || inflight.running[k] {
		return false
	}
	inflight.running[k] = true
	inflight.since[k] = inflightEpoch()
	inflight.futures[k] = future
	inflight.owners[k] = owner
	return true
}

// Removes a registration unless an explicit owner no longer holds it
func (s *Store) releaseRunningJob(jobID, h string, owner any) {
	k := s.inflightKey(jobID, h)
	inflight.Lock()
	defer inflight.Unlock()
	if owner != nil && inflight.owners[k] != owner {
		return
	}
	delete(inflight.owners, k)
	delete(inflight.running, k)
	delete(inflight.since, k)
	delete(inflight.allowance, k)
	delete(inflight.futures, k)
	delete(inflight.workerPIDs, k)
	delete(inflight.scopeIsolated, k)
}

// Stale allowance floor in minutes from cron.inflight_max_minutes, the env escape hatch, then thirty
func (s *Store) inflightMinAllowanceMinutes() float64 {
	if cfg, err := s.loadConfig(); err == nil {
		if cron, ok := cfg["cron"].(map[string]any); ok {
			if raw, present := cron["inflight_max_minutes"]; present && raw != nil {
				if v, err := pyint.Float(raw); err == nil && v > 0 {
					return v
				}
			}
		}
	}
	raw := pystr.Strip(s.cronEnvSetting(context.Background(), "ICEBATH_CRON_INFLIGHT_MAX_MINUTES", ""))
	if raw != "" {
		v, err := pyint.ParseFloat(raw)
		if err == nil {
			if v > 0 {
				return v
			}
		} else {
			s.log.Warn("Invalid ICEBATH_CRON_INFLIGHT_MAX_MINUTES=" + pyrepr.StrRepr(raw) + "; using default 30.0")
		}
	}
	return inflightMinAllowanceMinutes
}

var cronIntervalCache = struct {
	sync.Mutex
	m map[string]*float64
}{m: map[string]*float64{}}

// Minutes between the next two fires of a cron expression from now, false when it does not parse
func cronIntervalMinutes(expr string) (float64, bool) {
	cronIntervalCache.Lock()
	cached, hit := cronIntervalCache.m[expr]
	cronIntervalCache.Unlock()
	if hit {
		if cached == nil {
			return 0, false
		}
		return *cached, true
	}
	var result *float64
	if e, err := ParseCron(expr); err == nil {
		base := time.Now()
		if first, err := e.Next(base); err == nil {
			if second, err := e.Next(first); err == nil {
				if gap := second.Sub(first).Seconds() / 60.0; gap > 0 {
					result = &gap
				}
			}
		}
	}
	cronIntervalCache.Lock()
	cronIntervalCache.m[expr] = result
	cronIntervalCache.Unlock()
	if result == nil {
		return 0, false
	}
	return *result, true
}

// Interval of an interval or cron job in minutes, false for one shots, zero minutes and unparsable schedules
func (s *Store) jobIntervalMinutes(job *Job) (float64, bool) {
	if job == nil || job.Schedule == nil {
		return 0, false
	}
	schedule := job.Schedule
	if schedule.Kind == "" && pystr.Strip(schedule.Raw) != "" {
		parsed, err := s.parseSchedule(schedule.Raw)
		if err != nil || parsed == nil {
			return 0, false
		}
		schedule = parsed
	}
	switch schedule.Kind {
	case "interval":
		if !schedule.HasMinutes || schedule.Minutes == 0 {
			return 0, false
		}
		return float64(schedule.Minutes), true
	case "cron":
		return cronIntervalMinutes(schedule.Expr)
	}
	return 0, false
}

// Error a pool raises for a submit after shutdown
var errPoolShutdown = tools.ErrPoolShutdown

// Result slot of one pool task, done once the task returns
type poolFuture struct {
	done chan struct{}
}

// Whether the task finished
func (f *poolFuture) Done() bool {
	if f.done == nil {
		return false
	}
	select {
	case <-f.done:
		return true
	default:
		return false
	}
}

// Thread pool executor semantics, FIFO tasks on at most max workers, shutdown keeping queued work
type parallelPool struct {
	mu       sync.Mutex
	max      int
	workers  int
	idle     int
	queue    []func()
	shutdown bool
	wg       sync.WaitGroup
	wake     *sync.Cond
}

// Pool of max workers, the executor default of cpu count plus four capped at 32 when nil
func newParallelPool(maxWorkers *int) *parallelPool {
	n := min(32, runtime.NumCPU()+4)
	if maxWorkers != nil {
		n = *maxWorkers
	}
	p := &parallelPool{max: n}
	p.wake = sync.NewCond(&p.mu)
	return p
}

// Queues a task, starting a worker while none is idle and the cap allows
func (p *parallelPool) Submit(fn func()) (*poolFuture, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.shutdown {
		return nil, errPoolShutdown
	}
	f := &poolFuture{done: make(chan struct{})}
	p.wg.Add(1)
	p.queue = append(p.queue, func() {
		defer p.wg.Done()
		defer close(f.done)
		fn()
	})
	if p.idle > 0 {
		p.wake.Signal()
	} else if p.workers < p.max {
		p.workers++
		go p.work()
	}
	return f, nil
}

// Runs queued tasks until shutdown leaves the queue empty
func (p *parallelPool) work() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		for len(p.queue) == 0 {
			if p.shutdown {
				p.workers--
				return
			}
			p.idle++
			p.wake.Wait()
			p.idle--
		}
		task := p.queue[0]
		p.queue = p.queue[1:]
		p.mu.Unlock()
		func() {
			defer func() { recover() }()
			task()
		}()
		p.mu.Lock()
	}
}

// Refuses new tasks while queued ones still run, optionally waiting for all of them
func (p *parallelPool) Shutdown(wait bool) {
	p.mu.Lock()
	p.shutdown = true
	p.wake.Broadcast()
	p.mu.Unlock()
	if wait {
		p.wg.Wait()
	}
}

var parallelPools = struct {
	sync.Mutex
	pools map[string]*parallelPool
	max   map[string]*int
}{pools: map[string]*parallelPool{}, max: map[string]*int{}}

// Per home pool of the store, replaced when the worker cap changes
func (s *Store) getParallelPool(maxWorkers *int) *parallelPool {
	h := home.Key(rememberInflightHome(s.Home()))
	parallelPools.Lock()
	defer parallelPools.Unlock()
	pool := parallelPools.pools[h]
	prev, known := parallelPools.max[h]
	if pool == nil || !known || !sameWorkerCap(prev, maxWorkers) {
		if pool != nil {
			pool.Shutdown(false)
		}
		pool = newParallelPool(maxWorkers)
		parallelPools.pools[h] = pool
		parallelPools.max[h] = maxWorkers
	}
	return pool
}

// Optional worker caps compared by value
func sameWorkerCap(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Drops the pools of homes no longer ticked, their queued work still finishing
func discardParallelPools(keys []string) {
	parallelPools.Lock()
	defer parallelPools.Unlock()
	for _, k := range keys {
		pool := parallelPools.pools[k]
		delete(parallelPools.pools, k)
		delete(parallelPools.max, k)
		if pool != nil {
			pool.Shutdown(false)
		}
	}
}

// Waits for every pool at process exit and forgets them
func shutdownParallelPools() {
	parallelPools.Lock()
	pools := make([]*parallelPool, 0, len(parallelPools.pools))
	for _, p := range parallelPools.pools {
		pools = append(pools, p)
	}
	parallelPools.Unlock()
	for _, p := range pools {
		p.Shutdown(true)
	}
	parallelPools.Lock()
	parallelPools.pools = map[string]*parallelPool{}
	parallelPools.max = map[string]*int{}
	parallelPools.Unlock()
}

// ShutdownParallelPools is the atexit teardown of every parallel pool for the binary's exit path
func ShutdownParallelPools() { shutdownParallelPools() }
