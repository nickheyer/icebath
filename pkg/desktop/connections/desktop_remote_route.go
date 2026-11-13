package connections

import (
	"errors"
	"strings"
)

// DesktopRemoteRoute is the saved remote a profile resolves to, URL shaped or SSH
type DesktopRemoteRoute struct {
	Kind         string
	Source       string
	ConnectionID string
	AuthMode     string
	Headers      any
	Org          string
	Token        any
	URL          string
	SSH          *SSHConfig
}

// DesktopRemoteRouteInput is the v1 config, the remote env override, the profile and the registry a route is chosen from
type DesktopRemoteRouteInput struct {
	Config   map[string]any
	EnvURL   string
	EnvToken string
	Profile  string
	Registry ConnectionRegistry
}

// V1SSHTerminalPoolKey is the profile scope a v1 SSH terminal pools under, empty for the app wide route
func V1SSHTerminalPoolKey(source, profile string) string {
	if source == "profile" {
		return ConnectionScopeKey(profile)
	}
	return ""
}

func withKind(base map[string]any, kind string) map[string]any {
	out := jsSpread(base, nil)
	out["kind"] = kind
	return out
}

// ResolveDesktopRemoteRoute is a profile override, then the env remote, then the app wide setting, then the registry primary
func ResolveDesktopRemoteRoute(in DesktopRemoteRouteInput) (*DesktopRemoteRoute, error) {
	profileKey := ConnectionScopeKey(in.Profile)
	var profileConfig map[string]any
	if profileKey != "" {
		profileConfig = jsObject(jsObject(in.Config["profiles"])[profileKey])
	}
	if ssh := ProfileSSHOverride(in.Config, in.Profile); ssh != nil {
		return &DesktopRemoteRoute{Kind: "ssh", Source: "profile", SSH: ssh, Token: profileConfig["token"],
			ConnectionID: MatchingConnectionID(in.Registry, withKind(ssh.object(), "ssh"), "unique")}, nil
	}
	if override := ProfileRemoteOverride(in.Config, in.Profile); override != nil {
		kind := "remote"
		if profileConfig["mode"] == "cloud" {
			kind = "cloud"
		}
		authMode := "token"
		if override.AuthMode == "oauth" {
			authMode = "oauth"
		}
		route := &DesktopRemoteRoute{Kind: kind, Source: "profile", AuthMode: authMode, Token: override.Token, URL: override.URL,
			ConnectionID: MatchingConnectionID(in.Registry, withKind(profileConfig, kind), "unique")}
		if override.Headers != nil {
			route.Headers = override.Headers
		}
		if kind == "cloud" {
			route.Org = strings.TrimSpace(jsOrString(profileConfig["org"]))
		}
		return route, nil
	}
	if envURL := strings.TrimSpace(in.EnvURL); envURL != "" {
		envToken := strings.TrimSpace(in.EnvToken)
		if envToken == "" {
			return nil, errors.New("ICEBATH_DESKTOP_REMOTE_URL is set but ICEBATH_DESKTOP_REMOTE_TOKEN is not. " +
				"Both must be provided to connect to a remote Icebath backend.")
		}
		return &DesktopRemoteRoute{Kind: "remote", Source: "env", AuthMode: "token", Token: envToken, URL: envURL}, nil
	}
	remote := jsObject(in.Config["remote"])
	if in.Config["mode"] == "ssh" {
		ssh := NormalizeSSHConfig(jsSpread(map[string]any{"mode": "ssh"}, remote))
		if ssh == nil {
			return nil, errors.New("SSH remote mode is selected but no host is configured.")
		}
		return &DesktopRemoteRoute{Kind: "ssh", Source: "settings", SSH: ssh, Token: remote["token"],
			ConnectionID: MatchingConnectionID(in.Registry, withKind(ssh.object(), "ssh"), "primary")}, nil
	}
	if !ModeIsRemoteLike(in.Config["mode"]) {
		return resolveRegistryPrimaryRoute(in.Registry), nil
	}
	kind := "remote"
	if in.Config["mode"] == "cloud" {
		kind = "cloud"
	}
	route := &DesktopRemoteRoute{Kind: kind, Source: "settings", AuthMode: NormAuthMode(remote["authMode"]), Headers: remote["headers"],
		Token: remote["token"], URL: jsOrString(remote["url"]), ConnectionID: MatchingConnectionID(in.Registry, withKind(remote, kind), "primary")}
	if kind == "cloud" {
		route.Org = strings.TrimSpace(jsOrString(remote["org"]))
	}
	return route, nil
}

// Route of the registry primary when it names a remote, cloud or SSH connection
func resolveRegistryPrimaryRoute(registry ConnectionRegistry) *DesktopRemoteRoute {
	primaryID := strings.TrimSpace(registry.Primary)
	if primaryID == "" {
		return nil
	}
	entry := registry.Find(primaryID)
	if entry == nil {
		return nil
	}
	if entry.Kind == "ssh" {
		ssh := NormalizeSSHConfig(jsSpread(jsObject(toJSONValue(*entry)), map[string]any{"mode": "ssh"}))
		if ssh == nil {
			return nil
		}
		return &DesktopRemoteRoute{ConnectionID: entry.ID, Kind: "ssh", Source: "registry", SSH: ssh, Token: entry.Token}
	}
	if entry.Kind != "remote" && entry.Kind != "cloud" {
		return nil
	}
	url := strings.TrimSpace(entry.URL)
	if url == "" {
		return nil
	}
	route := &DesktopRemoteRoute{AuthMode: NormAuthMode(entry.AuthMode), ConnectionID: entry.ID, Kind: entry.Kind, Source: "registry", Token: entry.Token, URL: url}
	if entry.Headers != nil {
		route.Headers = entry.Headers
	}
	if entry.Kind == "cloud" {
		route.Org = strings.TrimSpace(entry.Org)
	}
	return route
}

// RegistryPrimaryBootRoute is the registry primary route when the registry launches the primary
func RegistryPrimaryBootRoute(registry ConnectionRegistry) *DesktopRemoteRoute {
	if registry.LaunchMode != "primary" {
		return nil
	}
	return resolveRegistryPrimaryRoute(registry)
}
