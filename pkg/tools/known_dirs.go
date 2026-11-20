package tools

import "runtime"

// Per user bin directory of pip, pipx and installer scripts
func userLocalBin() []string {
	if runtime.GOOS != "windows" {
		return []string{"~/.local/bin"}
	}
	return []string{"%USERPROFILE%/.local/bin"}
}

// System and per user macOS application folders
func macApplicationDirs() []string {
	if runtime.GOOS == "darwin" {
		return []string{"/Applications", "~/Applications"}
	}
	return nil
}

// Where flatpak links each installed app launcher by app id, system then per user
func flatpakExportDirs() []string {
	if runtime.GOOS == "linux" {
		return []string{"/var/lib/flatpak/exports/bin", "~/.local/share/flatpak/exports/bin"}
	}
	return nil
}

// Snap launcher directory on Linux
func snapBinDirs() []string {
	if runtime.GOOS == "linux" {
		return []string{"/snap/bin"}
	}
	return nil
}
