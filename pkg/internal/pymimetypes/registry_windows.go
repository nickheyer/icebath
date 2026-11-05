//go:build windows

package pymimetypes

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

const hostWindows = true

// Adds the Content Type string of every dotted HKEY_CLASSES_ROOT subkey as a strict type
func readWindowsRegistry(d *DB) {
	names, err := registry.CLASSES_ROOT.ReadSubKeyNames(-1)
	if err != nil && len(names) == 0 {
		return
	}
	for _, name := range names {
		if strings.ContainsRune(name, 0) || !strings.HasPrefix(name, ".") {
			continue
		}
		k, err := registry.OpenKey(registry.CLASSES_ROOT, name, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		val, typ, err := k.GetStringValue("Content Type")
		k.Close()
		if err != nil || typ != registry.SZ {
			continue
		}
		d.AddType(val, name, true)
	}
}
