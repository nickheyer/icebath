package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Host and container halves of a docker volume spec with its optional options
var dockerVolumeSpecRE = regexp.MustCompile(`^(?P<host>.+):(?P<container>/[^:]+?)(?::(?P<options>[^:]+))?$`)

// Container paths a host visible MEDIA output mount may use
var dockerMediaOutputContainerPaths = map[string]bool{"/output": true, "/outputs": true}

// Runner over an explicit config, the launching profile's standalone opt out still applied to it
func NewGatewayRunner(ctx context.Context, config *gateway.GatewayConfig) (*GatewayRunner, error) {
	r := allocGatewayRunner()
	r.config = config
	decision, err := gateway.StandaloneLauncherDecision(ctx, r.config)
	if err != nil {
		return nil, err
	}
	if decision != nil {
		gateway.LogMultiplexDecision(ctx, *decision)
	}
	if err := r.initGatewayRunner(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// Constructor body shared by every config source, from the multiplex flag to the clocks
func (r *GatewayRunner) initGatewayRunner(ctx context.Context) error {
	provider.SetMultiplexActive(r.multiplexing())
	r.adapters = pydict.New[gateway.Platform, gateway.PlatformAdapter]()
	r.sessionDBInitError = nil
	r.profileAdapters = pydict.New[string, *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]]()
	r.profileConfigs = map[string]*gateway.GatewayConfig{}
	r.warnIfDockerMediaDeliveryIsRisky(ctx)
	gateway.SetGatewayRunnerRef(r)
	r.initRuntimeSettings(ctx)
	r.initSessionStore(ctx)
	r.initLifecycleState()
	r.initRuntimeCaches(ctx)
	r.initSessionDB(ctx)
	return r.initRegistriesAndClocks(ctx)
}

// Warns when a docker backed messaging gateway has no host visible output mount for MEDIA delivery
func (r *GatewayRunner) warnIfDockerMediaDeliveryIsRisky(ctx context.Context) {
	if strings.ToLower(strings.TrimSpace(os.Getenv("TERMINAL_ENV"))) != "docker" {
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
	var volumes []string
	if raw := strings.TrimSpace(os.Getenv("TERMINAL_DOCKER_VOLUMES")); raw != "" {
		parsed, err := pyjson.Loads(raw)
		if err != nil {
			runLog().Debug("Could not parse TERMINAL_DOCKER_VOLUMES for gateway media warning", "error", err)
		} else if list, ok := parsed.([]any); ok {
			for _, v := range list {
				if s, ok := v.(string); ok {
					volumes = append(volumes, s)
				}
			}
		}
	}
	container := dockerVolumeSpecRE.SubexpIndex("container")
	for _, spec := range volumes {
		if m := dockerVolumeSpecRE.FindStringSubmatch(spec); m != nil && dockerMediaOutputContainerPaths[m[container]] {
			return
		}
	}
	runLog().Warn("Docker backend is enabled for the messaging gateway but no explicit host-visible " +
		"output mount (for example '/home/user/.icebath/cache/documents:/output') is configured. " +
		"This is fine if the model already emits host-visible paths, but MEDIA file delivery can fail " +
		"for container-local paths like '/workspace/...' or '/output/...'.")
}

// File holding the per chat voice modes, under the process home
func (r *GatewayRunner) voiceModePath() string {
	return filepath.Join(home.Process(), "gateway_voice_mode.json")
}

// Whether the gateway asked to exit cleanly
func (r *GatewayRunner) ShouldExitCleanly() bool { return r.exitCleanly }

// Whether the gateway asked to exit with a failure status
func (r *GatewayRunner) ShouldExitWithFailure() bool { return r.exitWithFailure }

// Reason the gateway gave for exiting, nil while it runs
func (r *GatewayRunner) ExitReason() *string { return r.exitReason }

// Exit status the gateway asked for, nil while it runs
func (r *GatewayRunner) ExitCode() *int { return r.exitCode }
