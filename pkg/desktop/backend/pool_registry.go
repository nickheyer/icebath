package backend

import "fmt"

// How a pool dial resolves, ForceLocal skipping the remote routes and PoolKey naming a composite entry
type poolDialOptions struct {
	ForceLocal bool
	PoolKey    string
}

// PoolSlot is the pool entry a registry dial fills while it stays the cached one at its key
type PoolSlot struct {
	s     *Service
	key   string
	entry *poolEntry
}

// Records the remote base URL a revalidation probes without waiting for the dial
func (p PoolSlot) SetRemoteBaseURL(baseURL string) {
	p.s.mu.Lock()
	p.entry.remoteBaseURL = baseURL
	p.s.mu.Unlock()
}

// Cached pool connection at key, nil when none
func (s *Service) PoolConnection(key string) *Future[Connection] {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry := s.pool[key]; entry != nil {
		return entry.future
	}
	return nil
}

// Every pool key in no particular order
func (s *Service) PoolKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.pool))
	for key := range s.pool {
		keys = append(keys, key)
	}
	return keys
}

// Installs a fresh pool entry at key whose connection is dial, forgetting it when dial fails and it is still cached
func (s *Service) StartPoolEntry(key string, dial func(slot PoolSlot) (Connection, error)) *Future[Connection] {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := &poolEntry{}
	slot := PoolSlot{s: s, key: key, entry: entry}
	entry.future = NewFuture(func() (Connection, error) {
		conn, err := dial(slot)
		if err != nil {
			s.mu.Lock()
			if s.pool[key] == entry {
				delete(s.pool, key)
			}
			s.mu.Unlock()
		}
		return conn, err
	})
	s.pool[key] = entry
	return entry.future
}

// Pools a forced local backend of profile at poolKey, logging and forgetting a failed spawn
func (s *Service) StartForcedLocalPoolEntry(profile, poolKey string) *Future[Connection] {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := &poolEntry{}
	entry.future = NewFuture(func() (Connection, error) {
		conn, err := s.dialPoolBackend(profile, entry, poolDialOptions{ForceLocal: true, PoolKey: poolKey})
		if err != nil {
			s.Log(fmt.Sprintf("Icebath backend for profile \"%s\" (forced-local) failed to start: %s", profile, err.Error()))
			s.mu.Lock()
			if s.pool[poolKey] == entry {
				delete(s.pool, poolKey)
			}
			s.mu.Unlock()
		}
		return conn, err
	})
	s.pool[poolKey] = entry
	return entry.future
}

// Descriptor a window gets for a connection and profile route, the getConnectionFor dial
func (s *Service) ConnectProfileRoute(windowID, connectionID, profile string) (Connection, error) {
	return s.connectRoute(windowID, connectionID, profile, false)
}

// Forgets the cached primary so the next dial boots it again
func (s *Service) InvalidatePrimary() { s.state.Invalidate() }

// Forgets the cached primary and boots it again, the managed SSH primary restore
func (s *Service) RestartPrimary() (Connection, error) {
	s.state.Invalidate()
	return s.startPrimary("", false)
}
