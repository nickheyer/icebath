package provider

import (
	"context"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Severity tagged structure issue lines of a config for the unknown provider hint
func configStructureIssueLines(cfg map[string]any) []string {
	return config.ConfigStructureIssueLines(config.ValidateConfigStructure(cfg, tools.RemovedBackendNote))
}

// Managed install or administrator pin refusal for writing an env key, empty when allowed
func envWriteRefusal(h, key, action string) string {
	return config.EnvWriteRefusal(h, key, action)
}

// Records every left core version in use into a first stamped config and adopts its browser env
func noteFirstStamp(h string, onDisk, toWrite map[string]any) []string {
	return config.NoteFirstStamp(h, onDisk, toWrite, config.StampHooks{
		WorkerSnapshotActive: workerConfigSnapshotActive,
		CoreVersions:         LeftCoreVersions(),
		RecordCoreUse:        RecordCoreUse,
		AdoptAgentBrowserEnv: adoptAgentBrowserEnv,
	})
}

// Moves a home's AGENT_BROWSER env settings into a config about to get its first stamp
func adoptAgentBrowserEnv(h string, cfg map[string]any) []string {
	return config.AdoptAgentBrowserEnv(h, cfg, config.BrowserEnvSources{
		HomeDotenv:   resolveHomeDotenv,
		IsLaunchHome: func(h string) bool { return home.Key(h) == home.Key(RoutingProcessHome()) },
		PMStoreRoots: func() ([]string, error) {
			store, err := pm.StoreRoot(context.Background())
			if err != nil {
				return nil, err
			}
			writable, err := pm.WritableStoreRoot(context.Background())
			if err != nil {
				return nil, err
			}
			return []string{store, writable}, nil
		},
		DeferStartupNotice: func(message string) { DeferStartupNotice(home.WithOverride(context.Background(), h), message) },
	})
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
