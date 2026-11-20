//go:build !windows

package install

import "fmt"

// Refuses an Apps and Features entry off Windows, where no WinGet install exists
func registerUninstallEntry(dir, version string) error {
	return fmt.Errorf("the WinGet install of %s registers only on Windows", dir)
}

// Removes nothing off Windows, which keeps no Apps and Features entries
func RemoveUninstallEntry() (bool, error) { return false, nil }
