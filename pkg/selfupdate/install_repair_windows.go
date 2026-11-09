//go:build windows

package selfupdate

import (
	"os"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// Entries of the registry user PATH with variables expanded, the process PATH when the registry cannot be read
func windowsUserPathEntries() []string {
	value := ""
	key, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.QUERY_VALUE)
	if err == nil {
		raw, _, rerr := key.GetStringValue("Path")
		key.Close()
		if rerr == nil {
			value, _ = registry.ExpandString(raw)
		} else {
			value = os.Getenv("PATH")
		}
	} else {
		value = os.Getenv("PATH")
	}
	out := []string{}
	for _, entry := range strings.Split(value, ";") {
		if strings.TrimSpace(entry) != "" {
			out = append(out, entry)
		}
	}
	return out
}

const (
	pendingRenameKey   = `SYSTEM\CurrentControlSet\Control\Session Manager`
	pendingRenameValue = "PendingFileRenameOperations"
)

// Drops reboot renames older versions queued for this install's shims, the count dropped, zero without elevation
func cleanupPendingShimRenames(scriptsDir string) int {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, pendingRenameKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return 0
	}
	defer key.Close()
	entries, kind, err := key.GetStringsValue(pendingRenameValue)
	if err != nil || kind != registry.MULTI_SZ {
		return 0
	}
	kept, removed := filterPendingShimRenames(entries, icebathExeShims(scriptsDir))
	if removed == 0 {
		return 0
	}
	if len(kept) > 0 {
		if err := key.SetStringsValue(pendingRenameValue, kept); err != nil {
			return 0
		}
		return removed
	}
	if err := key.DeleteValue(pendingRenameValue); err != nil {
		return 0
	}
	return removed
}
