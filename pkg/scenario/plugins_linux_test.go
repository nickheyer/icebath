package scenario

import (
	"testing"
)

// Returns whether a list under the plugins section of config.yaml holds the name
func pluginListed(s *S, list, name string) bool {
	entries, _ := Dig(s.YAML("config.yaml"), "plugins", list).([]any)
	for _, entry := range entries {
		if entry == name {
			return true
		}
	}
	return false
}

// Bundled plugins are listed, then one is enabled and disabled through the allow list of config.yaml
func TestScenarioPluginsListEnableDisable(t *testing.T) {
	s := Start(t, Setup{})
	s.Finding("SF-2")
	s.Run("plugins", "list").Exit(0).Has("Plugins", "Name", "Status", "Version", "Description", "Source", "disk-cleanup", "not enabled", "bundled")

	s.Run("plugins", "enable", "disk-cleanup").Exit(0).Has("Plugin disk-cleanup enabled.")
	if !pluginListed(s, "enabled", "disk-cleanup") || pluginListed(s, "disabled", "disk-cleanup") {
		s.Failf("config.yaml after enable holds plugins %v", Dig(s.YAML("config.yaml"), "plugins"))
	}
	s.Run("plugins", "enable", "disk-cleanup").Exit(0).Has("Plugin 'disk-cleanup' is already enabled.")
	s.Run("plugins", "list").Exit(0).Match(`disk-cleanup\s+│\s+enabled`)

	s.Run("plugins", "disable", "disk-cleanup").Exit(0).Has("Plugin disk-cleanup disabled.")
	if pluginListed(s, "enabled", "disk-cleanup") || !pluginListed(s, "disabled", "disk-cleanup") {
		s.Failf("config.yaml after disable holds plugins %v", Dig(s.YAML("config.yaml"), "plugins"))
	}
	s.Run("plugins", "disable", "disk-cleanup").Exit(0).Has("Plugin 'disk-cleanup' is already disabled.")
	s.Run("plugins", "enable", "no-such-plugin").Exit(1).Has("no-such-plugin")
	s.HasJSON(s.YAML("config.yaml"), `{"model": {"default": "fake-model"}}`)
}
