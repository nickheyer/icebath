package manage

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Hook of the gateway cli package, panicking with the Python source when pkg/gatewaycli is not linked
func gatewayServiceHook[F any](fn F, isNil bool, source string) F {
	if isNil {
		panic("manage: pkg/gatewaycli must install profiles.GatewayServiceHooks (" + source + ")")
	}
	return fn
}

// Systemd service name of the home in effect
func gatewayServiceName(ctx context.Context) string {
	h := profiles.GatewayServiceHooks.ServiceName
	return gatewayServiceHook(h, h == nil, "hermes_cli/gateway.py get_service_name")(ctx)
}

// User systemd unit directory under the account home
func gatewayUserSystemdUnitDir(ctx context.Context) string {
	h := profiles.GatewayServiceHooks.UserSystemdUnitDir
	return gatewayServiceHook(h, h == nil, "hermes_cli/gateway.py user_systemd_unit_dir")(ctx)
}

// Directory of system scope gateway units
func gatewaySystemUnitDir() string {
	h := profiles.GatewayServiceHooks.SystemUnitDir
	return gatewayServiceHook(h, h == nil, "hermes_cli/gateway.py get_systemd_unit_path")()
}

// LaunchAgents plist path of the home in effect
func gatewayLaunchdPlistPath(ctx context.Context) string {
	h := profiles.GatewayServiceHooks.LaunchdPlistPath
	return gatewayServiceHook(h, h == nil, "hermes_cli/gateway.py get_launchd_plist_path")(ctx)
}

// ICEBATH_HOME pinned by a systemd unit or launchd plist, false when it pins none
func gatewayServicePinnedHome(definition string) (string, bool) {
	h := profiles.GatewayServiceHooks.PinnedHome
	return gatewayServiceHook(h, h == nil, "hermes_cli/gateway_service_owner.py pinned_home")(definition)
}

// Expanded and resolved path, empty when resolution fails
func gatewayServiceResolve(p string) string {
	resolved, err := pypath.Resolve(pypath.Expanduser(p))
	if err != nil {
		return ""
	}
	return resolved
}

// Scheduled Task name of the home in effect
func gatewayWindowsTaskName(ctx context.Context) string {
	h := profiles.GatewayServiceHooks.WindowsTaskName
	return gatewayServiceHook(h, h == nil, "hermes_cli/gateway_windows.py get_task_name")(ctx)
}

// Whether schtasks knows the Scheduled Task of the home in effect
func gatewayWindowsTaskRegistered(ctx context.Context) bool {
	h := profiles.GatewayServiceHooks.WindowsTaskRegistered
	return gatewayServiceHook(h, h == nil, "hermes_cli/gateway_windows.py is_task_registered")(ctx)
}

// Exit code, stdout and stderr of schtasks under its hard timeout
func gatewayExecSchtasks(args []string) (int, string, string) {
	h := profiles.GatewayServiceHooks.ExecSchtasks
	return gatewayServiceHook(h, h == nil, "hermes_cli/gateway_windows.py _exec_schtasks")(args)
}

// Startup folder VBS launcher path of the home in effect
func gatewayWindowsStartupEntryPath(ctx context.Context) string {
	h := profiles.GatewayServiceHooks.WindowsStartupEntryPath
	return gatewayServiceHook(h, h == nil, "hermes_cli/gateway_windows.py get_startup_entry_path")(ctx)
}

// Startup folder legacy cmd entry path of the home in effect
func gatewayWindowsLegacyStartupEntryPath(ctx context.Context) string {
	h := profiles.GatewayServiceHooks.WindowsLegacyStartupEntryPath
	return gatewayServiceHook(h, h == nil, "hermes_cli/gateway_windows.py _legacy_startup_entry_path")(ctx)
}

// identify answer of the gateway serving root, nil when none answers
func gatewayIdentify(root string) (any, error) {
	h := profiles.GatewayStatusHooks.Identify
	return gatewayStatusHook(h, h == nil, "gateway/control_socket.py identify_gateway")(root)
}

// Rekey answer of the multiplexer serving root for a renamed profile, nil when none answers
func gatewayMigrateProfileIdentity(root, oldName, newName string) (any, error) {
	h := profiles.GatewayStatusHooks.MigrateProfileIdentity
	return gatewayStatusHook(h, h == nil, "gateway/control_socket.py migrate_gateway_profile_identity")(root, oldName, newName)
}

// Purge answer of the multiplexer serving root for a deleted profile, nil when none answers
func gatewayPurgeProfileIdentity(root, name string) (any, error) {
	h := profiles.GatewayStatusHooks.PurgeProfileIdentity
	return gatewayStatusHook(h, h == nil, "gateway/control_socket.py purge_gateway_profile_identity")(root, name)
}

// Service manager kind of the environment from hermes_cli/service_manager.py detect
func serviceManagerDetect() (string, error) {
	h := profiles.GatewayServiceHooks.ServiceManagerDetect
	return gatewayServiceHook(h, h == nil, "service_manager.detect")()
}

// Service manager of the environment from hermes_cli/service_manager.py get_service_manager
func serviceManagerGet() (s6Manager, error) {
	h := profiles.GatewayServiceHooks.ServiceManagerGet
	mgr, err := gatewayServiceHook(h, h == nil, "service_manager.get_service_manager")()
	if err != nil || mgr == nil {
		return nil, err
	}
	return mgr, nil
}
