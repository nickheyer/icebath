//go:build windows

package app

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// Opens the user Environment key read write and runs edit, returning what edit removed even after a later failure
func editUserEnvironment(w io.Writer, edit func(key registry.Key, removed *[]string) error, warnLabel string) []string {
	removed := []string{}
	key, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		uninstallWarn(w, warnLabel+": "+uninstallErrText(err))
		return removed
	}
	defer key.Close()
	if err := edit(key, &removed); err != nil {
		uninstallWarn(w, warnLabel+": "+uninstallErrText(err))
	}
	return removed
}

// Strips the icebath owned entries from the user scope PATH, keeping its expandable value type
func removePathFromWindowsRegistry(w io.Writer, h string, includeManagedBin bool) []string {
	markers := icebathPathMarkers(h, includeManagedBin)
	return editUserEnvironment(w, func(key registry.Key, removed *[]string) error {
		pathValue, pathType, err := key.GetStringValue("Path")
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		kept, ours := splitUserPath(pathValue, markers)
		*removed = append(*removed, ours...)
		if len(*removed) == 0 {
			return nil
		}
		joined := strings.Join(kept, ";")
		if pathType == registry.EXPAND_SZ {
			return key.SetExpandStringValue("Path", joined)
		}
		return key.SetStringValue("Path", joined)
	}, "Could not edit User PATH in registry")
}

// Deletes ICEBATH_HOME and ICEBATH_GIT_BASH_PATH from the user scope environment
func removeIcebathEnvVarsWindows(w io.Writer) []string {
	return editUserEnvironment(w, func(key registry.Key, removed *[]string) error {
		for _, name := range []string{"ICEBATH_HOME", "ICEBATH_GIT_BASH_PATH"} {
			if _, _, err := key.GetValue(name, nil); errors.Is(err, registry.ErrNotExist) {
				continue
			} else if err != nil {
				return err
			}
			if err := key.DeleteValue(name); err != nil {
				uninstallWarn(w, fmt.Sprintf("Could not delete %s from User env: %s", name, uninstallErrText(err)))
				continue
			}
			*removed = append(*removed, name)
		}
		return nil
	}, "Could not open User Environment key")
}
