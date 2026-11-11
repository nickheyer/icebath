package gatewayrun

import "context"

// Count of gateway service units installed for the profile home
func InstalledServiceCount(ctx context.Context, profileHome string) (int, error) {
	units, err := migrateInstalledServices(ctx, profileHome)
	return len(units), err
}
