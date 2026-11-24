package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	orderedutil "github.com/nickheyer/icebath/pkg/utils/ordered"
)

// Binds the Teams meeting pipeline to the Graph webhook ingress
var BindTeamsPipelineRuntime func(ctx context.Context, runner *GatewayRunner) (bool, error)

// Whether the standalone Teams pipeline plugin is enabled in the gateway config
func teamsPipelinePluginEnabled(ctx context.Context) bool {
	enabled, _ := config.CfgGet(gateway.LoadGatewayConfigMap(ctx, ""), "plugins", "enabled")
	list, ok := enabled.([]any)
	if !ok {
		return false
	}
	for _, v := range list {
		if v == "teams_pipeline" || v == "teams-pipeline" {
			return true
		}
	}
	return false
}

// Binds the Teams meeting pipeline runtime to Graph webhook ingress when that adapter and plugin are on
func (r *GatewayRunner) wireTeamsPipelineRuntime(ctx context.Context) {
	if !r.adapters.Has(gateway.PlatformMSGraphWebhook) {
		return
	}
	if !teamsPipelinePluginEnabled(ctx) {
		pkgLog().Debug("Teams pipeline plugin is disabled; skipping runtime wiring")
		return
	}
	if BindTeamsPipelineRuntime == nil {
		pkgLog().Warn("Teams pipeline runtime unavailable: the teams pipeline plugin is not built into this icebath")
		return
	}
	bound, err := func() (bound bool, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%v", rec)
			}
		}()
		return BindTeamsPipelineRuntime(ctx, r)
	}()
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("Teams pipeline runtime wiring failed: %s", err.Error()))
		return
	}
	if bound {
		pkgLog().Info("Teams pipeline runtime bound to msgraph webhook ingress")
	} else if r.teamsPipelineRuntimeError != nil {
		pkgLog().Warn(fmt.Sprintf("Teams pipeline runtime unavailable: %s", *r.teamsPipelineRuntimeError))
	}
}

// Primary profile's live adapters by platform
func (r *GatewayRunner) Adapters() *orderedutil.Map[gateway.Platform, gateway.PlatformAdapter] {
	return r.adapters
}

// Teams meeting pipeline runtime bound to the Graph webhook ingress, nil before binding
func (r *GatewayRunner) TeamsPipelineRuntime() any { return r.teamsPipelineRuntime }

// Records the bound Teams meeting pipeline runtime
func (r *GatewayRunner) SetTeamsPipelineRuntime(runtime any) { r.teamsPipelineRuntime = runtime }

// Records why the Teams meeting pipeline runtime could not be built
func (r *GatewayRunner) SetTeamsPipelineRuntimeError(message *string) {
	r.teamsPipelineRuntimeError = message
}
