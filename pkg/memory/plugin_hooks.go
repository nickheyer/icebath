package memory

import (
	"context"
	"os"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Tool environment of a home with its read only config, for plugin facing provider lookups
func homeEnv(ctx context.Context, h string) *tools.Env {
	cfg, _ := config.LoadReadonly(ctx, h, nil)
	cwd, _ := os.Getwd()
	return &tools.Env{Home: h, Config: cfg, Cwd: cwd, Log: pkgLog()}
}

// Provider directory a name resolves to in a home, false when the loader cannot resolve it
func ProviderDirForHome(h, name string) (string, bool) {
	ctx := context.Background()
	dir := FindProviderDir(ctx, homeEnv(ctx, h), name)
	return dir, dir != ""
}

const hostedMemoryProviderBase = "agent.memory_provider:MemoryProvider"

// Registration name of a memory provider object, false for any value that is not one
func HostedProviderName(obj any) (string, bool) {
	if p, ok := obj.(Provider); ok && p != nil {
		return p.Name(), true
	}
	hosted, ok := obj.(*plugins.HostedObject)
	if !ok || hosted == nil || hosted.Base != hostedMemoryProviderBase {
		return "", false
	}
	name, err := hosted.Get(context.Background(), "name")
	if err != nil {
		return "", false
	}
	return anyutil.String(name), true
}

// Prepares a provider's dependency union for the plugin installer, the readiness status dropped
func PrepareMemoryProviderDependenciesForPlugin(ctx context.Context, env *tools.Env, name string) error {
	_, _, err := PrepareMemoryProviderDependencies(ctx, env, name)
	return err
}
