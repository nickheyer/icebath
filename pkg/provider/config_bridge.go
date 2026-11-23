package provider

import (
	"context"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Severity tagged structure issue lines of a config for the unknown provider hint
func configStructureIssueLines(cfg map[string]any) []string {
	return config.ConfigStructureIssueLines(config.ValidateConfigStructure(cfg, tools.RemovedBackendNote))
}

// Managed install or administrator pin refusal for writing an env key, empty when allowed
func envWriteRefusal(ctx context.Context, h, key, action string) string {
	return config.EnvWriteRefusal(ctx, h, key, action)
}

// Selected config backend as the secret loader sees it, exiting when that backend is unavailable
func configBackendView(_ string) ConfigBackendView {
	backend, err := config.GetConfigBackend(context.Background())
	if err != nil {
		panic(err)
	}
	protected := map[string]bool{}
	for _, name := range backend.ProtectedEnvNames() {
		protected[name] = true
	}
	return ConfigBackendView{Name: backend.Name(), SelectorEnv: config.BackendEnv, ProtectedEnvNames: protected}
}

// Reports whether this process serves a frozen worker config snapshot
func workerConfigSnapshotActive() bool {
	return config.CurrentSafeWorkerPolicy() != nil
}
