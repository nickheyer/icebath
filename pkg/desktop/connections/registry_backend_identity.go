package connections

import (
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

const (
	identityProbeBudgetMs = 2000
	// Milliseconds a learned install id stays fresh
	InstallIDTTLMs = 5 * 60000
	// Milliseconds an install id miss is kept before probing again
	InstallIDNegativeTTLMs = 60000
)

// BoundedInstallID waits for an install id probe at most the budget, empty on timeout or failure
func BoundedInstallID(probe func() string, budgetMs int) string {
	done := make(chan string, 1)
	go func() { done <- probe() }()
	select {
	case id := <-done:
		return id
	case <-time.After(time.Duration(budgetMs) * time.Millisecond):
		return ""
	}
}

// InstallIDFromStatus is the trimmed install_id a /api/status body reports, empty when absent
func InstallIDFromStatus(body any) string {
	raw, ok := jsObject(body)["install_id"].(string)
	if !ok {
		return ""
	}
	return jsTrim(raw)
}

// PrimaryInstallIDResolver remembers the install id of the backend the primary descriptor names
type PrimaryInstallIDResolver struct {
	mu            sync.Mutex
	resolve       func() (any, error)
	status        func(descriptor any) (any, error)
	ttlMs         int64
	negativeTTLMs int64
	cached        *struct {
		id  string
		key string
		ts  int64
	}
}

// NewPrimaryInstallIDResolver probes the primary status once per descriptor base URL and TTL
func NewPrimaryInstallIDResolver(resolve func() (any, error), status func(descriptor any) (any, error)) *PrimaryInstallIDResolver {
	return &PrimaryInstallIDResolver{resolve: resolve, status: status, ttlMs: InstallIDTTLMs, negativeTTLMs: InstallIDNegativeTTLMs}
}

// ID answers the primary backend install id, empty when it cannot be learned
func (r *PrimaryInstallIDResolver) ID() string {
	descriptor, err := r.resolve()
	if err != nil {
		return ""
	}
	key := jsOrString(jsObject(descriptor)["baseUrl"])
	if key == "" {
		return ""
	}
	now := time.Now().UnixMilli()
	r.mu.Lock()
	if c := r.cached; c != nil && c.key == key {
		ttl := r.negativeTTLMs
		if c.id != "" {
			ttl = r.ttlMs
		}
		if now-c.ts < ttl {
			r.mu.Unlock()
			return c.id
		}
	}
	r.mu.Unlock()
	id := BoundedInstallID(func() string {
		body, err := r.status(descriptor)
		if err != nil {
			return ""
		}
		return InstallIDFromStatus(body)
	}, identityProbeBudgetMs)
	r.mu.Lock()
	r.cached = &struct {
		id  string
		key string
		ts  int64
	}{id, key, time.Now().UnixMilli()}
	r.mu.Unlock()
	return id
}

// RememberConnectionInstallID caches the install id a status body names for a connection
func RememberConnectionInstallID(connectionID string, statusBody any) string {
	id := InstallIDFromStatus(statusBody)
	backend.SharedConnectionCaches.SetInstallID(connectionID, backend.InstallIDEntry{ID: id, TS: time.Now().UnixMilli()})
	return id
}

// ProbeCachedConnectionInstallID answers a fresh cached install id or probes status, a failed probe keeping the last known id
func ProbeCachedConnectionInstallID(connectionID string, fetchStatus func() (any, error)) string {
	cached, ok := backend.SharedConnectionCaches.InstallID(connectionID)
	if ok {
		ttl := int64(InstallIDNegativeTTLMs)
		if cached.ID != "" {
			ttl = InstallIDTTLMs
		}
		if time.Now().UnixMilli()-cached.TS < ttl {
			return cached.ID
		}
	}
	body, err := fetchStatus()
	if err == nil {
		return RememberConnectionInstallID(connectionID, body)
	}
	if ok && cached.ID != "" {
		return cached.ID
	}
	backend.SharedConnectionCaches.SetInstallID(connectionID, backend.InstallIDEntry{TS: time.Now().UnixMilli()})
	return ""
}
