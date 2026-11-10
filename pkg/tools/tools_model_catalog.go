package tools

import (
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Catalog plugin of a picker row, its image or video plugin name else a managed row's backend
func resolveToolsetModelPlugin(tsKey string, row *pyjson.Object) string {
	switch tsKey {
	case "image_gen":
		if v := rowGet(row, "image_gen_plugin_name"); pyTruthy(v) {
			return pyrepr.Str(v)
		}
		return pyOrStr(rowGet(row, "imagegen_backend"))
	case "video_gen":
		return pyOrStr(rowGet(row, "video_gen_plugin_name"))
	}
	return ""
}

// Catalog and default model of a toolset backend, an in tree image backend before the plugin catalogs
func (s *ToolsSetup) toolsetModelCatalog(tsKey, pluginName string, cfg config.Tree) (*modelCatalog, string) {
	if tsKey == "image_gen" {
		if backend, ok := imagegenBackends[pluginName]; ok {
			return backend.catalogFn(s, cfg)
		}
		return s.pluginImageGenCatalog(pluginName)
	}
	return s.pluginVideoGenCatalog(pluginName)
}
