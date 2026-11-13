package backend

import (
	"fmt"
	"strings"
)

// HostBackendCollapseOptions say whether a profile request may share the one local backend of this host
type HostBackendCollapseOptions struct {
	Isolated              bool
	ProfileRemoteOverride bool
	PrimaryRemoteActive   bool
	UnscopableRequest     bool
}

// Whether a profile is served by the shared host backend instead of a second local one
func SharesHostBackend(o HostBackendCollapseOptions) bool {
	if o.Isolated || o.UnscopableRequest {
		return false
	}
	return !o.ProfileRemoteOverride && !o.PrimaryRemoteActive
}

// SecondLocalBackendError refuses a second local backend on a host that shares one
type SecondLocalBackendError struct{ PoolKey string }

func (e *SecondLocalBackendError) Error() string {
	return fmt.Sprintf("Refusing to start a second local Icebath backend for \"%s\": one backend serves every profile on this host. "+
		"Set ICEBATH_DESKTOP_ISOLATED_BACKEND=1 for a private backend.", e.PoolKey)
}

// Refuses a pool spawn for a profile the shared host backend serves
func AssertNoSecondLocalBackend(poolKey string, o HostBackendCollapseOptions) error {
	if SharesHostBackend(o) {
		return &SecondLocalBackendError{PoolKey: poolKey}
	}
	return nil
}

// Refuses to spawn a backend for a passive read that found no warm one
func AssertNotPassiveSpawn(passive bool, poolKey string) error {
	if passive {
		return fmt.Errorf("Passive read: no warm backend for \"%s\"", poolKey)
	}
	return nil
}

// Pool keys a local profile backend lives under, bare and registry scoped
func LocalProfilePoolKeys(profile string) []string {
	key := strings.ToLower(strings.TrimSpace(profile))
	if key == "" {
		return nil
	}
	return []string{key, "conn:local::" + key}
}

// Whether a remote primary that lacks the remembered profile may boot another one in its place
func PrimaryProfileRestorable(requestedProfile string, inheritsPrimary bool, launchArgProfile string, defaultHasRemoteConnection bool) bool {
	return (requestedProfile == "" || inheritsPrimary) && launchArgProfile == "" && !defaultHasRemoteConnection
}
