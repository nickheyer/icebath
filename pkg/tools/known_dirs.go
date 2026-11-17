package tools

import "runtime"

// Homebrew prefixes, present on macOS only
func homebrewDirs() []string {
	if runtime.GOOS == "darwin" {
		return []string{"/opt/homebrew/bin", "/usr/local/bin"}
	}
	return nil
}

// Per user bin directory of pip, pipx and installer scripts
func userLocalBin() []string {
	if runtime.GOOS != "windows" {
		return []string{"~/.local/bin"}
	}
	return []string{"%USERPROFILE%/.local/bin"}
}

// Cargo install directory
func rustToolDirs() []string {
	if runtime.GOOS != "windows" {
		return []string{"~/.cargo/bin"}
	}
	return []string{"%USERPROFILE%/.cargo/bin"}
}

// Global npm prefix, bun and volta bin directories
func nodeToolDirs() []string {
	if runtime.GOOS != "windows" {
		return []string{"~/.npm-global/bin", "~/.bun/bin", "~/.volta/bin"}
	}
	return []string{"%APPDATA%/npm", "%USERPROFILE%/.bun/bin", "%LOCALAPPDATA%/Volta/bin"}
}

// Directory where icebath vendors the tools it installs itself
func icebathVendoredDirs() []string {
	if runtime.GOOS != "windows" {
		return []string{"~/.icebath/bin"}
	}
	return []string{"%USERPROFILE%/.icebath/bin"}
}

// Windows per user program, scoop shim and winget link directories, empty elsewhere
func windowsUserProgramDirs() []string {
	if runtime.GOOS != "windows" {
		return nil
	}
	return []string{"%LOCALAPPDATA%/Programs", "%USERPROFILE%/scoop/shims", "%LOCALAPPDATA%/Microsoft/WinGet/Links"}
}

// Where the Visual Studio installer puts vswhere.exe, which is never on PATH
func visualStudioInstallerDirs() []string {
	if runtime.GOOS == "windows" {
		return []string{"%ProgramFiles(x86)%/Microsoft Visual Studio/Installer"}
	}
	return nil
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
