package install

import (
	"errors"

	"golang.org/x/sys/windows/registry"
)

// Registers the Apps and Features entry WinGet correlates a nested installer run with, its uninstaller the installed binary
func registerUninstallEntry(dir, version string) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, uninstallEntryKey, registry.ALL_ACCESS)
	if err != nil {
		return err
	}
	defer key.Close()
	binary := InstallBinaryPath(dir)
	uninstall := `"` + binary + `" uninstall --yes`
	for name, value := range map[string]string{
		"DisplayName": uninstallEntryName, "DisplayVersion": version, "Publisher": uninstallEntryPublisher,
		"InstallLocation": dir, "DisplayIcon": binary, "UninstallString": uninstall, "QuietUninstallString": uninstall,
		"URLInfoAbout": "https://icebath.app",
	} {
		if err := key.SetStringValue(name, value); err != nil {
			return err
		}
	}
	for _, name := range []string{"NoModify", "NoRepair"} {
		if err := key.SetDWordValue(name, 1); err != nil {
			return err
		}
	}
	return nil
}

// Deletes the Apps and Features entry, reporting whether one was there
func RemoveUninstallEntry() (bool, error) {
	err := registry.DeleteKey(registry.CURRENT_USER, uninstallEntryKey)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
