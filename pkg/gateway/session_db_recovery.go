package gateway

import (
	"context"
	"math"
	"runtime"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	handleCacheInitialRetryDelaySeconds = 1.0
	handleCacheMaxRetryDelaySeconds     = 60.0
)

type handleUnavailable struct {
	failures    int
	nextRetryAt float64
	inFlight    bool
	err         string
}

type handleHealthSource struct{}

var (
	handleHealthMu     sync.Mutex
	handleHealthStates = map[*handleHealthSource]map[string]string{}
)

// Publishes runtime status through a package var so tests capture the health writes
var handleCachePublishRuntimeStatus = PublishRuntimeStatus

// Publishes one aggregate store health across every live cache with no path or error text
func publishHandleHealth(ctx context.Context, source *handleHealthSource, path, stateName string) {
	handleHealthMu.Lock()
	states := handleHealthStates[source]
	if states == nil {
		states = map[string]string{}
		handleHealthStates[source] = states
	}
	states[path] = stateName
	all := map[string]bool{}
	for _, item := range handleHealthStates {
		for _, value := range item {
			all[value] = true
		}
	}
	aggregate := "ok"
	for _, candidate := range []string{"retrying", "unavailable"} {
		if all[candidate] {
			aggregate = candidate
			break
		}
	}
	handleHealthMu.Unlock()
	func() {
		defer func() { _ = recover() }()
		_, _ = handleCachePublishRuntimeStatus(ctx, RuntimeStatusUpdate{Fields: map[string]any{"session_store": map[string]any{"status": aggregate}}})
	}()
}

// True while the process wide registry owns the handle, an async wrapper judged by the SessionDB it wraps
func registryOwned(handle any) bool {
	if async, ok := handle.(*state.AsyncSessionDB); ok {
		if async == nil {
			return false
		}
		handle = async.DB()
	}
	db, ok := handle.(*state.SessionDB)
	return ok && db != nil && db.SharedRegistryOwned()
}

// Caches handles by path while failed opens back off and heal inside the process
type RecoverableHandleCache struct {
	Handles           map[string]any
	Lock              *sync.Mutex
	clock             func() float64
	initialRetryDelay float64
	maxRetryDelay     float64
	unavailable       map[string]*handleUnavailable
	registryBacked    map[string]bool
	healthSource      *handleHealthSource
	generation        int
	closeRejected     func(handle any)
}

// Constructor keywords of the cache, nil fields taking the upstream defaults
type RecoverableHandleCacheOptions struct {
	Handles           map[string]any
	Lock              *sync.Mutex
	Clock             func() float64
	InitialRetryDelay *float64
	MaxRetryDelay     *float64
}

// Keyword options of one get call
type HandleCacheGetOptions struct {
	RaiseOnError bool
	OnRecovered  func()
	NonCacheable func(error) bool
}

var handleCacheEpoch = time.Now()

// Seconds on the monotonic clock
func handleCacheMonotonic() float64 { return time.Since(handleCacheEpoch).Seconds() }

// Cache with the default clock, retry delays, handle map and lock
func NewRecoverableHandleCache() *RecoverableHandleCache {
	return NewRecoverableHandleCacheWith(RecoverableHandleCacheOptions{})
}

// Cache over the given handle map, lock, clock and retry delays
func NewRecoverableHandleCacheWith(opts RecoverableHandleCacheOptions) *RecoverableHandleCache {
	initial, maximum := handleCacheInitialRetryDelaySeconds, handleCacheMaxRetryDelaySeconds
	if opts.InitialRetryDelay != nil {
		initial = *opts.InitialRetryDelay
	}
	if opts.MaxRetryDelay != nil {
		maximum = *opts.MaxRetryDelay
	}
	c := &RecoverableHandleCache{Handles: opts.Handles, Lock: opts.Lock, clock: opts.Clock,
		unavailable: map[string]*handleUnavailable{}, registryBacked: map[string]bool{},
		healthSource: &handleHealthSource{}}
	if c.Handles == nil {
		c.Handles = map[string]any{}
	}
	if c.Lock == nil {
		c.Lock = &sync.Mutex{}
	}
	if c.clock == nil {
		c.clock = handleCacheMonotonic
	}
	c.initialRetryDelay = math.Max(0, initial)
	c.maxRetryDelay = math.Max(c.initialRetryDelay, maximum)
	runtime.AddCleanup(c, func(source *handleHealthSource) {
		handleHealthMu.Lock()
		delete(handleHealthStates, source)
		handleHealthMu.Unlock()
	}, c.healthSource)
	return c
}

// True with the lock held when close all ran or the slot was replaced during the open
func (c *RecoverableHandleCache) isStale(path string, unavailable *handleUnavailable, generation int) bool {
	return generation != c.generation || c.unavailable[path] != unavailable
}

// Cached handle or one single flight open, nil without error while a retry is in flight or backing off
func (c *RecoverableHandleCache) Get(ctx context.Context, path string, opener func() (any, error), nonCacheable func(error) bool) (any, error) {
	return c.GetWith(ctx, path, opener, HandleCacheGetOptions{NonCacheable: nonCacheable})
}

// Cached handle or one bounded open, raising non cacheable errors and every error under RaiseOnError
func (c *RecoverableHandleCache) GetWith(ctx context.Context, path string, opener func() (any, error), opts HandleCacheGetOptions) (any, error) {
	path = pypath.PathStr(path)
	c.Lock.Lock()
	if handle, ok := c.Handles[path]; ok {
		if !c.registryBacked[path] || registryOwned(handle) {
			c.Lock.Unlock()
			return handle, nil
		}
		delete(c.Handles, path)
		delete(c.registryBacked, path)
	}
	unavailable := c.unavailable[path]
	if unavailable == nil {
		unavailable = &handleUnavailable{}
		c.unavailable[path] = unavailable
	}
	if unavailable.inFlight || c.clock() < unavailable.nextRetryAt {
		c.Lock.Unlock()
		return nil, nil
	}
	unavailable.inFlight = true
	wasUnavailable := unavailable.failures > 0
	generation := c.generation
	c.Lock.Unlock()
	if wasUnavailable {
		publishHandleHealth(ctx, c.healthSource, path, "retrying")
	}
	handle, err := opener()
	if err != nil {
		uncacheable := opts.NonCacheable != nil && opts.NonCacheable(err)
		c.Lock.Lock()
		stale := c.isStale(path, unavailable, generation)
		if uncacheable {
			if !stale {
				delete(c.unavailable, path)
			}
			c.Lock.Unlock()
			return nil, err
		}
		if !stale {
			unavailable.failures++
			unavailable.err = pyrepr.ExceptionName(err) + ": " + pyrepr.ExceptionMessage(err)
			backoff := c.initialRetryDelay * math.Pow(2, float64(min(unavailable.failures-1, 30)))
			unavailable.nextRetryAt = c.clock() + math.Min(backoff, c.maxRetryDelay)
			unavailable.inFlight = false
		}
		c.Lock.Unlock()
		if !stale {
			publishHandleHealth(ctx, c.healthSource, path, "unavailable")
		}
		if opts.RaiseOnError {
			return nil, err
		}
		return nil, nil
	}
	c.Lock.Lock()
	stale := c.isStale(path, unavailable, generation)
	var closeRejected func(any)
	if !stale {
		c.Handles[path] = handle
		if registryOwned(handle) {
			c.registryBacked[path] = true
		} else {
			delete(c.registryBacked, path)
		}
		delete(c.unavailable, path)
	} else {
		closeRejected = c.closeRejected
	}
	c.Lock.Unlock()
	if stale {
		if closeRejected != nil {
			func() {
				defer func() { _ = recover() }()
				closeRejected(handle)
			}()
		}
		return nil, nil
	}
	publishHandleHealth(ctx, c.healthSource, path, "ok")
	if wasUnavailable && opts.OnRecovered != nil {
		opts.OnRecovered()
	}
	return handle, nil
}

// Error text the latest failed open of the path raised, empty when none is recorded
func (c *RecoverableHandleCache) LastError(path string) string {
	target, _ := pypath.Resolve(path)
	c.Lock.Lock()
	defer c.Lock.Unlock()
	for key, u := range c.unavailable {
		if resolved, _ := pypath.Resolve(key); resolved == target {
			return u.err
		}
	}
	return ""
}

// Drains every cached handle under the lock and closes each outside it
func (c *RecoverableHandleCache) CloseAll(closeHandle func(handle any)) {
	c.Lock.Lock()
	c.generation++
	c.closeRejected = closeHandle
	handles := make([]any, 0, len(c.Handles))
	paths := map[string]bool{}
	for path, handle := range c.Handles {
		handles = append(handles, handle)
		paths[path] = true
	}
	for path := range c.unavailable {
		paths[path] = true
	}
	clear(c.Handles)
	clear(c.registryBacked)
	clear(c.unavailable)
	c.Lock.Unlock()
	for _, handle := range handles {
		func() {
			defer func() { _ = recover() }()
			closeHandle(handle)
		}()
	}
	handleHealthMu.Lock()
	if states := handleHealthStates[c.healthSource]; states != nil {
		for path := range paths {
			delete(states, path)
		}
	}
	handleHealthMu.Unlock()
}
