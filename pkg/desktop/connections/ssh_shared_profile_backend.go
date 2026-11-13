package connections

import (
	"errors"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

// SSH config of a registry connection for one profile, a named profile re homing the remote unless the entry pins one
func ManagedSSHConfig(source RegistryConnection, profile string) *SSHConfig {
	profileKey := strings.TrimSpace(profile)
	if profileKey == "" {
		profileKey = "default"
	}
	remoteProfile := source.RemoteProfile
	if remoteProfile == "" && profileKey != "default" {
		remoteProfile = profileKey
	}
	entry := map[string]any{"mode": "ssh", "host": source.Host, "user": source.User, "keyPath": source.KeyPath, "remoteHermesPath": source.RemoteHermesPath, "remoteProfile": remoteProfile}
	if source.Port != 0 {
		entry["port"] = float64(source.Port)
	}
	return NormalizeSSHConfig(entry)
}

var (
	servedProfilesMu sync.Mutex
	servedProfiles   = map[string][]string{}
)

// SharedSSHBackend is one roster backend with the profile label it was dialed for
type SharedSSHBackend struct {
	Descriptor   backend.Connection
	ProfileLabel string
}

// Profiles the default backend of an SSH connection was seen serving, nil when never listed
func SharedSSHProfiles(connectionID string, backends []SharedSSHBackend) []string {
	for _, b := range backends {
		label := b.ProfileLabel
		if label == "" {
			label = "default"
		}
		if label != "default" {
			continue
		}
		servedProfilesMu.Lock()
		defer servedProfilesMu.Unlock()
		served, ok := servedProfiles[connectionID+"\x00"+jsOrString(b.Descriptor["baseUrl"])]
		if !ok {
			return nil
		}
		return append([]string(nil), served...)
	}
	return nil
}

func profileNames(body any) []string {
	profiles, ok := jsObject(toJSONValue(body))["profiles"].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, entry := range profiles {
		name := strings.ToLower(strings.TrimSpace(jsNullishString(jsObject(entry)["name"])))
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// Rides a named profile on the default SSH backend of the connection when that backend serves it
func ResolveSharedSSHProfileBackend(source RegistryConnection, profile string, ensureDefault func() (backend.Connection, error), listProfiles func(backend.Connection) (any, error)) (backend.Connection, error) {
	key := strings.ToLower(strings.TrimSpace(profile))
	if source.Kind != "ssh" || strings.TrimSpace(source.RemoteProfile) != "" || key == "" || key == "default" {
		return nil, nil
	}
	base, err := ensureDefault()
	if err != nil {
		return nil, err
	}
	cacheKey := source.ID + "\x00" + jsOrString(base["baseUrl"])
	servedProfilesMu.Lock()
	served, ok := servedProfiles[cacheKey]
	servedProfilesMu.Unlock()
	if !ok || !containsString(served, key) {
		body, err := listProfiles(base)
		if err != nil {
			var httpErr *backend.HTTPError
			if errors.As(err, &httpErr) && httpErr.StatusCode == 404 {
				return nil, nil
			}
			return nil, err
		}
		names := profileNames(body)
		if len(names) == 0 {
			return nil, nil
		}
		if !containsString(names, key) {
			e := lifecycleError("remote-profile-missing", "The remote Icebath profile '"+key+"' does not exist. Select an existing remote profile.", nil)
			e.Profile = key
			return nil, e
		}
		unique := make([]string, 0, len(names))
		for _, n := range names {
			if !containsString(unique, n) {
				unique = append(unique, n)
			}
		}
		servedProfilesMu.Lock()
		servedProfiles[cacheKey] = unique
		servedProfilesMu.Unlock()
	}
	out := base.Clone()
	out["profile"], out["connectionId"], out["sharedRemote"], out["remoteProfile"] = key, source.ID, true, ""
	return out, nil
}
