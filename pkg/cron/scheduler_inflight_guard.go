package cron

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const forcedReleaseHistory = 20

var executionTerminalStates = map[string]bool{"completed": true, "failed": true, "unknown": true, "cancelled": true, "missed": true}

// Forced release counters, waiter keys and shutdown interruption tokens shared under the registry lock
var inflightGuard = struct {
	forcedReleaseCount int
	forcedReleases     []*pyjson.Object
	restartSafeWaiters map[inflightKey]bool
	interrupted        map[any]bool
}{restartSafeWaiters: map[inflightKey]bool{}, interrupted: map[any]bool{}}

// InflightGuardStats is the in flight guard snapshot for health reporting
type InflightGuardStats struct {
	Running              []string
	RunningAgesSeconds   map[string]float64
	ForcedReleases       int
	RecentForcedReleases []*pyjson.Object
}

// Running job ids, their ages, the forced release count and the recent forced releases
func getInflightGuardStats() InflightGuardStats {
	now := inflightEpoch()
	inflight.Lock()
	defer inflight.Unlock()
	out := InflightGuardStats{Running: []string{}, RunningAgesSeconds: map[string]float64{}, ForcedReleases: inflightGuard.forcedReleaseCount}
	for k := range inflight.running {
		out.Running = append(out.Running, k.JobID)
	}
	sort.Strings(out.Running)
	for k, started := range inflight.since {
		out.RunningAgesSeconds[k.JobID] = pyint.Round(now-started, 1)
	}
	out.RecentForcedReleases = append([]*pyjson.Object(nil), inflightGuard.forcedReleases...)
	return out
}

// Keeps a bounded history of one forced release and appends it to the profile's jsonl record
func (s *Store) recordForcedRelease(jobID, name string, ageSeconds, allowanceSeconds float64) {
	entry := pyjson.NewObject()
	entry.Set("job_id", jobID)
	entry.Set("name", name)
	entry.Set("age_seconds", pyint.Round(ageSeconds, 1))
	entry.Set("allowance_seconds", pyint.Round(allowanceSeconds, 1))
	entry.Set("at", s.clk.nowISO())
	inflight.Lock()
	inflightGuard.forcedReleases = append(inflightGuard.forcedReleases, entry)
	if n := len(inflightGuard.forcedReleases); n > forcedReleaseHistory {
		inflightGuard.forcedReleases = append([]*pyjson.Object(nil), inflightGuard.forcedReleases[n-forcedReleaseHistory:]...)
	}
	inflight.Unlock()
	err := func() error {
		path := filepath.Join(s.home, "cron", "inflight_forced_releases.jsonl")
		if err := ensureCronDir(filepath.Dir(path)); err != nil {
			return err
		}
		line, err := pyjson.Dumps(entry)
		if err != nil {
			return err
		}
		fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
		if err != nil {
			return err
		}
		_, werr := fh.WriteString(line + "\n")
		if cerr := fh.Close(); werr == nil {
			werr = cerr
		}
		return werr
	}()
	if err != nil {
		s.log.Debug(fmt.Sprintf("Could not append forced-release record: %v", err))
	}
}

// Latest durable execution per releasable looking claim of this home, one ledger query
func (s *Store) latestExecutionsForReleasableClaims() map[string]map[string]any {
	local := home.Key(s.home)
	inflight.Lock()
	var candidates []string
	for k := range inflight.running {
		if k.Home != local {
			continue
		}
		fut := inflight.futures[k]
		if fut == nil || fut == futurePending || fut.Done() {
			candidates = append(candidates, k.JobID)
		}
	}
	inflight.Unlock()
	if len(candidates) == 0 {
		return map[string]map[string]any{}
	}
	sort.Strings(candidates)
	rows, err := func() (rows map[string]map[string]any, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		return latestExecutions(s, candidates)
	}()
	if err != nil || rows == nil {
		return map[string]map[string]any{}
	}
	return rows
}

// True when the ledger row was claimed at or after the registry claim started
func (s *Store) rowBelongsToClaim(row map[string]any, claimStarted float64) bool {
	claimedAt, _ := row["claimed_at"].(string)
	if claimedAt == "" {
		return false
	}
	parsed, err := ParseISO(claimedAt)
	if err != nil {
		return false
	}
	ts := s.clk.ensureAware(parsed, nil)
	return float64(ts.UnixNano())/1e9 >= claimStarted
}

// Logs and records one forced release, failing the run unless the ledger already settled it
func (s *Store) recordStaleRelease(job *Job, jobID string, age, allowance float64, fut *poolFuture, reason string) {
	name := jobID
	if job != nil && job.Name != "" {
		name = job.Name
	}
	futureState := "finished"
	switch {
	case fut == futurePending:
		futureState = "pending"
	case fut == nil:
		futureState = "missing"
	}
	s.log.Warn(fmt.Sprintf("cron.inflight.forced_release event=forced_release reason=%s job='%s' "+
		"id=%s age=%.0fs allowance=%.0fs future=%s — stale in-flight claim "+
		"released; the job was skipping every fire with 'already running'", reason, name, jobID, age, allowance, futureState))
	s.recordForcedRelease(jobID, name, age, allowance)
	if reason == "ledger-terminal" {
		return
	}
	if job != nil && job.Repeat != nil && job.Repeat.Times != nil {
		s.log.Warn(fmt.Sprintf("cron.inflight.forced_release.job_untouched job='%s' id=%s — "+
			"finite-repeat job released without mark_job_run (repeat budget "+
			"preserved); row left in place so it re-fires normally", name, jobID))
		return
	}
	msg := fmt.Sprintf("Stale in-flight claim force-released after %.1fm (allowance %.1fm); previous run never released "+
		"the scheduler in-flight guard", age/60, allowance/60)
	if _, err := s.markJobRun(context.Background(), jobID, false, MarkOptions{Error: &msg}); err != nil {
		s.log.Warn(fmt.Sprintf("Could not record forced release for job %s: %v", jobID, err))
	}
}

// Releases in flight claims of this home whose run settled in the ledger or outlived the allowance
func (s *Store) sweepStaleInflight(due []*Job) []string {
	byID := map[string]*Job{}
	for _, j := range due {
		if j != nil {
			byID[j.ID] = j
		}
	}
	floor := s.inflightMinAllowanceMinutes() * 60.0
	now := inflightEpoch()
	latest := s.latestExecutionsForReleasableClaims()
	local := home.Key(s.home)
	intervals := map[string]float64{}
	for id, j := range byID {
		if m, ok := s.jobIntervalMinutes(j); ok {
			intervals[id] = m
		}
	}
	type staleClaim struct {
		jobID          string
		age, allowance float64
		fut            *poolFuture
		reason         string
	}
	var stale []staleClaim
	inflight.Lock()
	var keys []inflightKey
	for k := range inflight.running {
		if k.Home == local {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].JobID < keys[j].JobID })
	for _, k := range keys {
		started, ok := inflight.since[k]
		if !ok {
			inflight.since[k] = now
			continue
		}
		age := now - started
		allowance := floor
		if m := intervals[k.JobID]; m != 0 {
			allowance = max(allowance, 2.0*m*60.0)
		}
		fut := inflight.futures[k]
		if fut != futurePending && fut != nil && !fut.Done() {
			continue
		}
		var reason string
		row := latest[k.JobID]
		status, _ := row["status"].(string)
		switch {
		case row != nil && executionTerminalStates[status] && s.rowBelongsToClaim(row, started):
			reason = "ledger-terminal"
		case age >= allowance:
			reason = "age"
		default:
			continue
		}
		delete(inflight.owners, k)
		delete(inflight.running, k)
		delete(inflight.since, k)
		delete(inflight.allowance, k)
		delete(inflight.futures, k)
		inflightGuard.forcedReleaseCount++
		stale = append(stale, staleClaim{k.JobID, age, allowance, fut, reason})
	}
	inflight.Unlock()
	out := make([]string, 0, len(stale))
	for _, c := range stale {
		s.recordStaleRelease(byID[c.jobID], c.jobID, c.age, c.allowance, c.fut, c.reason)
		out = append(out, c.jobID)
	}
	return out
}

// OwnerPair names one execution by job id and fire owner for a restricted shutdown mark
type OwnerPair struct {
	JobID string
	Owner string
}

// Marks every in flight run interrupted before persisting the failure, returning the job ids marked
func markRunningJobsInterrupted(ctx context.Context, reason string, onlyOwners map[OwnerPair]bool) []string {
	type fire struct {
		token any
		key   inflightKey
		owner *string
		home  string
	}
	inflight.Lock()
	var fires []fire
	for key, executions := range inflight.fireOwners {
		if inflightGuard.restartSafeWaiters[key] {
			continue
		}
		for token, entry := range executions {
			if onlyOwners != nil {
				if entry.Owner == nil || !onlyOwners[OwnerPair{key.JobID, *entry.Owner}] {
					continue
				}
			}
			fires = append(fires, fire{token, key, entry.Owner, entry.Home})
		}
	}
	if onlyOwners == nil {
		registered := map[inflightKey]bool{}
		for _, f := range fires {
			registered[f.key] = true
		}
		for key := range inflight.running {
			if !registered[key] && !inflightGuard.restartSafeWaiters[key] {
				fires = append(fires, fire{nil, key, nil, inflightHomePath(key.Home)})
			}
		}
	}
	for _, f := range fires {
		if f.token != nil {
			inflightGuard.interrupted[f.token] = true
		} else {
			inflightGuard.interrupted[f.key] = true
		}
	}
	inflight.Unlock()
	sort.SliceStable(fires, func(i, j int) bool { return fires[i].key.JobID < fires[j].key.JobID })
	marked := []string{}
	for _, f := range fires {
		if f.owner == nil || *f.owner == "" {
			slog.Default().Warn(fmt.Sprintf("Job '%s' interrupted before its durable fire owner was registered; "+
				"leaving persisted state untouched", f.key.JobID))
			marked = append(marked, f.key.JobID)
			continue
		}
		s := openCronStore(f.home, nil)
		msg := reason
		ok, err := s.markJobRun(ctx, f.key.JobID, false, MarkOptions{Error: &msg, ExpectedFireOwner: f.owner})
		if err != nil {
			s.log.Warn(fmt.Sprintf("Failed to mark job %s interrupted: %v", f.key.JobID, err))
			continue
		}
		if ok {
			marked = append(marked, f.key.JobID)
		}
	}
	return marked
}

// True when shutdown marked this execution or the job key interrupted, leaving the flag set
func (s *Store) isInterrupted(jobID string, token any) bool {
	key := s.inflightKey(jobID, s.home)
	inflight.Lock()
	defer inflight.Unlock()
	if token != nil && inflightGuard.interrupted[token] {
		return true
	}
	return inflightGuard.interrupted[key]
}

// True and cleared when shutdown marked this execution or the job key interrupted
func (s *Store) consumeInterruptedFlag(jobID string, token any) bool {
	key := s.inflightKey(jobID, s.home)
	inflight.Lock()
	defer inflight.Unlock()
	hit := false
	if token != nil && inflightGuard.interrupted[token] {
		delete(inflightGuard.interrupted, token)
		hit = true
	}
	if inflightGuard.interrupted[key] {
		delete(inflightGuard.interrupted, key)
		hit = true
	}
	return hit
}
