package gateway

import (
	"context"
	"slices"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Process children of a gateway pid snapshotted before it is signalled
type GatewayChild = gatewayChild

// Wait the attach path gives an owner's control channel before deciding
const AttachChannelWait = 5 * time.Second

// Whether pid names a live process through the handle based check on Windows
func PidExists(pid int) bool { return statusPidExists(pid) }

// Live command line of pid, false when it cannot be read
func ReadProcessCmdline(pid int) (string, bool) { return statusReadProcessCmdline(pid) }

// Path of this home's gateway pid file
func PidPath() string { return pidPath() }

// Parsed pid record at p, nil when missing or unreadable
func ReadPidRecord(p string) *pyjson.Object { return readPidRecord(p) }

// Whether a pid record carries the gateway argv shape
func RecordLooksLikeGateway(record *pyjson.Object) bool { return recordLooksLikeGateway(record) }

// Pid the record names under its pid key
func PidFromRecord(record *pyjson.Object) (int, bool) { return pidFromRecord(record, "pid") }

// Home this process resolves as its own
func ProcessHome() string { return processHermesHome() }

// Children of pid captured before a takeover signals it
func SnapshotGatewayChildren(pid int) []GatewayChild { return statusSnapshotGatewayChildren(pid) }

// Whether the owner home belongs to another tenant root than ours
func LaunchedByOtherTenant(ownerHome, ourHome string) (bool, error) {
	return launchedByOtherTenant(ownerHome, ourHome)
}

// Whether a live default multiplexer already serves the current named profile
func CurrentProfileServedByRunningMultiplexer(ctx context.Context) (bool, error) {
	return namedProfileServedByRunningMultiplexer(ctx, nil)
}

// Path of the planned stop marker of this home
func PlannedStopMarkerPath() string { return plannedStopMarkerPath() }

// Python equality of two decoded JSON values
func PyValuesEqual(a, b any) bool { return pyEq(a, b) }

// Whether v is a Python int that is not a bool
func PyIsInt(v any) bool { return pyIsInt(v) }

// Freezes the served profile homes boot reserved, the _runtime_profile_homes the config carries
func (c *GatewayConfig) SetRuntimeProfileHomes(homes []profiles.ServedProfile) {
	frozen := slices.Clone(homes)
	c.runtimeProfileHomes = &frozen
}

// Served profile homes boot reserved, false while discovery is not frozen
func (c *GatewayConfig) RuntimeProfileHomes() ([]profiles.ServedProfile, bool) {
	if c == nil || c.runtimeProfileHomes == nil {
		return nil, false
	}
	return slices.Clone(*c.runtimeProfileHomes), true
}

// Scope following handle cache the store opens its state.db through, nil before the store opened one
func (s *SessionStore) DBHandleCache() *RecoverableHandleCache {
	if s == nil {
		return nil
	}
	return s.dbHandleCache
}
