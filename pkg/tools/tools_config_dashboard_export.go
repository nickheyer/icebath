package tools

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Tools configuration of the home in ctx, the tools_config module functions the dashboard routes call
func NewToolsSetup(ctx context.Context, cfg config.Tree) *ToolsSetup {
	return &ToolsSetup{Ctx: ctx, Env: &Env{Home: home.Dir(ctx), Config: cfg}}
}

// Whether the credentials of a toolset are configured under an entitlement state already read
func (s *ToolsSetup) ToolsetHasKeys(tsKey string, cfg config.Tree, features *NousToolFeatures) (bool, error) {
	return s.toolsetHasKeys(tsKey, cfg, false, features)
}

// Toolsets a platform resolves to without the implicit MCP servers
func (s *ToolsSetup) CurrentPlatformTools(cfg config.Tree, platform string) map[string]bool {
	return s.currentPlatformTools(cfg, platform)
}

// Writes the toolset selection of a platform into the config and saves it
func (s *ToolsSetup) SavePlatformTools(cfg config.Tree, platform string, enabled map[string]bool) error {
	return s.savePlatformToolsConfig(cfg, platform, enabled)
}

// Model ids in catalog order, their metadata and the default model of a toolset backend
func (s *ToolsSetup) ToolsetModelCatalog(tsKey, pluginName string, cfg config.Tree) (ids []string, meta map[string]map[string]any, defaultModel string) {
	catalog, def := s.toolsetModelCatalog(tsKey, pluginName, cfg)
	if catalog == nil {
		return nil, map[string]map[string]any{}, def
	}
	return catalog.ids, catalog.meta, def
}

// Catalog plugin of a provider picker row, empty when the row names none
func ResolveToolsetModelPlugin(tsKey string, row *pyjson.Object) string {
	return resolveToolsetModelPlugin(tsKey, row)
}

// Provider category of a toolset as the category table holds it, nil for a toolset without an entry
func ToolCategory(tsKey string) *pyjson.Object { return catalogCategory(tsKey) }

// Display label of a platform, the default for a key outside the platform table
func PlatformLabel(key, def string) string {
	if info, ok := PlatformInfoFor(key); ok {
		return info.Label
	}
	return def
}

// Runs a CLI child with captured pipes and closed stdin under a timeout
func RunCapture(ctx context.Context, argv []string, timeout time.Duration) (*CaptureResult, error) {
	return runCapture(ctx, argv, timeout, false, nil)
}

// Backend web_extract resolves for env
func GetExtractBackend(env *Env) string { return getExtractBackend(env) }

// Whether web_search rides the managed Nous route for env
func ManagedWebSearch(env *Env) bool { return managedWebSearch(env) }

// Replaces the fixed docker CLI locations searched after the PATH until the returned call
func SwapDockerSearchPaths(paths []string) (restore func()) {
	previous := dockerSearchPaths
	dockerSearchPaths = paths
	return func() { dockerSearchPaths = previous }
}

// Replaces the probe that tells whether the dependency environment can import a module until the returned call
func SwapPostSetupModuleImportable(fn func(ctx context.Context, moduleName string) bool) (restore func()) {
	previous := postSetupModuleImportable
	postSetupModuleImportable = fn
	return func() { postSetupModuleImportable = previous }
}
