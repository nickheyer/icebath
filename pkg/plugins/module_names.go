package plugins

import "strings"

// Root prefix of the module names older builds gave plugins, read under the icebath spelling
const retiredModulePrefix = "hermes_"

// Module name with a retired root read as the icebath module of the same name
func currentModuleName(module string) string {
	if rest, ok := strings.CutPrefix(module, retiredModulePrefix); ok && rest != "" {
		return "icebath_" + rest
	}
	return module
}
