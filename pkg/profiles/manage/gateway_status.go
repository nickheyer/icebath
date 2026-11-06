package manage

import "github.com/nickheyer/icebath/pkg/profiles"

type gatewayOwnershipConflict = profiles.GatewayOwnershipConflict

// Hook of the gateway package, panicking with the Python source when pkg/gateway is not linked
func gatewayStatusHook[F any](fn F, isNil bool, source string) F {
	if isNil {
		panic("manage: pkg/gateway must install profiles.GatewayStatusHooks (" + source + ")")
	}
	return fn
}

// Whether a gateway runs for profileDir by the uncached liveness ladder, its pid file never unlinked
func gatewayProfileRunning(profileDir string) bool {
	h := profiles.GatewayStatusHooks.ProfileRunning
	return gatewayStatusHook(h, h == nil, "gateway/status.py resolve_gateway_liveness")(profileDir)
}

// Whether the live default multiplexer already serves the named profile
func gatewayServesNamedProfile(name string) bool {
	h := profiles.GatewayStatusHooks.ServesNamedProfile
	return gatewayStatusHook(h, h == nil, "hermes_cli/gateway.py named_profile_served_by_running_multiplexer")(name)
}

// Tells the live default multiplexer that profiles changed
func gatewayNotifyProfilesChanged(name string) {
	h := profiles.GatewayStatusHooks.NotifyProfilesChanged
	gatewayStatusHook(h, h == nil, "hermes_cli/gateway_multiplex_served.py notify_multiplexer_profiles_changed")(name)
}

// served_profiles of the live default gateway, recorded false when absent
func gatewayRecordedServedProfiles() ([]string, bool, error) {
	h := profiles.GatewayStatusHooks.RecordedServedProfiles
	return gatewayStatusHook(h, h == nil, "hermes_cli/gateway_multiplex_served.py recorded_served_profiles")()
}

// served_profiles the live default gateway recorded, recorded false when the key is absent or no gateway runs
func RecordedServedProfiles() ([]string, bool, error) { return gatewayRecordedServedProfiles() }

// Exclusive maintenance reservation of dirs, a *gatewayOwnershipConflict when a gateway owns one
func gatewayExclusiveMaintenance(dirs []string) (func(), error) {
	h := profiles.GatewayStatusHooks.ExclusiveMaintenance
	return gatewayStatusHook(h, h == nil, "gateway/runtime_ownership.py exclusive_maintenance")(dirs)
}

// Liveness of pid without signalling it
func gatewayPidExists(pid int) bool {
	h := profiles.GatewayStatusHooks.PidExists
	return gatewayStatusHook(h, h == nil, "gateway/status.py _pid_exists")(pid)
}

// Start time fingerprint of pid, false when unavailable
func gatewayProcessStartTime(pid int) (int64, bool) {
	h := profiles.GatewayStatusHooks.ProcessStartTime
	return gatewayStatusHook(h, h == nil, "gateway/status.py get_process_start_time")(pid)
}

// Terminates pid with the gateway identity guard
func gatewayTerminatePid(pid int, force bool, expectedStart *int64) error {
	h := profiles.GatewayStatusHooks.TerminatePid
	return gatewayStatusHook(h, h == nil, "gateway/status.py terminate_pid")(pid, force, expectedStart)
}

// Whether a gateway record names a home other than expectedHome
func gatewayRecordedHomeConflicts(record map[string]any, expectedHome string) bool {
	h := profiles.GatewayStatusHooks.RecordedHomeConflicts
	return gatewayStatusHook(h, h == nil, "gateway/status.py recorded_gateway_home_conflicts")(record, expectedHome)
}
