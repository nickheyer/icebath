package selfupdate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/pm"
)

const (
	StewardDesktop   = "desktop-app"
	StewardDocker    = "docker"
	StewardNix       = "nix"
	StewardAptTermux = "apt-termux"
	StewardHomebrew  = "homebrew"
	StewardScoop     = "scoop"
	StewardWinGet    = "winget"
	StewardDpkg      = "dpkg"
	StewardRPM       = "rpm"
	StewardPacman    = "pacman"
	StewardApk       = "apk"
)

var StewardUpdateMessages = map[string]string{
	StewardDesktop: "✗ This icebath runs from inside the desktop app bundle.\n" +
		"\n" +
		"Manage updates from within the desktop app.\n" +
		"Prefer a self-managed source install? See:\n" +
		"  https://hermes-agent.nousresearch.com/docs/user-guide/switching-to-source",
	StewardNix: "✗ This icebath runs from the Nix store.\n" +
		"\n" +
		"The store path is immutable. Update through your flake:\n" +
		"  nix flake update && rebuild your profile or system",
	StewardAptTermux: "✗ This icebath runs from a Termux APT package.\n" +
		"\n" +
		"The package manager owns the code tree. Update with:\n" +
		"  pkg upgrade icebath-agent",
	StewardHomebrew: "✗ This icebath was installed by Homebrew.\n" +
		"\n" +
		"The package manager owns the binary. Update with:\n" +
		"  brew upgrade --cask icebath",
	StewardScoop: "✗ This icebath was installed by Scoop.\n" +
		"\n" +
		"The package manager owns the binary. Update with:\n" +
		"  scoop update icebath",
	StewardWinGet: "✗ This icebath was installed by WinGet.\n" +
		"\n" +
		"The package manager owns the binary. Update with:\n" +
		"  winget upgrade --id nickheyer.icebath",
	StewardDpkg: "✗ This icebath was installed by a Debian package.\n" +
		"\n" +
		"The package manager owns the binary. Update with:\n" +
		"  sudo apt update && sudo apt install --only-upgrade icebath",
	StewardRPM: "✗ This icebath was installed by an RPM package.\n" +
		"\n" +
		"The package manager owns the binary. Update with:\n" +
		"  sudo dnf upgrade icebath",
	StewardPacman: "✗ This icebath was installed by an Arch package.\n" +
		"\n" +
		"The package manager owns the binary. Update through your AUR helper:\n" +
		"  paru -S icebath-bin",
	StewardApk: "✗ This icebath was installed by an Alpine package.\n" +
		"\n" +
		"The package manager owns the binary. Update with:\n" +
		"  apk upgrade icebath",
}

const SourceOnTermuxUpdateMessage = "✗ This icebath is a source checkout running under Termux.\n" +
	"\n" +
	"Source installs are not supported on Termux — `icebath update` would\n" +
	"build Python packages on the device. Switch to the APT package:\n" +
	"  pkg install icebath-agent"

const SourceOnTermuxUpdateCommand = "pkg install icebath-agent"

const stewardUpdateFallback = "✗ This icebath install is managed by {steward}.\n" +
	"\n" +
	"The tree has no git checkout, so `icebath update` cannot update it.\n" +
	"Update it with the tool that installed it."

const (
	stewardDeleteDataPreamble = "To delete your icebath data (chats, configuration, etc),\n"
	stewardDeleteDataCLI      = "run:\n$ icebath uninstall --data\n"
	stewardDeleteDataDesktop  = "Open icebath Desktop, go to Settings -> About, and delete your data from there.\n"
	stewardManagedByDesktop   = "✗ icebath is managed by the desktop app.\n"
)

var stewardUninstallMessages = map[string]string{
	StewardDocker: "✗ This icebath runs from a Docker image.\n" +
		"\n" +
		"There is no code to uninstall — remove the container and image:\n" +
		"  docker rm <container> && docker rmi nousresearch/icebath-agent\n" +
		"\n" +
		stewardDeleteDataPreamble +
		stewardDeleteDataCLI,
	StewardAptTermux: "✗ This icebath was installed by a Termux APT package.\n" +
		"\n" +
		"The package manager owns the code tree — uninstall it with:\n" +
		"  pkg uninstall icebath-agent\n" +
		"\n" +
		stewardDeleteDataPreamble +
		stewardDeleteDataCLI,
	StewardNix: "✗ This icebath was installed by Nix.\n" +
		"\n" +
		"The store path is immutable — uninstall it the same way you\n" +
		"installed it: remove icebath-agent from your flake / profile\n" +
		"(e.g. `nix profile remove`), then rebuild.\n" +
		"\n" +
		stewardDeleteDataPreamble +
		stewardDeleteDataCLI,
}

var stewardDesktopUninstallByPlatform = map[string]string{
	"win32": stewardManagedByDesktop +
		"\n" +
		"Remove the app from Windows Settings → Apps → Installed apps.\n" +
		stewardDeleteDataPreamble +
		stewardDeleteDataDesktop,
	"darwin": stewardManagedByDesktop +
		"\n" +
		"Quit the app and drag icebath.app from Applications to the Trash.\n" +
		stewardDeleteDataPreamble +
		stewardDeleteDataDesktop,
}

const stewardDesktopUninstallDefault = stewardManagedByDesktop +
	"\n" +
	"Delete the icebath AppImage (or app directory) from wherever you\n" +
	"saved it.\n" +
	stewardDeleteDataPreamble +
	stewardDeleteDataDesktop

const stewardUninstallFallback = "✗ icebath is managed by {steward}.\n" +
	"\n" +
	"The tree has no git checkout, so the uninstaller will not remove it.\n" +
	"Remove it with the tool that installed it.\n" +
	"\n" +
	stewardDeleteDataPreamble +
	stewardDeleteDataCLI

// The icebath update refusal text for a sealed tree
func StewardUpdateMessage(steward string) string {
	if message, ok := StewardUpdateMessages[steward]; ok {
		return message
	}
	return strings.ReplaceAll(stewardUpdateFallback, "{steward}", steward)
}

// Python sys.platform spelling of the running OS
func sysPlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "android":
		return "linux"
	}
	return runtime.GOOS
}

// The uninstall refusal text for a sealed tree, platform nil meaning the running one
func StewardUninstallMessage(steward string, platform *string) string {
	if steward == StewardDesktop {
		key := sysPlatform()
		if platform != nil {
			key = *platform
		}
		if message, ok := stewardDesktopUninstallByPlatform[key]; ok {
			return message
		}
		return stewardDesktopUninstallDefault
	}
	if message, ok := stewardUninstallMessages[steward]; ok {
		return message
	}
	return strings.ReplaceAll(stewardUninstallFallback, "{steward}", steward)
}

// Package manager whose install layout the binary path sits in, empty for none
func packageManagerPath(binary string) string {
	slashed := filepath.ToSlash(binary)
	parts := strings.Split(slashed, "/")
	for i, part := range parts {
		lower := strings.ToLower(part)
		next := ""
		if i+1 < len(parts) {
			next = strings.ToLower(parts[i+1])
		}
		switch {
		case lower == "nix" && next == "store" && i <= 1:
			return StewardNix
		case lower == "cellar" || lower == "caskroom":
			return StewardHomebrew
		case lower == "winget" && next == "packages":
			return StewardWinGet
		case lower == "apps" && next == "icebath" && i > 0 && strings.EqualFold(parts[i-1], "scoop"):
			return StewardScoop
		}
	}
	for _, env := range []string{"SCOOP", "SCOOP_GLOBAL"} {
		if root := os.Getenv(env); root != "" {
			apps := filepath.ToSlash(filepath.Join(root, "apps")) + "/"
			if strings.HasPrefix(strings.ToLower(slashed), strings.ToLower(apps)) {
				return StewardScoop
			}
		}
	}
	return ""
}

// System package database query answering whether a package owns the binary, swapped by tests
var packageOwner = func(binary string) string {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return ""
	}
	for _, query := range []struct {
		steward string
		argv    []string
		owned   func(string) bool
	}{
		{StewardDpkg, []string{"dpkg-query", "-S", binary}, nil},
		{StewardRPM, []string{"rpm", "-qf", binary}, nil},
		{StewardPacman, []string{"pacman", "-Qo", binary}, nil},
		{StewardApk, []string{"apk", "info", "--who-owns", binary}, func(out string) bool { return strings.Contains(out, " is owned by ") }},
	} {
		if _, err := exec.LookPath(query.argv[0]); err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out, err := exec.CommandContext(ctx, query.argv[0], query.argv[1:]...).Output()
		cancel()
		if err == nil && (query.owned == nil || query.owned(string(out))) {
			return query.steward
		}
	}
	return ""
}

// Binary path of the install at a tree
func installBinary(projectRoot string) string {
	name := "icebath"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(projectRoot, name)
}

// Steward owning the sealed tree, nil for a git checkout and unknown when nothing proves ownership
func SealedSteward(projectRoot string) *string {
	if _, err := os.Stat(filepath.Join(projectRoot, ".git")); err == nil {
		return nil
	}
	steward := "unknown"
	stamp := pm.ReadInstallStamp(projectRoot)
	if distribution, ok := stamp.Vals["distribution"].(string); ok && distribution != "" {
		steward = distribution
	} else {
		binary := installBinary(projectRoot)
		if resolved, err := filepath.EvalSymlinks(binary); err == nil {
			binary = resolved
		}
		if manager := packageManagerPath(binary); manager != "" {
			steward = manager
		} else if manager := packageOwner(binary); manager != "" {
			steward = manager
		}
	}
	return &steward
}

// Steward and whether code removal is allowed, a git checkout reporting git and true
func ClassifyInstall(projectRoot string) (string, bool) {
	steward := SealedSteward(projectRoot)
	if steward == nil {
		return "git", true
	}
	return *steward, false
}
