package gateway

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Installs the runtime status functions pkg/profiles/manage reaches through profiles.GatewayStatusHooks
func init() {
	h := &profiles.GatewayStatusHooks
	h.ProfileRunning = func(profileDir string) bool {
		return ResolveGatewayLiveness(LivenessOptions{ProfileDir: profileDir, NoCache: true,
			PidProbe: func(p string) (int, bool, error) { return GetRunningPid(p, false) }}).Running
	}
	h.ServesNamedProfile = func(name string) bool {
		served, err := namedProfileServed(name)
		if err != nil {
			panic(err.Error())
		}
		return served
	}
	h.NotifyProfilesChanged = func(name string) { NotifyMultiplexerProfilesChanged(context.Background(), name, 8*time.Second) }
	h.RecordedServedProfiles = func() ([]string, bool, error) { return RecordedServedProfiles("") }
	h.ExclusiveMaintenance = profilesExclusiveMaintenance
	h.Identify = func(root string) (any, error) {
		return controlAnswer(IdentifyGateway(root, DefaultControlClientTimeout)), nil
	}
	h.MigrateProfileIdentity = func(root, oldName, newName string) (any, error) {
		return controlAnswer(MigrateGatewayProfileIdentity(root, oldName, newName, 8*time.Second)), nil
	}
	h.PurgeProfileIdentity = func(root, name string) (any, error) {
		return controlAnswer(PurgeGatewayProfileIdentity(root, name, 8*time.Second)), nil
	}
	h.PidExists = statusPidExists
	h.ProcessStartTime = func(pid int) (int64, bool) {
		v, ok := GetProcessStartTime(pid).(int64)
		return v, ok
	}
	h.TerminatePid = func(pid int, force bool, expectedStart *int64) error {
		var expected any
		if expectedStart != nil {
			expected = *expectedStart
		}
		return TerminatePid(pid, force, expected)
	}
	h.RecordedHomeConflicts = func(record map[string]any, expectedHome string) bool {
		return RecordedGatewayHomeConflicts(objectFromMap(record), expectedHome)
	}
}

// Exclusive maintenance for profiles, an ownership conflict surfacing as profiles.GatewayOwnershipConflict
func profilesExclusiveMaintenance(dirs []string) (func(), error) {
	_, release, err := ExclusiveMaintenance(context.Background(), dirs, "")
	var conflict *OwnershipConflict
	if errors.As(err, &conflict) {
		return nil, &profiles.GatewayOwnershipConflict{Msg: conflict.Msg}
	}
	return release, err
}

// Ordered object of a Go map, keys sorted since a Go map carries no insertion order
func objectFromMap(m map[string]any) *pyjson.Object {
	if m == nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	o := pyjson.NewObject()
	for _, k := range keys {
		o.Set(k, m[k])
	}
	return o
}
