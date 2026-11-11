package gateway

import "context"

// False with a warning when the installed unit pins another home, so removing it would take down another install
func SystemdUnitBelongsToCurrentHome(ctx context.Context, system bool) (bool, error) {
	return systemdUnitBelongsToCurrentHome(ctx, system)
}

// Whether schtasks answers the gateway task query with exit code zero
func WindowsIsTaskRegistered(ctx context.Context) (bool, error) { return windowsIsTaskRegistered(ctx) }

// Whether a Startup folder entry of the gateway, current or legacy, exists
func WindowsStartupEntryInstalled(ctx context.Context) (bool, error) {
	return windowsStartupEntryInstalled(ctx)
}

// Mode, host and port of a dashboard or serve command line, not ok for any other command
func ParseDashboardRuntime(command string) (string, string, int, bool) {
	return parseDashboardRuntime(command)
}

// Kind and directory of each launchd plist directory a dashboard or serve job can be loaded from
func LaunchdPlistDirs() [][2]string {
	var out [][2]string
	for _, entry := range dashboardLaunchdPlistDirsFn() {
		out = append(out, [2]string{entry.kind, entry.dir})
	}
	return out
}
