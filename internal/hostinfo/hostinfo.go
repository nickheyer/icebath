// Facts about the machine running icebath, each probed once where it cannot change and kept on a Host
package hostinfo

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Reports a fact this host does not have, as opposed to a probe that failed
var ErrAbsent = errors.New("hostinfo: absent on this host")

// Facts of this machine that cannot change while the process runs, kept from their first probe, one per process
type Host struct {
	nativeArch memo[Arch]
	os         memo[OS]
	distro     memo[Distribution]
	cpuModel   memo[string]
	cpuVendor  memo[string]
	ramTotal   memo[uint64]
	gpuClass   memo[GPUClass]
	nvidia     memo[[]nvidiaGPU]
	wsl        memo[bool]
	container  memo[Container]
	summary    memo[Summary]
	smiPath    memo[string]
	libc       ctxMemo[LibC]
	platform   ctxMemo[string]
	smi        ttlMemo[SMIReading]
	battery    ttlMemo[Battery]
	cuda       settleMemo[CUDADevice]
	load       loadState
	native     nativeCache
}

// Returns a Host that has probed nothing yet
func New() *Host { return &Host{} }

// Value probed on first use and kept for the life of its Host
type memo[T any] struct {
	once sync.Once
	val  T
	err  error
}

// Result of probe, which runs at most once
func (m *memo[T]) get(probe func() (T, error)) (T, error) {
	m.once.Do(func() { m.val, m.err = probe() })
	return m.val, m.err
}

// Result of a probe that cannot fail, which runs at most once
func (m *memo[T]) value(probe func() T) T {
	m.once.Do(func() { m.val = probe() })
	return m.val
}

// Value probed under a context, kept once a probe finishes without being cancelled
type ctxMemo[T any] struct {
	mu   sync.Mutex
	done bool
	val  T
	err  error
}

// Kept result, else the result of probe, kept unless ctx ended it
func (m *ctxMemo[T]) get(ctx context.Context, probe func(context.Context) (T, error)) (T, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.done {
		return m.val, m.err
	}
	val, err := probe(ctx)
	if err != nil && ctx.Err() != nil {
		return val, err
	}
	m.val, m.err, m.done = val, err, true
	return val, err
}

// Value probed again once it is older than its time to live, failures kept like answers
type ttlMemo[T any] struct {
	mu  sync.Mutex
	set bool
	at  time.Time
	val T
	err error
}

// Kept result while younger than ttl unless refresh is set, else a new probe kept unless ctx ended it
func (m *ttlMemo[T]) get(ctx context.Context, ttl time.Duration, refresh bool, probe func(context.Context) (T, error)) (T, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !refresh && m.set && time.Since(m.at) < ttl {
		return m.val, m.err
	}
	at := time.Now()
	val, err := probe(ctx)
	if err != nil && ctx.Err() != nil {
		return val, err
	}
	m.set, m.at, m.val, m.err = true, at, val, err
	return val, err
}

// Value kept for the life of its Host once a probe answers, a failure kept only until a retry
type settleMemo[T any] struct {
	mu  sync.Mutex
	set bool
	at  time.Time
	val T
	err error
}

// Kept answer, else the kept failure while younger than retry, else a new probe
func (m *settleMemo[T]) get(retry time.Duration, probe func() (T, error)) (T, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.set && (m.err == nil || time.Since(m.at) < retry) {
		return m.val, m.err
	}
	m.at = time.Now()
	m.val, m.err = probe()
	m.set = true
	return m.val, m.err
}
