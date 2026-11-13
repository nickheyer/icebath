package plugins

import (
	"path/filepath"
	"strings"
)

// Plugin at root/segments named after its last segment, extra manifest lines appended
func scannerPlugin(segments []string, extra string) mv2Plugin {
	return mv2Plugin{dir: filepath.Join(segments...), manifest: "name: " + segments[len(segments)-1] + "\nversion: 0.1.0\ndescription: Test plugin " +
		strings.Join(segments, "/") + "\n" + extra, init: "def register(ctx):\n    pass\n"}
}

// Keys of the plugins a manager recorded from any source but bundled
func nonBundledKeys(m *Manager) []string {
	keys := []string{}
	for key, p := range m.plugins {
		if p.Manifest.Source != "bundled" {
			keys = append(keys, key)
		}
	}
	return keys
}
