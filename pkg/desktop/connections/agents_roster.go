package connections

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

// Milliseconds before an SSH source whose inventory failed is probed again
const sshInventoryRetryMs = 60000

// Lists the profiles and install id of an SSH source over a short lived probe session, at most once per retry window
func (s *Service) probeSSHProfileInventory(ctx context.Context, connection RegistryConnection) error {
	caches := backend.SharedConnectionCaches
	_, hasRoster := caches.SSHRoster(connection.ID)
	var last *int64
	if at, ok := caches.SSHInventoryAttemptedAt(connection.ID); ok {
		last = &at
	}
	if !ShouldRetrySSHInventory(hasRoster, last, time.Now().UnixMilli(), sshInventoryRetryMs) {
		return nil
	}
	caches.SetSSHInventoryAttemptedAt(connection.ID, time.Now().UnixMilli())
	entry := map[string]any{"mode": "ssh", "host": connection.Host, "user": connection.User, "keyPath": connection.KeyPath,
		"remoteHermesPath": connection.RemoteHermesPath}
	if connection.Port != 0 {
		entry["port"] = float64(connection.Port)
	}
	config := NormalizeSSHConfig(entry)
	if config == nil {
		return nil
	}
	ssh, err := NewSSHProbeConnection(SSHConnectionConfig{Host: config.Host, User: config.User, Port: config.Port, KeyPath: config.KeyPath},
		SSHConnectionOptions{RememberLog: s.sshRememberLog, SSHBinary: s.desktopSSHBinary()})
	if err != nil {
		return err
	}
	defer func() { _ = ssh.Close() }()
	inventory := func() error {
		if err := ssh.Open(ctx); err != nil {
			return err
		}
		platform, err := DetectRemotePlatform(ssh, config.RemoteHermesPath)
		if err != nil {
			return err
		}
		profiles, err := ListRemoteHermesProfiles(ssh, &platform)
		if err != nil {
			return err
		}
		if len(profiles) > 0 {
			caches.SetSSHRoster(connection.ID, profiles)
		}
		id, err := ReadRemoteInstallID(ssh, &platform)
		if err != nil {
			return err
		}
		caches.SetInstallID(connection.ID, backend.InstallIDEntry{ID: id, TS: time.Now().UnixMilli()})
		return nil
	}
	if err := inventory(); err != nil {
		s.sshRememberLog("[ssh] profile inventory failed for " + connection.ID + ": " + err.Error())
	}
	return nil
}

// Roster enumeration of one source with whether it needs a sign in
type rosterEnumeration struct {
	ConnectionAgents
	NeedsSignIn bool
}

var lineBreaks = regexp.MustCompile(`[\r\n]+`)

// Waits for a dial at most ms, the dial continuing after the deadline
func withEnumerationDeadline(dial func() (backend.Connection, error), ms int) (backend.Connection, error) {
	type result struct {
		c   backend.Connection
		err error
	}
	done := make(chan result, 1)
	go func() {
		c, err := dial()
		done <- result{c, err}
	}()
	select {
	case r := <-done:
		return r.c, r.err
	case <-time.After(time.Duration(ms) * time.Millisecond):
		return nil, errors.New("roster enumeration timed out")
	}
}

// Profile names and roster metadata of an /api/profiles body, default always listed
func rosterProfilesOf(body any) ([]string, map[string]RosterProfileMetadata) {
	list, isList := jsObject(body)["profiles"].([]any)
	profiles := []string{}
	var metadata map[string]RosterProfileMetadata
	if isList {
		metadata = map[string]RosterProfileMetadata{}
		for _, p := range list {
			obj := jsObject(p)
			name := jsTrim(jsOrString(obj["name"]))
			if name == "" {
				continue
			}
			profiles = append(profiles, name)
			m := RosterProfileMetadata{}
			if d, ok := obj["display_name"].(string); ok && jsTrim(d) != "" {
				m.DisplayName = jsTrim(d)
			}
			if t, ok := obj["bot_title"].(string); ok {
				title := jsTrim(t)
				m.Title = &title
			}
			if u, ok := obj["ui_meta"].(map[string]any); ok && u != nil {
				m.UIMeta = u
			}
			if a, ok := obj["has_avatar"].(bool); ok {
				m.HasAvatar = &a
			}
			metadata[name] = m
		}
	}
	hasDefault := false
	for _, p := range profiles {
		if p == "default" {
			hasDefault = true
		}
	}
	if !hasDefault {
		profiles = append([]string{"default"}, profiles...)
	}
	return profiles, metadata
}

// Enumerates every registered source concurrently, a dead source reported instead of failing the roster
func (s *Service) enumerateRegistryAgentSources(registry ConnectionRegistry) []rosterEnumeration {
	ctx := WithoutInteractiveOauthLogin(context.Background())
	caches := backend.SharedConnectionCaches
	out := make([]rosterEnumeration, len(registry.Connections))
	parallel(len(registry.Connections), func(i int) {
		connection := registry.Connections[i]
		detail := ""
		raw := rosterEnumeration{ConnectionAgents: ConnectionAgents{Connection: connection}}
		enumerate := func() error {
			if connection.Kind == "ssh" {
				if err := s.probeSSHProfileInventory(ctx, connection); err != nil {
					return err
				}
				raw.Error = "connect-on-demand"
				if known, ok := caches.InstallID(connection.ID); ok {
					raw.InstallID = known.ID
				}
				return nil
			}
			if connection.Kind == "local" {
				route := ResolveRegistryLocalRoute("default", LocalRouteOptions{GlobalRemote: s.globalRemoteActive(),
					ProfileRemoteOverride: s.profileHasRemoteOverride(s.local.backend.PrimaryProfileKey())})
				if ShouldDeferLocalEnumeration(route, s.poolKeys(), connection.ID) {
					return errDeferredEnumeration
				}
			}
			descriptor, err := withEnumerationDeadline(func() (backend.Connection, error) {
				return s.local.backend.RunDialClaim(backend.BackendScopeKey(connection.ID, ""), func() (backend.Connection, error) {
					return s.EnsureRegistryBackend(connection.ID, "")
				})
			}, RosterSourceEnumerationTimeoutMs(connection.Kind, connection.AuthMode))
			if err != nil {
				return err
			}
			body, installID, err := FetchRosterSourceData(
				func() (any, error) {
					return s.fetchJSONForBackend(ctx, descriptor, "/api/profiles", backendFetchOptions{TimeoutMs: 8000})
				},
				func() string { return s.probeConnectionInstallID(ctx, connection.ID, map[string]any(descriptor)) })
			if err != nil {
				return err
			}
			raw.Profiles, raw.ProfileMetadata = rosterProfilesOf(body)
			raw.InstallID = installID
			return nil
		}
		err := enumerate()
		if errors.Is(err, errDeferredEnumeration) {
			out[i] = rosterEnumeration{ConnectionAgents: ConnectionAgents{Connection: connection, Error: "connect-on-demand"}}
			return
		}
		if err != nil {
			parts := []string{}
			if code, ok := readStatusCode(err); ok && code != 0 {
				parts = append(parts, jsNumber(float64(code)))
			}
			if cause := errors.Unwrap(err); cause != nil && cause.Error() != "" {
				parts = append(parts, cause.Error())
			}
			detail = strings.Join(parts, " | ")
			raw = rosterEnumeration{ConnectionAgents: ConnectionAgents{Connection: connection, Error: RedactSecrets(err.Error())},
				NeedsSignIn: backend.IsReauthRequiredError(err) || (connection.AuthMode == "oauth" && IsGatewayAuthRejection(err))}
		}
		if raw.Error != "" && raw.Error != "connect-on-demand" {
			parts := []string{raw.Error}
			if detail != "" {
				parts = append(parts, detail)
			}
			diagnostic := jsStringSlice(lineBreaks.ReplaceAllString(RedactSecrets(strings.Join(parts, " | ")), " "), 800)
			if previous, ok := caches.RosterSourceError(connection.ID); !ok || previous != diagnostic {
				s.log("[fleet-roster] " + connection.ID + ": " + diagnostic)
				caches.SetRosterSourceError(connection.ID, diagnostic)
			}
		} else if raw.Profiles != nil {
			if _, ok := caches.RosterSourceError(connection.ID); ok {
				caches.DeleteRosterSourceError(connection.ID)
				s.log("[fleet-roster] " + connection.ID + ": connection recovered")
			}
		}
		if len(raw.Profiles) > 0 {
			caches.SetSSHRoster(connection.ID, raw.Profiles)
		}
		cached, _ := caches.SSHRoster(connection.ID)
		remembered := RememberSSHEnumeration(SSHEnumeration{Profiles: raw.Profiles, Error: raw.Error}, cached, connection.Kind)
		raw.Profiles, raw.Error = remembered.Profiles, remembered.Error
		out[i] = raw
	})
	return out
}

var errDeferredEnumeration = errors.New("connect-on-demand")

// Pool keys of the backends this app holds
func (s *Service) poolKeys() []string {
	pool := s.local.backend.PoolBackends()
	keys := make([]string, len(pool))
	for i, p := range pool {
		keys[i] = p.Key
	}
	return keys
}

// AgentRosterSource is one source row of the union roster
type AgentRosterSource struct {
	ConnectionID string `json:"connectionId"`
	Label        string `json:"label"`
	Kind         string `json:"kind"`
	RosterSourceStatus
	InstallID string `json:"installId,omitempty"`
}

// AgentRoster is the union agent roster across every registered connection
type AgentRoster struct {
	Agents              []RosterAgent       `json:"agents"`
	PrimaryConnectionID string              `json:"primaryConnectionId"`
	Sources             []AgentRosterSource `json:"sources"`
}

func agentsOf(enumerations []rosterEnumeration) []ConnectionAgents {
	out := make([]ConnectionAgents, len(enumerations))
	for i, e := range enumerations {
		out[i] = e.ConnectionAgents
	}
	return out
}

// GetAgentRoster enumerates every registered connection and flattens its profiles into the union roster
func (s *Service) GetAgentRoster() (AgentRoster, error) {
	registry, err := s.lockedConnectionsRegistry()
	if err != nil {
		return AgentRoster{}, err
	}
	enumerations := s.enumerateRegistryAgentSources(registry)
	roster := AgentRoster{Agents: BuildAgentRoster(agentsOf(enumerations), registry.Primary), PrimaryConnectionID: registry.Primary,
		Sources: make([]AgentRosterSource, len(enumerations))}
	for i, e := range enumerations {
		roster.Sources[i] = AgentRosterSource{ConnectionID: e.Connection.ID, Label: e.Connection.Label, Kind: e.Connection.Kind,
			RosterSourceStatus: RosterSourceStatusOf(e.Profiles, e.Error, e.NeedsSignIn), InstallID: e.InstallID}
	}
	return roster, nil
}
