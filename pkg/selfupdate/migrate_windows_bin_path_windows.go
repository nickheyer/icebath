//go:build windows

package selfupdate

import (
	"errors"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// Raw user PATH entries and the registry value type, expandable string when the value is absent
func readUserPathRaw() ([]string, uint32, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.QUERY_VALUE)
	if err != nil {
		return nil, 0, err
	}
	defer key.Close()
	raw, kind, err := key.GetStringValue("Path")
	if errors.Is(err, registry.ErrNotExist) {
		return []string{}, registry.EXPAND_SZ, nil
	}
	if err != nil {
		return nil, 0, err
	}
	var entries []string
	for _, entry := range strings.Split(raw, ";") {
		if entry != "" {
			entries = append(entries, entry)
		}
	}
	return entries, kind, nil
}

// Writes the user PATH back keeping its registry value type
func writeUserPathRaw(entries []string, kind uint32) error {
	key, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	value := strings.Join(entries, ";")
	if kind == registry.EXPAND_SZ {
		return key.SetExpandStringValue("Path", value)
	}
	return key.SetStringValue("Path", value)
}

// Expands percent variables the way os.path.expandvars does on Windows
func windowsExpandVars(entry string) string {
	expanded, err := registry.ExpandString(entry)
	if err != nil {
		return entry
	}
	return expanded
}
