package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/hostinfo"
)

// Old updater shim that reports no uv tool install so a relaunch picks the new updater
func IsUVToolInstall() bool { return false }

// Old updater shim that reports every method as supported so the old updater does no legacy work
func IsUnsupportedInstallMethod(string) bool { return false }

// Old updater shim that offers no obsolete install advice for any method
func FormatUnsupportedInstallWarning(string) string { return "" }

var nixManagedSystems = map[string]bool{"nixos": true, "home-manager": true}

// Nix store root marking an install built by nix, a var so tests point it at a temp tree
var nixStore = "/nix/store"

const nixUpdateMsg = "Update Icebath through the Nix source that installed it " +
	"(e.g. nix profile upgrade, or update your flake input and rebuild with nixos-rebuild or home-manager switch)"

// Upgrade guidance for a nix managed install, empty when no nix system manages it
func GetManagedUpdateCommand(ctx context.Context, h string) string {
	if nixManagedSystems[ManagedSystem(ctx, h)] {
		return nixUpdateMsg
	}
	return ""
}

var supportedInstallMethods = map[string]bool{
	"apt": true, "docker": true, "nix": true, "nixos": true, "home-manager": true, "git": true, "unknown": true,
	"script": true, "setup-app": true, "chocolatey": true, "pip": true, "homebrew": true, "scoop": true, "winget": true,
	"dpkg": true, "rpm": true, "pacman": true, "apk": true,
}

// Methods whose package manager owns the binary and its updates, Termux APT included
var packageManagerMethods = map[string]bool{
	"apt": true, "chocolatey": true, "pip": true, "homebrew": true, "scoop": true, "winget": true,
	"dpkg": true, "rpm": true, "pacman": true, "apk": true,
}

// Reports whether a package manager other than nix or docker owns the install and its updates
func IsPackageManagerInstallMethod(method string) bool { return packageManagerMethods[method] }

// Reports whether icebath update replaces the binary from a published release for the method
func IsReleaseBinaryInstallMethod(method string) bool {
	return method == "script" || method == "setup-app" || method == "unknown"
}

// Package manager whose install layout a binary path sits in, empty for none
func PackageManagerOfPath(binary string) string {
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
			return "nix"
		case lower == "cellar" || lower == "caskroom":
			return "homebrew"
		case lower == "winget" && next == "packages":
			return "winget"
		case lower == "apps" && next == "icebath" && i > 0 && strings.EqualFold(parts[i-1], "scoop"):
			return "scoop"
		case lower == "lib" && next == "icebath" && i > 0 && strings.EqualFold(parts[i-1], "chocolatey"):
			return "chocolatey"
		case lower == "venvs" && next == "icebath-cli" && i > 0 && strings.EqualFold(parts[i-1], "pipx"):
			return "pip"
		}
	}
	for _, env := range []string{"SCOOP", "SCOOP_GLOBAL"} {
		if root := os.Getenv(env); root != "" {
			apps := filepath.ToSlash(filepath.Join(root, "apps")) + "/"
			if strings.HasPrefix(strings.ToLower(slashed), strings.ToLower(apps)) {
				return "scoop"
			}
		}
	}
	if root := os.Getenv("ChocolateyInstall"); root != "" {
		lib := filepath.ToSlash(filepath.Join(root, "lib")) + "/"
		if strings.HasPrefix(strings.ToLower(slashed), strings.ToLower(lib)) {
			return "chocolatey"
		}
	}
	return ""
}

// Text of a file decoded as utf-8-sig, ok false on a read error, an error for bytes Python cannot decode
func readUTF8SigText(path string) (string, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false, nil
	}
	raw = trimUTF8BOM(raw)
	if err := pyUTF8DecodeError(raw); err != nil {
		return "", false, err
	}
	return string(raw), true, nil
}

// Install method a stamp file names, empty when unreadable or not a supported method
func installMethodStamp(path string) (string, error) {
	text, ok, err := readUTF8SigText(path)
	if err != nil || !ok {
		return "", err
	}
	method := strings.ToLower(strings.TrimFunc(text, isPySpace))
	if !supportedInstallMethods[method] {
		return "", nil
	}
	return method, nil
}

// Tree this process runs from, the steward install root override or the resolved executable directory
func installCodeRoot() string {
	if env := os.Getenv("ICEBATH_INSTALL_ROOT"); env != "" {
		return env
	}
	exe, err := os.Executable()
	if err != nil {
		wd, _ := os.Getwd()
		return wd
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe)
}

// Install method from the code stamp, a home stamp, the managed marker, a package manager layout or a git checkout
func DetectInstallMethod(ctx context.Context, h, projectRoot string) (string, error) {
	root := projectRoot
	if root == "" {
		root = installCodeRoot()
	}
	method, err := installMethodStamp(filepath.Join(root, ".install_method"))
	if err != nil || method != "" {
		return method, err
	}
	method, err = installMethodStamp(filepath.Join(h, ".install_method"))
	if err != nil {
		return "", err
	}
	if method != "" && !(method == "docker" && !hostinfo.IsContainer()) {
		return method, nil
	}
	if managed := ManagedSystem(ctx, h); managed != "" {
		return strings.ReplaceAll(strings.ToLower(managed), " ", "-"), nil
	}
	if resolved, err := pyResolve(root); err == nil && resolved != nixStore {
		for dir := filepath.Dir(resolved); ; dir = filepath.Dir(dir) {
			if dir == nixStore {
				return "nix", nil
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	if resolved, err := pyResolve(root); err == nil {
		if manager := PackageManagerOfPath(filepath.Join(resolved, "icebath")); manager != "" && manager != "nix" {
			return manager, nil
		}
	}
	gitPath := filepath.Join(root, ".git")
	if fi, err := os.Stat(gitPath); err == nil && fi.IsDir() {
		return "git", nil
	}
	text, ok, err := readUTF8SigText(gitPath)
	if err != nil {
		return "", err
	}
	if ok && strings.HasPrefix(strings.TrimFunc(text, isPySpace), "gitdir:") {
		return "git", nil
	}
	return "unknown", nil
}

// Reports whether nix owns the method, a nix store install or a nixos or home-manager module
func IsNixInstallMethod(method string) bool {
	return method == "nix" || nixManagedSystems[method]
}

var updateCommandByMethod = map[string]string{
	"docker":     "docker pull ghcr.io/nickheyer/icebath:latest",
	"apt":        "pkg upgrade icebath-agent",
	"chocolatey": "choco upgrade icebath",
	"pip":        "pipx upgrade icebath-cli",
	"homebrew":   "brew upgrade --cask icebath",
	"scoop":      "scoop update icebath",
	"winget":     "winget upgrade --id nickheyer.icebath",
	"dpkg":       "sudo apt update && sudo apt install --only-upgrade icebath",
	"rpm":        "sudo dnf upgrade icebath",
	"pacman":     "paru -Syu icebath-bin",
	"apk":        "apk upgrade icebath",
}

// Update command or guidance for an install method, icebath update when the method has no steward
func RecommendedUpdateCommandForMethod(method string) string {
	if IsNixInstallMethod(method) {
		return nixUpdateMsg
	}
	if cmd, ok := updateCommandByMethod[method]; ok {
		return cmd
	}
	return "icebath update"
}

// Update command for this install, the managed guidance winning over a stale code stamp
func RecommendedUpdateCommand(ctx context.Context, h string) (string, error) {
	if cmd := GetManagedUpdateCommand(ctx, h); cmd != "" {
		return cmd, nil
	}
	method, err := DetectInstallMethod(ctx, h, installCodeRoot())
	if err != nil {
		return "", err
	}
	return RecommendedUpdateCommandForMethod(method), nil
}

const dockerUpdateMessage = "✗ ``icebath update`` doesn't apply inside the Docker container.\n" +
	"\n" +
	"Icebath Agent runs as a published image (ghcr.io/nickheyer/icebath), not a\n" +
	"git checkout — the container has no working tree to pull into.  Update by\n" +
	"pulling a fresh image and restarting your container instead:\n" +
	"\n" +
	"  docker pull ghcr.io/nickheyer/icebath:latest\n" +
	"  # then restart whatever started the container, e.g.:\n" +
	"  docker compose up -d --force-recreate gateway dashboard\n" +
	"  # or, for ad-hoc runs, exit the current container and `docker run` again\n" +
	"\n" +
	"Verify the new version after restart:\n" +
	"  docker run --rm ghcr.io/nickheyer/icebath:latest --version\n" +
	"\n" +
	"Notes:\n" +
	"  • If you pinned a specific tag (e.g. ``:0.14.0``) the ``:latest`` tag\n" +
	"    won't move your container — pull the newer tag you actually want, or\n" +
	"    switch to ``:latest`` for rolling updates.  See available\n" +
	"    tags at https://github.com/nickheyer/icebath/pkgs/container/icebath\n" +
	"  • Built with ``ICEBATH_BOT_DESKTOP=1`` (the one carrying Bot Screen)?  Rebuild\n" +
	"    with the same build argument: the published image has no Xvnc/Xfce and no\n" +
	"    sudo to add them, so pulling it stops the bots' screens from starting.\n" +
	"  • Your config and session history live under ``$ICEBATH_HOME`` (``/opt/data``\n" +
	"    in the container, typically bind-mounted from the host) and persist\n" +
	"    across image upgrades — re-pulling doesn't lose any state.\n" +
	"  • Running a fork?  Build your own image with this repo's ``Dockerfile``\n" +
	"    and replace the ``docker pull`` step with your build/push pipeline."

// User facing guidance for icebath update inside the published Docker image
func FormatDockerUpdateMessage() string { return dockerUpdateMessage }
