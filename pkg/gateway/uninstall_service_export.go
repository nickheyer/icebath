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
