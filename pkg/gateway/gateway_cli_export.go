package gateway

import (
	"context"
	"io"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Live host gateway serving every profile through the memoized probe, nil when none answers
func HostGatewayNow() *HostGateway { return hostGateway() }

// Host gateway serving the profile, nil for the default profile or when none serves it
func HostMultiplexerServingOwner(ctx context.Context, profileName *string) *HostGateway {
	return hostMultiplexerServing(ctx, profileName)
}

// Host gateway serving the profile when the profile itself runs no gateway
func ServedByAnotherHostGateway(ctx context.Context, profileName *string) *HostGateway {
	return servedByAnotherHostGateway(ctx, profileName)
}

// Whether the running default multiplexer serves the named profile, the probe error kept
func NamedProfileServedByRunningMultiplexerCtx(ctx context.Context, profileName *string) (bool, error) {
	return namedProfileServedByRunningMultiplexer(ctx, profileName)
}

// Prints the refusal and reports true when a named profile served by the multiplexer must not run its own gateway
func NamedProfileRefusedUnderMultiplexer(ctx context.Context, w io.Writer, force bool) (bool, error) {
	return namedProfileRefusedUnderMultiplexer(ctx, w, force)
}

// Installed unit or plist kind, else windows when the thunk says so, empty when none
func InstalledServiceKindFor(ctx context.Context, windows func() (bool, error)) (string, error) {
	return installedServiceKindFor(ctx, windows)
}

// Whether any gateway service definition is installed for this home
func IsServiceInstalled(ctx context.Context) (bool, error) { return isServiceInstalled(ctx) }

// Host service manager name in the canonical predicate order, empty when none applies
func ServiceBackendName(ctx context.Context, windows bool) string {
	return serviceBackendName(ctx, windows)
}

// Runs start, stop, restart or uninstall on the named service backend
func ServiceCall(ctx context.Context, backend, verb string, system bool) error {
	return serviceCall(ctx, backend, verb, system)
}

// Endpoint verdict for the profile home, the error a bad home raises
func DiscoverGatewayEndpointChecked(ctx context.Context, profileHome string, timeout time.Duration) (ObservedEndpoint, error) {
	return discoverGatewayEndpoint(ctx, profileHome, timeout)
}

// Writes the systemd unit for this home and enables it as asked
func SystemdInstall(ctx context.Context, w io.Writer, force, system bool, runAsUser string, enableOnStartup, nonInteractive, forceUnitPath bool) error {
	return systemdInstall(ctx, w, force, system, runAsUser, enableOnStartup, nonInteractive, forceUnitPath)
}

// Writes and loads the launchd plist for this home
func LaunchdInstall(ctx context.Context, w io.Writer, force, startNow, forceUnitPath bool) error {
	return launchdInstall(ctx, w, force, startNow, forceUnitPath)
}

// Prints the refusal and reports true for a user scope unit install inside a container
func RefusesContainerUserScopeInstall(w io.Writer, system bool) bool {
	return refusesContainerUserScopeInstall(w, system)
}

// Starts the gateway as a detached background process, false when the spawn failed
func SpawnDetachedGateway(ctx context.Context) (bool, error) { return spawnDetachedGateway(ctx) }

// Waits for every gateway process to exit, force killing survivors after forceAfter seconds
func WaitForGatewayExitForcing(timeout, forceAfter float64) bool {
	return waitForGatewayExit(timeout, forceAfter, true)
}

// Waits until the api server port is free for the next gateway
func WaitForAPIServerPortFree(ctx context.Context, timeout float64) (bool, error) {
	return waitForAPIServerPortFree(ctx, timeout)
}

// Reaps unsupervised orphan gateways of this home before a restart
func ReapUnsupervisedGatewayOrphans(ctx context.Context, extraExclude map[int]bool, minAgeS float64) (bool, error) {
	return reapUnsupervisedGatewayOrphans(ctx, extraExclude, minAgeS)
}

// Pinned home of the systemd unit file
func ServiceHomeForUnit(ctx context.Context, unitPath string, system bool) (string, error) {
	return serviceHomeForUnit(ctx, unitPath, system)
}

// Removes legacy pre rename gateway units, returning the count and the removed paths
func RemoveLegacyHermesUnits(ctx context.Context, w io.Writer, interactive, dryRun bool) (int, []string, error) {
	return removeLegacyHermesUnits(ctx, w, interactive, dryRun)
}

// Prints each line of text indented by two spaces through emit
func PrintIndented(text string, emit func(string)) { printIndented(text, emit) }

// Whether a pid record names this process home's profile
func PidRecordBelongsToCurrentProfile(record *pyjson.Object) (bool, error) {
	return pidRecordBelongsToCurrentProfile(record)
}
