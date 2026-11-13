package connections

// OpaqueProfileRoute is one connection qualified profile a plugin can address
type OpaqueProfileRoute struct {
	ConnectionID  string `json:"connectionId"`
	Mode          string `json:"mode"`
	Primary       bool   `json:"primary,omitempty"`
	Profile       string `json:"profile"`
	TargetProfile string `json:"targetProfile"`
}

func normalizeRouteProfile(name string) string { return orDefault(jsTrim(name), "default") }

// IsLocalEnumerationFailure reports a local listing that failed rather than waiting for a dial
func IsLocalEnumerationFailure(err string) bool { return err != "" && err != "connect-on-demand" }

// LocalRouteFallbackProfiles keeps cached local profile names routable while the local listing fails
func LocalRouteFallbackProfiles(agents []RosterAgent, localConnectionID string, profileNames []string, localEnumerationFailed bool) []string {
	if !localEnumerationFailed {
		return []string{}
	}
	existing := map[string]bool{}
	for _, a := range agents {
		if a.ConnectionID == localConnectionID {
			existing[normalizeRouteProfile(a.Profile)] = true
		}
	}
	fallback := []string{}
	for _, raw := range profileNames {
		profile := normalizeRouteProfile(raw)
		if !existing[profile] {
			existing[profile] = true
			fallback = append(fallback, profile)
		}
	}
	return fallback
}

// UndialedSSHRouteSeeds is one default route per SSH source no enumeration dialed
func UndialedSSHRouteSeeds(agents []RosterAgent, sources []RegistryConnection) []RosterAgent {
	dialed := map[string]bool{}
	for _, a := range agents {
		dialed[a.ConnectionID] = true
	}
	seeds := []RosterAgent{}
	for _, source := range sources {
		if source.Kind == "ssh" && !dialed[source.ID] {
			seeds = append(seeds, RosterAgent{ConnectionID: source.ID, Profile: "default"})
		}
	}
	return seeds
}

// BuildRegistryProfileRoutes maps roster agents to plugin routes, once per connection and profile
func BuildRegistryProfileRoutes(agents []RosterAgent, primaryConnectionID string, sources []RegistryConnection) []OpaqueProfileRoute {
	byID := map[string]RegistryConnection{}
	for _, source := range sources {
		byID[source.ID] = source
	}
	seen := map[string]bool{}
	routes := []OpaqueProfileRoute{}
	for _, agent := range agents {
		profile := normalizeRouteProfile(agent.Profile)
		source, ok := byID[agent.ConnectionID]
		key := agent.ConnectionID + "\x00" + profile
		if !ok || seen[key] {
			continue
		}
		seen[key] = true
		route := OpaqueProfileRoute{ConnectionID: source.ID, Primary: source.ID == primaryConnectionID, Profile: profile, TargetProfile: profile}
		if source.Kind == "local" {
			route.Mode = "local"
		} else {
			route.Mode = "remote"
			if source.Kind == "ssh" && source.RemoteProfile != "" {
				route.TargetProfile = normalizeRouteProfile(source.RemoteProfile)
			}
		}
		routes = append(routes, route)
	}
	return routes
}

// GetProfileRoutes answers every connection qualified profile route a plugin may address
func (s *Service) GetProfileRoutes(rawProfileNames any) ([]OpaqueProfileRoute, error) {
	fallbackNames := []string{}
	if list, ok := rawProfileNames.([]any); ok {
		for _, v := range list {
			if name, ok := v.(string); ok {
				if trimmed := jsTrim(name); trimmed != "" {
					fallbackNames = append(fallbackNames, trimmed)
				}
			}
		}
		if len(fallbackNames) > 256 {
			fallbackNames = fallbackNames[:256]
		}
	}
	registry, err := s.lockedConnectionsRegistry()
	if err != nil {
		return nil, err
	}
	enumerations := s.enumerateRegistryAgentSources(registry)
	agents := BuildAgentRoster(agentsOf(enumerations), registry.Primary)
	for _, seed := range UndialedSSHRouteSeeds(agents, registry.Connections) {
		source := registry.Find(seed.ConnectionID)
		agents = append(agents, RosterAgent{ConnectionID: source.ID, ConnectionKind: source.Kind, ConnectionLabel: source.Label,
			Handle: seed.Profile, Profile: seed.Profile})
	}
	var localSource *RegistryConnection
	for i := range registry.Connections {
		if registry.Connections[i].Kind == "local" {
			localSource = &registry.Connections[i]
			break
		}
	}
	if localSource != nil {
		localError := ""
		for _, e := range enumerations {
			if e.Connection.ID == localSource.ID {
				localError = e.Error
				break
			}
		}
		for _, profile := range LocalRouteFallbackProfiles(agents, localSource.ID, fallbackNames, IsLocalEnumerationFailure(localError)) {
			agents = append(agents, RosterAgent{ConnectionID: localSource.ID, ConnectionKind: localSource.Kind, ConnectionLabel: localSource.Label,
				Handle: profile, Profile: profile})
		}
	}
	return BuildRegistryProfileRoutes(agents, registry.Primary, registry.Connections), nil
}
