package tools

import (
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Shared metrics toggle of the menu for the external configurator tests
func (s *ToolsSetup) ConfigureSharedMetricsInteractiveForTest(cfg config.Tree) error {
	return s.configureSharedMetricsInteractive(cfg)
}

// Swaps one configurator seam for an external test, the returned func restoring it
func swapSeam[T any](seam *T, fn T) func() {
	prev := *seam
	*seam = fn
	return func() { *seam = prev }
}

// Replaces save_config for an external test
func SwapToolsConfigSaveForTest(fn func(s *ToolsSetup, cfg config.Tree) error) func() {
	return swapSeam(&toolsConfigSave, fn)
}

// Replaces the per platform checklist for an external test
func SwapPromptToolsetChecklistForTest(fn func(s *ToolsSetup, label string, enabled map[string]bool, platform string, forceFresh bool) (map[string]bool, error)) func() {
	return swapSeam(&promptToolsetChecklist, fn)
}

// Replaces the configured platform lookup for an external test
func SwapGetEnabledPlatformsForTest(fn func(s *ToolsSetup) ([]string, error)) func() {
	return swapSeam(&getEnabledPlatforms, fn)
}

// Replaces the per toolset setup for an external test
func SwapConfigureToolsetForTest(fn func(s *ToolsSetup, tsKey string, cfg config.Tree, forceFresh, reconfigure bool) error) func() {
	return swapSeam(&configureToolset, fn)
}

// Plugin image generation rows for the external picker tests
func (s *ToolsSetup) PluginImageGenProvidersForTest() []*pyjson.Object {
	return s.pluginImageGenProviders()
}

// Model catalog ids and default of a toolset backend for the external picker tests
func (s *ToolsSetup) ToolsetModelCatalogForTest(tsKey, pluginName string, cfg config.Tree) ([]string, string) {
	c, def := s.toolsetModelCatalog(tsKey, pluginName, cfg)
	return c.ids, def
}

// Catalog plugin of a picker row for the external picker tests
func ResolveToolsetModelPluginForTest(tsKey string, row *pyjson.Object) string {
	return resolveToolsetModelPlugin(tsKey, row)
}

// Rows of a tool category for the external picker tests
func CategoryProvidersForTest(category string) []*pyjson.Object {
	return catProviders(categoryOf(category))
}
