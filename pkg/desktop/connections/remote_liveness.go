package connections

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

// Probe budgets, failure limits and windows of the remote liveness policy
const (
	RemoteLivenessTimeout               = 10 * time.Second
	PooledRemoteDispatchProbeTimeout    = RemoteLivenessTimeout
	RemoteLivenessFailureLimit          = 3
	RemoteLivenessFailureWindowMs       = 60_000
	RemotePooledLivenessFailureWindowMs = 15 * 60_000
	PowerResumeRevalidationHoldoffMs    = 15_000
)

// BackendProbe reads path from a backend descriptor within timeout
type BackendProbe func(conn backend.Connection, path string, timeout time.Duration) error

// RemoteLivenessFailure is the streak a failed probe reached and whether it consumed the streak
type RemoteLivenessFailure struct {
	Failures    int
	ShouldReset bool
}

type livenessStreak struct {
	failures      int
	lastFailureAt int64
}

// RemoteLivenessTracker counts consecutive probe failures per gateway base URL
type RemoteLivenessTracker struct {
	failureLimit    int
	failureWindowMs int64
	now             func() int64
	mu              sync.Mutex
	byBaseURL       map[string]livenessStreak
}

// Tracker with a failure limit, a window in milliseconds and a millisecond clock
func NewRemoteLivenessTracker(failureLimit int, failureWindowMs int64, now func() int64) (*RemoteLivenessTracker, error) {
	if failureLimit < 1 {
		return nil, errors.New("Remote liveness failure limit must be a positive integer.")
	}
	if failureWindowMs < 1 {
		return nil, errors.New("Remote liveness failure window must be positive.")
	}
	return &RemoteLivenessTracker{failureLimit: failureLimit, failureWindowMs: failureWindowMs, now: now, byBaseURL: map[string]livenessStreak{}}, nil
}

func nowMillis() int64 { return time.Now().UnixMilli() }

// Clears the streak of a base URL after a live probe
func (t *RemoteLivenessTracker) RecordSuccess(baseURL string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.byBaseURL, baseURL)
}

// Counts a failure inside the window, consuming the streak once it reaches the limit
func (t *RemoteLivenessTracker) RecordFailure(baseURL string) RemoteLivenessFailure {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	failures := 1
	if previous, ok := t.byBaseURL[baseURL]; ok && now-previous.lastFailureAt <= t.failureWindowMs {
		failures = previous.failures + 1
	}
	shouldReset := failures >= t.failureLimit
	if shouldReset {
		delete(t.byBaseURL, baseURL)
	} else {
		t.byBaseURL[baseURL] = livenessStreak{failures: failures, lastFailureAt: now}
	}
	return RemoteLivenessFailure{Failures: failures, ShouldReset: shouldReset}
}

// Forgets every streak
func (t *RemoteLivenessTracker) Clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.byBaseURL = map[string]livenessStreak{}
}

func trimTrailingSlashes(s string) string { return strings.TrimRight(s, "/") }

// DispatchHealthOptions name the cached descriptor a dispatch probes and how it retires and reconnects
type DispatchHealthOptions struct {
	Connection *backend.Future[backend.Connection]
	Current    func() *backend.Future[backend.Connection]
	Probe      BackendProbe
	Reconnect  func() (backend.Connection, error)
	Retire     func(err error) error
}

// Returns the cached descriptor once its health probe answers, else retires it if still current and reconnects
func EnsureHealthyPooledRemoteBackendForDispatch(o DispatchHealthOptions) (backend.Connection, error) {
	connection, err := o.Connection.Wait()
	if err == nil {
		if o.Current() != o.Connection {
			return o.Reconnect()
		}
		if err = o.Probe(connection, "/api/health", PooledRemoteDispatchProbeTimeout); err != nil && backend.IsMissingHealthEndpointError(err) {
			err = o.Probe(connection, "/api/status", PooledRemoteDispatchProbeTimeout)
		}
	}
	if err != nil {
		if o.Current() == o.Connection {
			if retireErr := o.Retire(err); retireErr != nil {
				return nil, retireErr
			}
		}
		return o.Reconnect()
	}
	if o.Current() != o.Connection {
		return o.Reconnect()
	}
	return connection, nil
}

// PooledRevalidationOptions name the pooled descriptors a sweep probes and how dead ones are stopped
type PooledRevalidationOptions struct {
	Entries     []backend.PoolBackend
	Log         func(string)
	Probe       BackendProbe
	StopBackend func(key string) error
	Tracker     *RemoteLivenessTracker
}

func remotePoolEntries(entries []backend.PoolBackend) []backend.PoolBackend {
	out := make([]backend.PoolBackend, 0, len(entries))
	for _, entry := range entries {
		if entry.RemoteBaseURL != "" {
			out = append(out, entry)
		}
	}
	return out
}

func probePoolEntry(entry backend.PoolBackend, probe BackendProbe) error {
	if entry.Connection == nil {
		return errors.New("Remote backend descriptor is unavailable.")
	}
	connection, err := entry.Connection.Wait()
	if err != nil {
		return err
	}
	return probe(connection, "/api/status", RemoteLivenessTimeout)
}

// Probes every pooled remote descriptor in parallel and stops the ones whose failure streak reached the limit
func RevalidatePooledRemoteBackends(o PooledRevalidationOptions) ([]string, error) {
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		dropped  = []string{}
		stopErrs []error
	)
	for _, entry := range remotePoolEntries(o.Entries) {
		wg.Add(1)
		go func(entry backend.PoolBackend) {
			defer wg.Done()
			baseURL := trimTrailingSlashes(entry.RemoteBaseURL)
			if probePoolEntry(entry, o.Probe) == nil {
				o.Tracker.RecordSuccess(baseURL)
				return
			}
			failure := o.Tracker.RecordFailure(baseURL)
			if !failure.ShouldReset {
				o.Log(fmt.Sprintf("Pooled remote backend for profile \"%s\" failed liveness probe (%d/%d); keeping descriptor for retry.", entry.Key, failure.Failures, RemoteLivenessFailureLimit))
				return
			}
			o.Log(fmt.Sprintf("Pooled remote backend for profile \"%s\" failed liveness probe; dropping stale descriptor.", entry.Key))
			stopErr := o.StopBackend(entry.Key)
			mu.Lock()
			dropped = append(dropped, entry.Key)
			if stopErr != nil {
				stopErrs = append(stopErrs, stopErr)
			}
			mu.Unlock()
		}(entry)
	}
	wg.Wait()
	return dropped, errors.Join(stopErrs...)
}

// SuspectPoolOptions name the pooled descriptors a post resume sweep probes, retires and rebuilds
type SuspectPoolOptions struct {
	Entries []backend.PoolBackend
	Log     func(string)
	Probe   BackendProbe
	Rebuild func(poolKey string) error
	Retire  func(poolKey string) error
	Tracker *RemoteLivenessTracker
}

// SuspectPoolResult lists the pool keys a post resume sweep retired and rebuilt
type SuspectPoolResult struct {
	Rebuilt []string
	Retired []string
}

// Retires each pooled remote that fails one probe after resume and rebuilds it, never failing the sweep
func RevalidateSuspectPooledRemoteBackends(o SuspectPoolOptions) SuspectPoolResult {
	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		result = SuspectPoolResult{Rebuilt: []string{}, Retired: []string{}}
	)
	for _, entry := range remotePoolEntries(o.Entries) {
		wg.Add(1)
		go func(entry backend.PoolBackend) {
			defer wg.Done()
			baseURL := trimTrailingSlashes(entry.RemoteBaseURL)
			probeErr := probePoolEntry(entry, o.Probe)
			if probeErr == nil {
				o.Tracker.RecordSuccess(baseURL)
				return
			}
			o.Log(fmt.Sprintf("Pooled remote backend \"%s\" failed its post-resume probe (%s); rebuilding tunnel.", entry.Key, probeErr.Error()))
			if retireErr := o.Retire(entry.Key); retireErr != nil {
				o.Log(fmt.Sprintf("Pooled remote backend \"%s\" could not be retired after resume (%s); leaving descriptor for dispatch-time recovery.", entry.Key, retireErr.Error()))
				return
			}
			mu.Lock()
			result.Retired = append(result.Retired, entry.Key)
			mu.Unlock()
			o.Tracker.RecordSuccess(baseURL)
			if rebuildErr := o.Rebuild(entry.Key); rebuildErr != nil {
				o.Log(fmt.Sprintf("Pooled remote backend \"%s\" could not be rebuilt after resume (%s); renderer reconnect will retry.", entry.Key, rebuildErr.Error()))
				return
			}
			mu.Lock()
			result.Rebuilt = append(result.Rebuilt, entry.Key)
			mu.Unlock()
		}(entry)
	}
	wg.Wait()
	return result
}

// Trigger for resume and unlock signals running one revalidation per holdoff window and logging its failure
func NewPowerResumeRemoteRevalidation(log func(string), now func() int64, revalidate func() error) func() {
	var (
		mu         sync.Mutex
		lastKickAt int64
		kicked     bool
	)
	return func() {
		at := now()
		mu.Lock()
		if kicked && at-lastKickAt < PowerResumeRevalidationHoldoffMs {
			mu.Unlock()
			return
		}
		lastKickAt, kicked = at, true
		mu.Unlock()
		if err := revalidate(); err != nil {
			log(fmt.Sprintf("Post-resume remote revalidation failed (%s); will retry on the next wake or renderer reconnect.", err.Error()))
		}
	}
}

// RemoteRevalidationOptions name the cached primary a revalidation probes and how a dead one is reset
type RemoteRevalidationOptions struct {
	Connection *backend.Future[backend.Connection]
	Current    func() *backend.Future[backend.Connection]
	Log        func(string)
	Probe      BackendProbe
	Reset      func()
	Tracker    *RemoteLivenessTracker
}

// Probes the cached remote primary and resets it once its failure streak reaches the limit
func RevalidateRemoteConnection(o RemoteRevalidationOptions) backend.RevalidateResult {
	kept := backend.RevalidateResult{OK: true}
	connection, err := o.Connection.Wait()
	if err != nil || o.Current() != o.Connection {
		return kept
	}
	mode, _ := connection["mode"].(string)
	rawBaseURL, _ := connection["baseUrl"].(string)
	if mode != "remote" || rawBaseURL == "" {
		return kept
	}
	baseURL := trimTrailingSlashes(rawBaseURL)
	probeErr := o.Probe(connection, "/api/status", RemoteLivenessTimeout)
	if o.Current() != o.Connection {
		return kept
	}
	if probeErr == nil {
		o.Tracker.RecordSuccess(baseURL)
		return kept
	}
	failure := o.Tracker.RecordFailure(baseURL)
	if !failure.ShouldReset {
		o.Log(fmt.Sprintf("Cached remote Icebath backend failed liveness probe (%d/%d); keeping connection for retry.", failure.Failures, RemoteLivenessFailureLimit))
		return kept
	}
	o.Log("Cached remote Icebath backend failed liveness probe; dropping stale connection.")
	o.Reset()
	return backend.RevalidateResult{OK: true, Rebuilt: true}
}
