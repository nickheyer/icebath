package gatewayrun

import (
	"context"
	"regexp"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	orderedutil "github.com/nickheyer/icebath/pkg/utils/ordered"
)

// Host and container halves of a docker volume spec with its optional options
var dockerVolumeSpecRE = regexp.MustCompile(`^(?P<host>.+):(?P<container>/[^:]+?)(?::(?P<options>[^:]+))?$`)

// Container paths a host visible MEDIA output mount may use
var dockerMediaOutputContainerPaths = map[string]bool{"/output": true, "/outputs": true}

// Runner over an explicit config with the standalone opt out applied, else over the loaded runner config
func NewGatewayRunner(ctx context.Context, config *gateway.GatewayConfig) (*GatewayRunner, error) {
	r := allocGatewayRunner()
	if config == nil {
		loaded, err := loadGatewayConfigForRunner(ctx)
		if err != nil {
			return nil, err
		}
		r.config = loaded
	} else {
		r.config = config
		decision, err := gateway.StandaloneLauncherDecision(ctx, r.config)
		if err != nil {
			return nil, err
		}
		if decision != nil {
			gateway.LogMultiplexDecision(ctx, *decision)
		}
	}
	if err := r.initGatewayRunner(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// Constructor body shared by every config source, from the multiplex flag to the clocks
func (r *GatewayRunner) initGatewayRunner(ctx context.Context) error {
	provider.SetMultiplexActive(r.multiplexing())
	r.adapters = orderedutil.NewMap[gateway.Platform, gateway.PlatformAdapter]()
	r.sessionStoreOutage = nil
	r.profileAdapters = orderedutil.NewMap[string, *orderedutil.Map[gateway.Platform, gateway.PlatformAdapter]]()
	r.profileConfigs = orderedutil.NewMap[string, *gateway.GatewayConfig]()
	r.warnIfDockerMediaDeliveryIsRisky(ctx)
	gateway.SetGatewayRunnerRef(r)
	r.initRuntimeSettings(ctx)
	r.initSessionStore(ctx)
	r.initLifecycleState()
	r.initRuntimeCaches(ctx)
	if err := r.initSessionDB(ctx); err != nil {
		return err
	}
	return r.initRegistriesAndClocks(ctx)
}

// Warns when a docker backed messaging gateway has no host visible output mount for MEDIA delivery
func (r *GatewayRunner) warnIfDockerMediaDeliveryIsRisky(ctx context.Context) {
	if settings.WhereCommandsRun.Get(ctx) != "docker" {
		return
	}
	messaging := 0
	for _, p := range r.config.GetConnectedPlatforms(ctx) {
		if p != gateway.PlatformLocal && p != gateway.PlatformAPIServer && p != gateway.PlatformWebhook {
			messaging++
		}
	}
	if messaging == 0 {
		return
	}
	volumes := settings.TerminalDockerVolumes.Strings(ctx)
	container := dockerVolumeSpecRE.SubexpIndex("container")
	for _, spec := range volumes {
		if m := dockerVolumeSpecRE.FindStringSubmatch(spec); m != nil && dockerMediaOutputContainerPaths[m[container]] {
			return
		}
	}
	pkgLog().Warn("Docker backend is enabled for the messaging gateway but no explicit host-visible " +
		"output mount (for example '/home/user/.icebath/cache/documents:/output') is configured. " +
		"This is fine if the model already emits host-visible paths, but MEDIA file delivery can fail " +
		"for container-local paths like '/workspace/...' or '/output/...'.")
}

// Whether the gateway asked to exit cleanly
func (r *GatewayRunner) ShouldExitCleanly() bool { return r.exitCleanly }

// Whether the gateway asked to exit with a failure status
func (r *GatewayRunner) ShouldExitWithFailure() bool { return r.exitWithFailure }

// Reason the gateway gave for exiting, nil while it runs
func (r *GatewayRunner) ExitReason() *string { return r.exitReason }

// Exit status the gateway asked for, nil while it runs
func (r *GatewayRunner) ExitCode() *int { return r.exitCode }

// Whether a signal handler started this stop, read by adapters that tell shutdown exits from crashes
func (r *GatewayRunner) StopRequestedBySignal() bool { return r.stopRequestedBySignal }
