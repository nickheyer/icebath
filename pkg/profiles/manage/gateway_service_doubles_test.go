package manage

import (
	"context"

	"github.com/nickheyer/icebath/pkg/profiles"
)

// Installs inert gateway service and control socket doubles, since this test binary never links pkg/gatewaycli
func init() {
	s := &profiles.GatewayServiceHooks
	s.ServiceName = func(context.Context) string { return "icebath-gateway" }
	s.UserSystemdUnitDir = func(context.Context) string { return "/nonexistent-user-units" }
	s.SystemUnitDir = func() string { return "/nonexistent-system-units" }
	s.LaunchdPlistPath = func(context.Context) string { return "/nonexistent.plist" }
	s.PinnedHome = func(string) (string, bool) { return "", false }
	s.WindowsTaskName = func(context.Context) string { return "icebath" }
	s.WindowsTaskRegistered = func(context.Context) bool { return false }
	s.ExecSchtasks = func([]string) (int, string, string) { return 0, "", "" }
	s.WindowsStartupEntryPath = func(context.Context) string { return "/nonexistent-startup" }
	s.WindowsLegacyStartupEntryPath = func(context.Context) string { return "/nonexistent-legacy" }
	s.ServiceManagerDetect = func() (string, error) { return "none", nil }
	s.ServiceManagerGet = func() (profiles.ProfileServiceManager, error) { return nil, nil }
	g := &profiles.GatewayStatusHooks
	g.Identify = func(string) (any, error) { return nil, nil }
	g.MigrateProfileIdentity = func(string, string, string) (any, error) { return nil, nil }
	g.PurgeProfileIdentity = func(string, string) (any, error) { return nil, nil }
}
