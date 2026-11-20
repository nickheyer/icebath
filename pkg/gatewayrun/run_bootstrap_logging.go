package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Syncs bundled skills, sets up file logging and the security audit, then the verbosity stderr handler
func startGatewayConfigureLogging(ctx context.Context, verbosity *int) error {
	processHome := gateway.ProcessHome()
	bestEffort(func() (map[string]any, error) { return skills.SyncSkills(ctx, true) }, "")
	if _, err := logging.SetupLogging(ctx, logging.Options{Home: processHome, Mode: "gateway"}); err != nil {
		return err
	}
	bestEffort(func() ([]string, error) {
		raw, _ := bestEffort(func() (map[string]any, error) { return config.ReadRawConfig(ctx, processHome) }, "")
		return gateway.LogStartupSecurityWarnings(ctx, processHome, raw, false), nil
	}, "Startup security audit failed (non-fatal): %s")
	if verbosity == nil {
		return nil
	}
	level := logging.LevelDebug
	switch *verbosity {
	case 0:
		level = logging.LevelWarning
	case 1:
		level = logging.LevelInfo
	}
	return logging.AddGatewayStderrHandler(level)
}
