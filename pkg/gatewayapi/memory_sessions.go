package gatewayapi

import (
	"context"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Registry key of a parked manager, the owning home key and the session id
type memorySessionKey struct{ homeKey, sessionID string }

// Parked manager with its owning home and the monotonic and wall clock of its last check in
type memorySessionEntry struct {
	manager  *memory.MemoryManager
	owner    string
	lastUsed time.Time
	wall     float64
}

// APIServerMemorySessions keeps one initialised memory manager per profile home and session across requests
type APIServerMemorySessions struct {
	mu         sync.Mutex
	order      []memorySessionKey
	entries    map[memorySessionKey]memorySessionEntry
	maxSize    *int
	idleTTLSec *float64
}

// Registry with optional fixed bounds, the gateway agent cache bounds resolved lazily otherwise
func NewAPIServerMemorySessions(maxSize *int, idleTTLSecs *float64) *APIServerMemorySessions {
	return &APIServerMemorySessions{entries: map[memorySessionKey]memorySessionEntry{}, maxSize: maxSize, idleTTLSec: idleTTLSecs}
}

// Size and idle bounds, the configured agent cache knobs or their defaults
func (m *APIServerMemorySessions) bounds(ctx context.Context) (int, float64) {
	if m.maxSize == nil || m.idleTTLSec == nil {
		var configured *gateway.AgentCacheBounds
		func() {
			defer func() { _ = recover() }()
			b := gateway.ResolveAgentCacheBounds(gateway.LoadGatewayConfigMap(ctx, ""))
			configured = &b
		}()
		if m.maxSize == nil {
			size := gateway.AgentCacheMaxSize
			if configured != nil && configured.MaxSize != nil && *configured.MaxSize != 0 {
				size = *configured.MaxSize
			}
			m.maxSize = &size
		}
		if m.idleTTLSec == nil {
			ttl := gateway.AgentCacheIdleTTLSecs
			if configured != nil && configured.IdleTTLSecs != nil && *configured.IdleTTLSecs != 0 {
				ttl = *configured.IdleTTLSecs
			}
			m.idleTTLSec = &ttl
		}
	}
	return *m.maxSize, *m.idleTTLSec
}

// Registry key and home of the current scope
func ownerHome(ctx context.Context) (string, string) {
	h := home.Dir(ctx)
	return home.Key(h), h
}

// Removes a key from the ordering, caller holds the lock
func (m *APIServerMemorySessions) dropOrderLocked(key memorySessionKey) {
	for i, k := range m.order {
		if k == key {
			m.order = append(m.order[:i], m.order[i+1:]...)
			return
		}
	}
}

// Manager an earlier request on the session checked in, nil when none or parked past the retry horizon
func (m *APIServerMemorySessions) Checkout(ctx context.Context, sessionID string) *memory.MemoryManager {
	if sessionID == "" {
		return nil
	}
	homeKey, _ := ownerHome(ctx)
	key := memorySessionKey{homeKey, sessionID}
	m.mu.Lock()
	entry, ok := m.entries[key]
	if ok {
		delete(m.entries, key)
		m.dropOrderLocked(key)
	}
	m.mu.Unlock()
	if !ok {
		return nil
	}
	if apiNow()-entry.wall > state.RetryHorizonS {
		m.shutdownAsync(ctx, entry.manager, entry.owner)
		return nil
	}
	return entry.manager
}

// Parks the agent's manager under its final session, shutting down displaced or idle managers
func (m *APIServerMemorySessions) Checkin(ctx context.Context, a *agent.AIAgent) {
	manager, _ := a.MemoryManager().(*memory.MemoryManager)
	sessionID := a.SessionID()
	if manager == nil || sessionID == "" {
		return
	}
	homeKey, h := ownerHome(ctx)
	maxSize, idleTTL := m.bounds(ctx)
	now := time.Now()
	type doomedEntry struct {
		manager *memory.MemoryManager
		owner   string
	}
	var doomed []doomedEntry
	key := memorySessionKey{homeKey, sessionID}
	m.mu.Lock()
	if displaced, ok := m.entries[key]; ok {
		delete(m.entries, key)
		m.dropOrderLocked(key)
		if displaced.manager != manager {
			doomed = append(doomed, doomedEntry{displaced.manager, displaced.owner})
		}
	}
	m.entries[key] = memorySessionEntry{manager: manager, owner: h, lastUsed: now, wall: apiNow()}
	m.order = append(m.order, key)
	for _, k := range append([]memorySessionKey(nil), m.order...) {
		e := m.entries[k]
		if e.manager == manager {
			continue
		}
		if now.Sub(e.lastUsed).Seconds() > idleTTL || len(m.entries) > maxSize {
			delete(m.entries, k)
			m.dropOrderLocked(k)
			doomed = append(doomed, doomedEntry{e.manager, e.owner})
		}
	}
	m.mu.Unlock()
	for _, d := range doomed {
		m.shutdownAsync(ctx, d.manager, d.owner)
	}
}

// Drains and shuts down every parked manager inline at adapter shutdown
func (m *APIServerMemorySessions) CloseAll(ctx context.Context) {
	m.mu.Lock()
	entries := make([]memorySessionEntry, 0, len(m.order))
	for _, k := range m.order {
		entries = append(entries, m.entries[k])
	}
	m.entries = map[memorySessionKey]memorySessionEntry{}
	m.order = nil
	m.mu.Unlock()
	for _, e := range entries {
		shutdownMemoryManager(ctx, e.manager, e.owner)
	}
}

// Evicts a manager off the request path so no reply waits on a provider drain
func (m *APIServerMemorySessions) shutdownAsync(ctx context.Context, manager *memory.MemoryManager, owner string) {
	go shutdownMemoryManager(context.WithoutCancel(ctx), manager, owner)
}

// Bounded drain then provider shutdown under the owning profile's scope
func shutdownMemoryManager(ctx context.Context, manager *memory.MemoryManager, owner string) {
	defer func() {
		if rec := recover(); rec != nil {
			apiLog().Debug("api_server memory manager shutdown failed", "error", rec)
		}
	}()
	scoped := ctx
	if owner != "" && provider.IsMultiplexActive(ctx) {
		scoped = gateway.ProfileRuntimeScope(ctx, owner, true)
	}
	func() {
		defer func() { _ = recover() }()
		manager.FlushPending(10 * time.Second)
	}()
	manager.ShutdownAll(scoped)
}

// Parked managers by key, for tests
func (m *APIServerMemorySessions) Parked() map[[2]string]*memory.MemoryManager {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[[2]string]*memory.MemoryManager, len(m.entries))
	for k, e := range m.entries {
		out[[2]string{k.homeKey, k.sessionID}] = e.manager
	}
	return out
}
