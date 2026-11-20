package config

import "os"

// Keys of the mapping at a key path of a YAML file in loaded dict order, nil when the file or the mapping is absent
func MappingKeyOrder(path string, keys ...string) []string {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	doc, err := SafeLoadText(src, false)
	if err != nil {
		return nil
	}
	for _, key := range keys {
		m, ok := doc.(map[string]any)
		if !ok {
			return nil
		}
		if doc, ok = m[key]; !ok {
			return nil
		}
	}
	m, ok := doc.(map[string]any)
	if !ok {
		return nil
	}
	return MappingKeys(m)
}
