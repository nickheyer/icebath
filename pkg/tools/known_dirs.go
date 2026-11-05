package tools

import "runtime"

// Gateway service names icebath writes, and the hermes spellings an older install wrote
const (
	gatewayTaskNameDefault       = "Icebath_Gateway"
	legacyGatewayTaskNameDefault = "Hermes_Gateway"
	gatewayLaunchdLabel          = "ai.icebath.gateway"
	legacyGatewayLaunchdLabel    = "ai.hermes.gateway"
	gatewaySystemdUnitPrefix     = "icebath-gateway"
	legacySystemdUnitPrefix      = "hermes-gateway"
)

// Files recording that this OS user used icebath, the first one written and every one read
var machineMarkerFiles = func() []string {
	switch runtime.GOOS {
	case "windows":
		return []string{"%LOCALAPPDATA%/Nous Research/.machine",
			"%LOCALAPPDATA%/Packages/NousResearch*_*/LocalCache/Local/Nous Research/.machine"}
	case "darwin":
		return []string{"~/Library/Application Support/Nous Research/.machine"}
	}
	return []string{"$XDG_STATE_HOME/nous-research/.machine", "~/.local/state/nous-research/.machine"}
}

// Every default place a data root can sit, icebath roots first and older hermes roots after each
var icebathHomeGlobs = func() []string {
	if runtime.GOOS == "windows" {
		return []string{"%LOCALAPPDATA%/icebath*", "%LOCALAPPDATA%/hermes*",
			"%USERPROFILE%/.icebath", "%USERPROFILE%/.hermes",
			"%LOCALAPPDATA%/Packages/NousResearch*_*/LocalCache/Local/icebath*",
			"%LOCALAPPDATA%/Packages/NousResearch*_*/LocalCache/Local/hermes*"}
	}
	return []string{"~/.icebath*", "~/.hermes*"}
}

// Gateway service definitions of this OS user, each naming the home it serves
var gatewayServiceDefinitionGlobs = func() []string {
	switch runtime.GOOS {
	case "windows":
		startup := "%APPDATA%/Microsoft/Windows/Start Menu/Programs/Startup/"
		return []string{startup + gatewayTaskNameDefault + "*", startup + legacyGatewayTaskNameDefault + "*"}
	case "darwin":
		return []string{"~/Library/LaunchAgents/" + gatewayLaunchdLabel + "*.plist",
			"~/Library/LaunchAgents/" + legacyGatewayLaunchdLabel + "*.plist"}
	}
	return []string{"$XDG_CONFIG_HOME/systemd/user/" + gatewaySystemdUnitPrefix + "*.service",
		"$XDG_CONFIG_HOME/systemd/user/" + legacySystemdUnitPrefix + "*.service",
		"~/.config/systemd/user/" + gatewaySystemdUnitPrefix + "*.service",
		"~/.config/systemd/user/" + legacySystemdUnitPrefix + "*.service"}
}

// Machine wide gateway services, each running as the account its User line names or root
var systemGatewayServiceDefinitionGlobs = func() []string {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return nil
	}
	return []string{"/etc/systemd/system/" + gatewaySystemdUnitPrefix + "*.service",
		"/etc/systemd/system/" + legacySystemdUnitPrefix + "*.service"}
}

// Folders macOS guards with a privacy prompt that opening anything inside raises
var macPrivacyProtectedDirs = func() []string {
	if runtime.GOOS != "darwin" {
		return nil
	}
	return []string{"~/Documents", "~/Desktop", "~/Downloads", "~/Library/Mobile Documents"}
}

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
