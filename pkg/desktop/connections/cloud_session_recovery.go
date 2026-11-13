package connections

import (
	"sync"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

// Milliseconds a failed Cloud cookie recovery of a base URL suppresses the next attempt
const cloudRecoveryRetryAfterMs = 60_000

// CloudRecoveryDeps are the native session check, the cookie restore and the recovery notice of a Cloud recovery
type CloudRecoveryDeps struct {
	HasNativeSession     func(baseURL string) bool
	RestoreCookieSession func(baseURL string) (bool, error)
	OnRecovered          func(baseURL string)
	Now                  func() int64
}

// CloudSessionRecovery restores a Cloud agent cookie once per base URL after a cookie ticket 401
type CloudSessionRecovery struct {
	deps     CloudRecoveryDeps
	mu       sync.Mutex
	inflight map[string]*backend.Future[bool]
	failedAt map[string]int64
}

// Recovery over its session checks, cookie restore and clock
func NewCloudSessionRecovery(deps CloudRecoveryDeps) *CloudSessionRecovery {
	return &CloudSessionRecovery{deps: deps, inflight: map[string]*backend.Future[bool]{}, failedAt: map[string]int64{}}
}

func (r *CloudSessionRecovery) markFailed(baseURL string) {
	r.mu.Lock()
	r.failedAt[baseURL] = r.deps.Now()
	r.mu.Unlock()
}

// Shared cookie restore of a base URL, nil while a recent restore of it failed
func (r *CloudSessionRecovery) recovery(baseURL string) *backend.Future[bool] {
	r.mu.Lock()
	defer r.mu.Unlock()
	if pending := r.inflight[baseURL]; pending != nil {
		return pending
	}
	if failed, ok := r.failedAt[baseURL]; ok && r.deps.Now()-failed < cloudRecoveryRetryAfterMs {
		return nil
	}
	pending := backend.NewFuture(func() (bool, error) {
		restored, err := r.deps.RestoreCookieSession(baseURL)
		if err != nil {
			restored = false
		}
		if !restored {
			r.markFailed(baseURL)
		}
		r.mu.Lock()
		delete(r.inflight, baseURL)
		r.mu.Unlock()
		return restored, nil
	})
	r.inflight[baseURL] = pending
	return pending
}

// Mints through mint, restoring a Cloud agent cookie session and minting once more after a cookie only 401
func RecoverCloudSession[T any](r *CloudSessionRecovery, baseURL string, mint func() (T, error)) (T, error) {
	var zero T
	hadNativeSession := r.deps.HasNativeSession(baseURL)
	value, err := mint()
	if err == nil {
		return value, nil
	}
	status, hasStatus := readStatusCode(err)
	if !backend.IsNousCloudAgentURL(baseURL) || hadNativeSession || r.deps.HasNativeSession(baseURL) || !hasStatus || status != 401 {
		return zero, err
	}
	pending := r.recovery(baseURL)
	if pending == nil {
		return zero, err
	}
	if restored, _ := pending.Wait(); !restored {
		return zero, err
	}
	if r.deps.HasNativeSession(baseURL) {
		return zero, err
	}
	ticket, retryErr := mint()
	if retryErr != nil {
		r.markFailed(baseURL)
		return zero, retryErr
	}
	r.mu.Lock()
	delete(r.failedAt, baseURL)
	r.mu.Unlock()
	r.deps.OnRecovered(baseURL)
	return ticket, nil
}
