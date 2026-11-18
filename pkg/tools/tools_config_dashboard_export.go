package tools

import (
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

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

// Provider category of a toolset as the category table holds it, nil for a toolset without an entry
func ToolCategory(tsKey string) *pyjson.Object { return catalogCategory(tsKey) }

// Display label of a platform, the default for a key outside the platform table
func PlatformLabel(key, def string) string {
	if info, ok := PlatformInfoFor(key); ok {
		return info.Label
	}
	return def
}
