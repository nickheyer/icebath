package provider

import "os"

// LeftCoreFailureStamp is the path of the stamp a failed automatic plugin install leaves in a home
func LeftCoreFailureStamp(h, plugin string) string { return leftCoreFailureStamp(h, plugin) }

// NoteLeftCoreSuccess removes the failed install stamp, so a later failure starts the backoff from an hour
func NoteLeftCoreSuccess(h, plugin string) { _ = os.Remove(leftCoreFailureStamp(h, plugin)) }

// LeftCoreFirstCause is the one line of an install error a notice shows
func LeftCoreFirstCause(errText string) string { return leftCoreFirstCause(errText) }

// HomeYAMLLayers is the config.yaml of a home as the gateway loader reads it, env references expanded and the managed overlay applied
func HomeYAMLLayers(h string) (map[string]any, error) { return leftCoreYAMLLayers(h) }

// HomeRawConfig is the raw config.yaml of a home, empty when missing, unreadable or not a mapping
func HomeRawConfig(h string) map[string]any { return leftCoreReadConfig(h) }

// HomeRawConfigForWrite is the raw config.yaml at a path for a write back, empty when missing, failing on malformed YAML
func HomeRawConfigForWrite(path string) (map[string]any, error) {
	return leftCoreReadUserConfigRaw(path)
}
