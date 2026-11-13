package pty

import (
	"github.com/nickheyer/icebath/pkg/desktop/backend"
	"github.com/nickheyer/icebath/pkg/desktop/connections"
)

// Connection routing a terminal reads to choose and reach the host its shell runs on
type Routes interface {
	TerminalGatewayTarget(owner connections.TerminalGatewayOwner) (connections.TerminalGatewayTarget, error)
	V1SSHTerminalTarget(profile string) (*connections.TerminalGatewayTarget, error)
	EnsureRegistryBackendClaimed(connectionID, profile string) error
	EnsureProfileBackendClaimed(profile string, inheritsPrimary bool) error
	SSHConnectionStateOf(scope string) (connections.SSHConnectionState, bool)
	DesktopSSHBinary() string
}

// Gateway a start payload names, nil for a tab that recorded none
func RequestedTerminalGateway(payload any) *connections.TerminalGatewayOwner {
	id, _ := get(payload, "connectionId").(string)
	id = jsTrim(id)
	if id == "" {
		return nil
	}
	owner := &connections.TerminalGatewayOwner{ConnectionID: id}
	if profile, ok := get(payload, "profile").(string); ok {
		owner.Profile = jsTrim(profile)
	}
	return owner
}

func routeProfile(route *backend.WindowConnectionRoute) string {
	if route == nil || route.Profile == nil {
		return ""
	}
	return *route.Profile
}

// SSH target of a window, its registry gateway when registry scoped, else the v1 route of its profile, nil off SSH
func (s *Service) activeSSHTerminalTarget(windowID string) (*connections.TerminalGatewayTarget, error) {
	route := s.opts.WindowRoutes.Get(windowID)
	if route != nil && route.RegistryScoped && route.ConnectionID != nil && *route.ConnectionID != "" {
		target, err := s.opts.Routes.TerminalGatewayTarget(connections.TerminalGatewayOwner{ConnectionID: *route.ConnectionID, Profile: routeProfile(route)})
		if err != nil {
			return nil, err
		}
		if target.Kind != connections.TerminalTargetSSH && target.Kind != connections.TerminalTargetPending {
			return nil, nil
		}
		return &target, nil
	}
	profile := s.opts.PrimaryProfile()
	if route != nil && route.Profile != nil {
		profile = *route.Profile
	}
	return s.opts.Routes.V1SSHTerminalTarget(profile)
}

// Brings up the backend a window routes to, its registry connection or its profile backend
func (s *Service) ensureTerminalBackend(windowID string) error {
	route := s.opts.WindowRoutes.Get(windowID)
	if route != nil && route.RegistryScoped && route.ConnectionID != nil && *route.ConnectionID != "" {
		return s.opts.Routes.EnsureRegistryBackendClaimed(*route.ConnectionID, routeProfile(route))
	}
	profile := s.opts.PrimaryProfile()
	if route != nil && route.Profile != nil {
		profile = *route.Profile
	}
	return s.opts.Routes.EnsureProfileBackendClaimed(profile, routeProfile(route) == "")
}

type windowTarget struct {
	target *connections.TerminalGatewayTarget
	err    error
}

type ownerTarget struct {
	target connections.TerminalGatewayTarget
	err    error
}

// Recorded gateway of a tab decides its host, a tab with none follows the window route
func (s *Service) resolveStartTarget(owner *connections.TerminalGatewayOwner, windowID string) (*connections.TerminalGatewayTarget, string, error) {
	if owner == nil {
		got, err := backend.ResolveTerminalConnectionForSender(windowID, func(id string) (windowTarget, bool) {
			target, err := s.activeSSHTerminalTarget(id)
			if err != nil {
				return windowTarget{err: err}, false
			}
			return windowTarget{target: target}, target != nil && target.Kind == connections.TerminalTargetPending
		}, s.ensureTerminalBackend)
		if err != nil {
			return nil, "", err
		}
		return got.target, "", got.err
	}
	got, err := backend.ResolveTerminalConnection(func() (ownerTarget, bool) {
		target, err := s.opts.Routes.TerminalGatewayTarget(*owner)
		if err != nil {
			return ownerTarget{err: err}, false
		}
		return ownerTarget{target: target}, target.Kind == connections.TerminalTargetPending
	}, func() error { return s.opts.Routes.EnsureRegistryBackendClaimed(owner.ConnectionID, owner.Profile) })
	if err != nil {
		return nil, "", err
	}
	if got.err != nil {
		return nil, "", got.err
	}
	switch got.target.Kind {
	case connections.TerminalTargetMissing, connections.TerminalTargetNoShell:
		return nil, got.target.Kind, nil
	case connections.TerminalTargetLocal:
		return nil, "", nil
	}
	return &got.target, "", nil
}

// Host the shell ran on, so a tab that recorded no gateway adopts it
func (s *Service) spawnedOn(owner *connections.TerminalGatewayOwner, sshTarget *connections.TerminalGatewayTarget) *string {
	if owner != nil {
		id := owner.ConnectionID
		return &id
	}
	if sshTarget == nil {
		local := connections.LocalConnectionID
		return &local
	}
	if state, ok := s.opts.Routes.SSHConnectionStateOf(sshTarget.Scope); ok && state.RegistryConnectionID != "" {
		id := state.RegistryConnectionID
		return &id
	}
	return nil
}
