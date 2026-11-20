//go:build !icebath_core && !icebath_noextras

package plugins

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
