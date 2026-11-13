package connections

import (
	"sort"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

// Gateway a terminal tab recorded, a connection id and an optional profile
type TerminalGatewayOwner struct {
	ConnectionID string
	Profile      string
}

// Kinds of place a terminal shell runs
const (
	TerminalTargetLocal   = "local"
	TerminalTargetMissing = "missing"
	TerminalTargetNoShell = "no-shell"
	TerminalTargetPending = "pending"
	TerminalTargetSSH     = "ssh"
)

// Where a terminal shell runs, with the pool scope and connection of an SSH target
type TerminalGatewayTarget struct {
	Kind  string
	Scope string
	SSH   *SSHConnection
}

// One published SSH pool scope
type TerminalPoolEntry struct {
	Scope string
	State *SSHConnectionState
}

// Publication rank of a scope, kept when the scope is published again so the pool iterates like a Map
func (sl *sshSlice) publishRank(scope string) uint64 {
	if existing := sl.connections[scope]; existing != nil {
		return existing.published
	}
	sl.published++
	return sl.published
}

func poolState(pool []TerminalPoolEntry, scope string) *SSHConnectionState {
	for _, e := range pool {
		if e.Scope == scope {
			return e.State
		}
	}
	return nil
}

// Local, missing, no-shell, pending or the SSH scope of a tab owner against the registry and the pool in publication order
func TerminalGatewayTargetOf(owner TerminalGatewayOwner, registry ConnectionRegistry, pool []TerminalPoolEntry) TerminalGatewayTarget {
	id := jsTrim(owner.ConnectionID)
	if id == "" || id == LocalConnectionID {
		return TerminalGatewayTarget{Kind: TerminalTargetLocal}
	}
	source := registry.Find(id)
	if source == nil {
		return TerminalGatewayTarget{Kind: TerminalTargetMissing}
	}
	if source.Kind == "local" {
		return TerminalGatewayTarget{Kind: TerminalTargetLocal}
	}
	if source.Kind != "ssh" {
		return TerminalGatewayTarget{Kind: TerminalTargetNoShell}
	}
	scope := backend.BackendScopeKey(id, owner.Profile)
	if state := poolState(pool, scope); state != nil && state.SSH != nil {
		return TerminalGatewayTarget{Kind: TerminalTargetSSH, Scope: scope, SSH: state.SSH}
	}
	entries := make([]backend.RegistrySSHPoolEntry, 0, len(pool))
	for _, e := range pool {
		entries = append(entries, backend.RegistrySSHPoolEntry{Scope: e.Scope, RegistryConnectionID: e.State.RegistryConnectionID, HasSSH: e.State.SSH != nil})
	}
	if pooledScope, ok := backend.RegistrySSHPoolScopeByConnectionID(entries, id); ok {
		if pooled := poolState(pool, pooledScope); pooled != nil && pooled.SSH != nil {
			return TerminalGatewayTarget{Kind: TerminalTargetSSH, Scope: pooledScope, SSH: pooled.SSH}
		}
	}
	return TerminalGatewayTarget{Kind: TerminalTargetPending}
}

// SSH pool scopes in the order their connections were first published
func (s *Service) terminalPool() []TerminalPoolEntry {
	s.ssh.mu.Lock()
	defer s.ssh.mu.Unlock()
	out := make([]TerminalPoolEntry, 0, len(s.ssh.connections))
	for scope, state := range s.ssh.connections {
		out = append(out, TerminalPoolEntry{Scope: scope, State: state})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].State.published < out[j].State.published })
	return out
}

// Where a tab that recorded owner runs its shell, read against the registry and the live SSH pool
func (s *Service) TerminalGatewayTarget(owner TerminalGatewayOwner) (TerminalGatewayTarget, error) {
	registry, err := s.lockedConnectionsRegistry()
	if err != nil {
		return TerminalGatewayTarget{}, err
	}
	return TerminalGatewayTargetOf(owner, registry, s.terminalPool()), nil
}

// SSH target of the v1 route a profile resolves to, nil off SSH and pending until its tunnel is published
func (s *Service) V1SSHTerminalTarget(profile string) (*TerminalGatewayTarget, error) {
	route, err := s.desktopRemoteRoute(profile)
	if err != nil {
		return nil, err
	}
	if route == nil || route.Kind != "ssh" {
		return nil, nil
	}
	scope := V1SSHTerminalPoolKey(route.Source, profile)
	s.ssh.mu.Lock()
	state := s.ssh.connections[scope]
	s.ssh.mu.Unlock()
	if state != nil && state.SSH != nil {
		return &TerminalGatewayTarget{Kind: TerminalTargetSSH, Scope: scope, SSH: state.SSH}, nil
	}
	return &TerminalGatewayTarget{Kind: TerminalTargetPending}, nil
}

// Registry backend of a connection and profile, coalesced with every other dial of that scope
func (s *Service) EnsureRegistryBackendClaimed(connectionID, profile string) error {
	_, err := s.backendService().RunDialClaim(backend.BackendScopeKey(connectionID, profile), func() (backend.Connection, error) {
		return s.EnsureRegistryBackend(connectionID, profile)
	})
	return err
}

// Local or primary backend of a profile, coalesced with every other dial of that scope
func (s *Service) EnsureProfileBackendClaimed(profile string, inheritsPrimary bool) error {
	b := s.backendService()
	_, err := b.RunDialClaim(backend.PrimaryDialClaimKey(backend.BackendScopeKey("", profile), inheritsPrimary), func() (backend.Connection, error) {
		return b.EnsureBackend(profile, backend.EnsureOptions{InheritsPrimary: inheritsPrimary})
	})
	return err
}

// Copy of the SSH connection published under scope, false when none is
func (s *Service) SSHConnectionStateOf(scope string) (SSHConnectionState, bool) {
	s.ssh.mu.Lock()
	defer s.ssh.mu.Unlock()
	if state := s.ssh.connections[scope]; state != nil {
		return *state, true
	}
	return SSHConnectionState{}, false
}

// SSH client every desktop spawn runs, desktop.ssh_path then the platform candidates
func (s *Service) DesktopSSHBinary() string { return s.desktopSSHBinary() }
