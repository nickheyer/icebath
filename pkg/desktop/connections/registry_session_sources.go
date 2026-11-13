package connections

import (
	"errors"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

// RegistrySessionSourceDeps are the registry, pool and probes the connected session sources are read from
type RegistrySessionSourceDeps struct {
	Connections    []RegistryConnection
	Pool           []backend.PoolBackend
	ScopePrefix    func(connectionID string) string
	ReusedPrimary  func(connectionID string) any
	InstallID      func(connectionID string, descriptor any) string
	SharedProfiles func(connectionID string, backends []RegistrySessionBackend) []string
	PendingMs      int
}

// Result of a backend future, an error once it is still pending after ms
func waitFuture(f *backend.Future[backend.Connection], ms int) (backend.Connection, error) {
	type result struct {
		c   backend.Connection
		err error
	}
	done := make(chan result, 1)
	go func() {
		c, err := f.Wait()
		done <- result{c, err}
	}()
	select {
	case r := <-done:
		return r.c, r.err
	case <-time.After(time.Duration(ms) * time.Millisecond):
		return nil, errors.New("pending")
	}
}

// CollectRegistrySessionSources lists every connected registry gateway as a session source, never dialing one
func CollectRegistrySessionSources(deps RegistrySessionSourceDeps, includeLocal bool) []RegistrySessionSource {
	pendingMs := deps.PendingMs
	if pendingMs == 0 {
		pendingMs = 2000
	}
	sources := []RegistrySessionSource{}
	for _, connection := range deps.Connections {
		if connection.Kind == "local" && !includeLocal {
			continue
		}
		prefix := deps.ScopePrefix(connection.ID)
		var pooled []backend.PoolBackend
		for _, entry := range deps.Pool {
			if strings.HasPrefix(entry.Key, prefix) && entry.Connection != nil {
				pooled = append(pooled, entry)
			}
		}
		perProfile := connection.Kind == "ssh" || (includeLocal && connection.Kind == "local")
		backends := []RegistrySessionBackend{}
		if connection.Kind == "ssh" {
			hasDefault := false
			for _, entry := range pooled {
				if orDefault(entry.Key[len(prefix):], "default") == "default" {
					hasDefault = true
					break
				}
			}
			if !hasDefault {
				if primary := deps.ReusedPrimary(connection.ID); jsTruthy(primary) {
					label := "default"
					backends = append(backends, RegistrySessionBackend{Descriptor: primary, ProfileLabel: &label})
				}
			}
		}
		read := pooled
		if !perProfile && len(pooled) > 1 {
			read = pooled[:1]
		}
		for _, entry := range read {
			descriptor, err := waitFuture(entry.Connection, pendingMs)
			if err != nil {
				continue
			}
			var label *string
			if perProfile {
				l := orDefault(entry.Key[len(prefix):], "default")
				label = &l
			}
			backends = append(backends, RegistrySessionBackend{Descriptor: map[string]any(descriptor), ProfileLabel: label})
		}
		if len(backends) > 0 {
			backendID := deps.InstallID(connection.ID, backends[0].Descriptor)
			var shared []string
			if connection.Kind == "ssh" {
				shared = deps.SharedProfiles(connection.ID, backends)
			}
			sources = append(sources, RegistrySessionSource{BackendID: backendID, Backends: backends, ConnectionID: connection.ID,
				Kind: connection.Kind, SharedProfiles: shared})
		}
	}
	return sources
}
