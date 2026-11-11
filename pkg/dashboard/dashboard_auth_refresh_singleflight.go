package dashboard

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

const (
	refreshSuccessTTL = 30 * time.Second
	refreshFailureTTL = 5 * time.Second
	refreshMaxEntries = 256
)

// Provider identity and refresh token digest one flight and cache entry is keyed by
type refreshKey struct {
	provider gateway.DashboardAuthProvider
	digest   [sha256.Size]byte
}

// Lock one refresh token's callers queue on and how many hold or wait on it
type refreshFlight struct {
	mu    sync.Mutex
	users int
}

// Cached outcome of one rotation until its expiry
type refreshCached struct {
	expires time.Time
	session *gateway.DashboardSession
}

var (
	refreshGuard   sync.Mutex
	refreshCache   = map[refreshKey]refreshCached{}
	refreshFlights = map[refreshKey]*refreshFlight{}
)

// Drops expired cache entries and then the soonest expiring past the size cap
func refreshPrune(now time.Time) {
	for k, v := range refreshCache {
		if !v.expires.After(now) {
			delete(refreshCache, k)
		}
	}
	for len(refreshCache) > refreshMaxEntries {
		var oldest refreshKey
		first := true
		for k, v := range refreshCache {
			if first || v.expires.Before(refreshCache[oldest].expires) {
				oldest, first = k, false
			}
		}
		delete(refreshCache, oldest)
	}
}

// Rotates the token through one provider once per burst, the rest of the burst served from the short cache
func refreshProvider(ctx context.Context, provider gateway.DashboardAuthProvider, token string) (*gateway.DashboardSession, error) {
	key := refreshKey{provider: provider, digest: sha256.Sum256([]byte(token))}
	refreshGuard.Lock()
	refreshPrune(time.Now())
	flight := refreshFlights[key]
	if flight == nil {
		flight = &refreshFlight{}
		refreshFlights[key] = flight
	}
	flight.users++
	refreshGuard.Unlock()
	defer func() {
		refreshGuard.Lock()
		flight.users--
		if flight.users == 0 {
			delete(refreshFlights, key)
		}
		refreshGuard.Unlock()
	}()
	flight.mu.Lock()
	defer flight.mu.Unlock()
	refreshGuard.Lock()
	if cached, ok := refreshCache[key]; ok && cached.expires.After(time.Now()) {
		refreshGuard.Unlock()
		return cached.session, nil
	}
	refreshGuard.Unlock()
	session, err := provider.RefreshSession(ctx, token)
	var expired *gateway.RefreshExpiredError
	if errors.As(err, &expired) {
		session, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	refreshGuard.Lock()
	now := time.Now()
	ttl := refreshFailureTTL
	if session != nil {
		ttl = refreshSuccessTTL
	}
	refreshCache[key] = refreshCached{expires: now.Add(ttl), session: session}
	refreshPrune(now)
	refreshGuard.Unlock()
	return session, nil
}

// Rotated session and the provider that rotated it
type refreshedSession struct {
	session  *gateway.DashboardSession
	provider string
}

// Rotates the token through the provider stack with per provider single flight, nil when every provider rejects it
func refreshSessionCoalesced(ctx context.Context, token, providerHint, phase string, log *slog.Logger,
	onRejected, onUnreachable func(gateway.DashboardAuthProvider)) (*refreshedSession, error) {
	call := func(provider gateway.DashboardAuthProvider) (*refreshedSession, error) {
		session, err := refreshProvider(ctx, provider, token)
		if err != nil {
			return nil, err
		}
		if session == nil {
			if onRejected != nil {
				onRejected(provider)
			}
			return nil, nil
		}
		return &refreshedSession{session: session, provider: provider.Name()}, nil
	}
	return scanSessionProviders(ctx, providerHint, call, providerScan{phase: phase, log: log, onUnreachable: onUnreachable})
}
