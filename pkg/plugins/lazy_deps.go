package plugins

import (
	"context"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Retired dependency installer refusing every feature so an old updater claims no readiness
func Ensure(feature string, prompt bool) error {
	return &HostBuiltinError{Type: "ImportError", Message: "Dependencies are unknown to this old updater. Please relaunch Icebath."}
}

// Retired spec installer handing an updater call stack to the relaunch takeover, an ImportError otherwise
func InstallSpecs(ctx context.Context, specs []string, timeout int, constraints []any, dryRun bool) error {
	if pm.InHistoricalUpdate() {
		if err := pm.StopForRelaunch(ctx); err != nil {
			return err
		}
	}
	return &HostBuiltinError{Type: "ImportError", Message: "tools.lazy_deps.install_specs is retired; runtime dependency installation is unavailable."}
}
