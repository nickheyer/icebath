package manage

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Hook of the gateway cli package, panicking with the hook name when pkg/gatewaycli is not linked
func gatewayServiceHook[F any](fn F, isNil bool, source string) F {
	if isNil {
		panic("manage: pkg/gatewaycli must install profiles.GatewayServiceHooks (" + source + ")")
	}
	return fn
}

// Systemd service name of the home in effect
func gatewayServiceName(ctx context.Context) string {
	h := profiles.GatewayServiceHooks.ServiceName
	return gatewayServiceHook(h, h == nil, "ServiceName")(ctx)
}

// User systemd unit directory under the account home
func gatewayUserSystemdUnitDir(ctx context.Context) string {
	h := profiles.GatewayServiceHooks.UserSystemdUnitDir
	return gatewayServiceHook(h, h == nil, "UserSystemdUnitDir")(ctx)
}

// Directory of system scope gateway units
func gatewaySystemUnitDir() string {
	h := profiles.GatewayServiceHooks.SystemUnitDir
	return gatewayServiceHook(h, h == nil, "SystemUnitDir")()
}

// LaunchAgents plist path of the home in effect
func gatewayLaunchdPlistPath(ctx context.Context) string {
	h := profiles.GatewayServiceHooks.LaunchdPlistPath
	return gatewayServiceHook(h, h == nil, "LaunchdPlistPath")(ctx)
}

// ICEBATH_HOME pinned by a systemd unit or launchd plist, false when it pins none
func gatewayServicePinnedHome(definition string) (string, bool) {
	h := profiles.GatewayServiceHooks.PinnedHome
	return gatewayServiceHook(h, h == nil, "PinnedHome")(definition)
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
	return gatewayServiceHook(h, h == nil, "WindowsTaskName")(ctx)
}

// Whether schtasks knows the Scheduled Task of the home in effect
func gatewayWindowsTaskRegistered(ctx context.Context) bool {
	h := profiles.GatewayServiceHooks.WindowsTaskRegistered
	return gatewayServiceHook(h, h == nil, "WindowsTaskRegistered")(ctx)
}

// Exit code, stdout and stderr of schtasks under its hard timeout
func gatewayExecSchtasks(args []string) (int, string, string) {
	h := profiles.GatewayServiceHooks.ExecSchtasks
	return gatewayServiceHook(h, h == nil, "ExecSchtasks")(args)
}

// Startup folder VBS launcher path of the home in effect
func gatewayWindowsStartupEntryPath(ctx context.Context) string {
	h := profiles.GatewayServiceHooks.WindowsStartupEntryPath
	return gatewayServiceHook(h, h == nil, "WindowsStartupEntryPath")(ctx)
}

// Startup folder legacy cmd entry path of the home in effect
func gatewayWindowsLegacyStartupEntryPath(ctx context.Context) string {
	h := profiles.GatewayServiceHooks.WindowsLegacyStartupEntryPath
	return gatewayServiceHook(h, h == nil, "WindowsLegacyStartupEntryPath")(ctx)
}

// identify answer of the gateway serving root, nil when none answers
func gatewayIdentify(root string) (any, error) {
	h := profiles.GatewayStatusHooks.Identify
	return gatewayStatusHook(h, h == nil, "Identify")(root)
}

// Rekey answer of the multiplexer serving root for a renamed profile, nil when none answers
func gatewayMigrateProfileIdentity(root, oldName, newName string) (any, error) {
	h := profiles.GatewayStatusHooks.MigrateProfileIdentity
	return gatewayStatusHook(h, h == nil, "MigrateProfileIdentity")(root, oldName, newName)
}

// Purge answer of the multiplexer serving root for a deleted profile, nil when none answers
func gatewayPurgeProfileIdentity(root, name string) (any, error) {
	h := profiles.GatewayStatusHooks.PurgeProfileIdentity
	return gatewayStatusHook(h, h == nil, "PurgeProfileIdentity")(root, name)
}

// Service manager kind of the environment from detect
func serviceManagerDetect() (string, error) {
	h := profiles.GatewayServiceHooks.ServiceManagerDetect
	return gatewayServiceHook(h, h == nil, "ServiceManagerDetect")()
}

// Service manager of the environment from
func serviceManagerGet() (s6Manager, error) {
	h := profiles.GatewayServiceHooks.ServiceManagerGet
	mgr, err := gatewayServiceHook(h, h == nil, "ServiceManagerGet")()
	if err != nil || mgr == nil {
		return nil, err
	}
	return mgr, nil
}
