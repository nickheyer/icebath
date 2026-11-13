package connections

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

// Plaintext of a stored route token under the secret policy
func (s *Service) routeTokenPlaintext(token any) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.decryptDesktopSecret(token)
}

// Route of a profile from the v1 config, the remote env override and the registry
func (s *Service) desktopRemoteRoute(profile string) (*DesktopRemoteRoute, error) {
	registry, err := s.lockedConnectionsRegistry()
	if err != nil {
		return nil, err
	}
	return ResolveDesktopRemoteRoute(DesktopRemoteRouteInput{Config: s.lockedConnectionConfig(), EnvURL: os.Getenv("ICEBATH_DESKTOP_REMOTE_URL"),
		EnvToken: os.Getenv("ICEBATH_DESKTOP_REMOTE_TOKEN"), Profile: profile, Registry: registry})
}

// Managed SSH restore in flight for the primary profile, nil when none owns it
func (s *Service) managedPrimaryRestoreFor(profileKey string) *ManagedPrimaryRestore {
	s.cloud.mu.Lock()
	defer s.cloud.mu.Unlock()
	for _, owner := range s.cloud.managedPrimaryRestores {
		if owner.Profile == profileKey {
			found := owner
			return &found
		}
	}
	return nil
}

// ResolveRemote is the saved remote a profile runs on, nil when local, a managed restore dialing its captured snapshot
func (s *Service) ResolveRemote(profile string, o backend.RemoteResolveOptions) (backend.Connection, error) {
	profileKey := strings.TrimSpace(profile)
	if profileKey == "" {
		profileKey = "default"
	}
	if o.Primary {
		if managed := s.managedPrimaryRestoreFor(profileKey); managed != nil {
			source := managed.Source
			sshConfig := ManagedSSHConfig(source, profileKey)
			if sshConfig == nil {
				return nil, fmt.Errorf("SSH connection \"%s\" has no host configured.", source.Label)
			}
			if err := s.ssh.gate.AssertCanDial(source.ID, managed.CorrelationID); err != nil {
				return nil, err
			}
			current, err := s.desktopRemoteRoute(profileKey)
			if err != nil {
				return nil, err
			}
			persistenceSource := "registry:" + source.ID
			if current != nil && current.Kind == "ssh" && current.ConnectionID == source.ID {
				persistenceSource = current.Source
			}
			persistProfile := ""
			if persistenceSource == "profile" {
				persistProfile = profileKey
			}
			conn, err := s.bootstrapSSHConnection(persistProfile, *sshConfig, s.routeTokenPlaintext(source.Token), persistenceSource, "",
				SSHBootstrapMetadata{ManagedScope: "primary", ManagedUpdateCorrelation: managed.CorrelationID, PrimaryRegistryScope: true, RegistryConnectionID: source.ID})
			if err != nil {
				return nil, err
			}
			out := conn.Clone()
			out["connectionId"] = source.ID
			return out, nil
		}
	}
	var route *DesktopRemoteRoute
	if o.ForceRegistryPrimary {
		s.mu.Lock()
		s.registry = nil
		s.mu.Unlock()
		registry, err := s.lockedConnectionsRegistry()
		if err != nil {
			return nil, err
		}
		route = RegistryPrimaryBootRoute(registry)
	} else {
		var err error
		if route, err = s.desktopRemoteRoute(profile); err != nil {
			return nil, err
		}
	}
	if route == nil {
		return nil, nil
	}
	var conn backend.Connection
	var err error
	if route.Kind == "ssh" {
		if route.ConnectionID != "" {
			if err := s.ssh.gate.AssertCanDial(route.ConnectionID, ""); err != nil {
				return nil, err
			}
		}
		scope := "transient"
		if o.Primary {
			scope = "primary"
		} else if o.PoolKey != "" {
			scope = "pool"
		}
		persistProfile := ""
		if route.Source == "profile" {
			persistProfile = profile
		}
		conn, err = s.bootstrapSSHConnection(persistProfile, *route.SSH, s.routeTokenPlaintext(route.Token), route.Source, "",
			SSHBootstrapMetadata{ManagedScope: scope, PoolKey: o.PoolKey, PrimaryRegistryScope: o.Primary && route.ConnectionID != "", RegistryConnectionID: route.ConnectionID})
	} else {
		token := ""
		if route.AuthMode != "oauth" {
			if route.Source == "env" {
				token = jsOrString(route.Token)
			} else {
				token = s.routeTokenPlaintext(route.Token)
			}
		}
		kind := "url"
		if route.Kind == "cloud" {
			kind = "cloud"
		}
		conn, err = s.buildRemoteConnection(context.Background(), route.URL, route.AuthMode, token, route.Source, "", kind, nil, route.Headers)
	}
	if err != nil {
		return nil, err
	}
	if route.ConnectionID == "" {
		return conn, nil
	}
	out := conn.Clone()
	out["connectionId"] = route.ConnectionID
	return out, nil
}
