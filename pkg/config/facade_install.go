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

// Container probe a home scoped docker stamp must pass, a var so tests can pin it
var runningInContainer = hostinfo.IsContainer

// Install method from the code stamp, a home stamp, the managed marker, the nix store or a git checkout
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
	if method != "" && !(method == "docker" && !runningInContainer()) {
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
	"docker": "docker pull nousresearch/icebath-agent:latest",
	"apt":    "pkg upgrade icebath-agent",
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
	"Icebath Agent runs as a published image (nousresearch/icebath-agent), not a\n" +
	"git checkout — the container has no working tree to pull into.  Update by\n" +
	"pulling a fresh image and restarting your container instead:\n" +
	"\n" +
	"  docker pull nousresearch/icebath-agent:latest\n" +
	"  # then restart whatever started the container, e.g.:\n" +
	"  docker compose up -d --force-recreate icebath-agent\n" +
	"  # or, for ad-hoc runs, exit the current container and `docker run` again\n" +
	"\n" +
	"Verify the new version after restart:\n" +
	"  docker run --rm nousresearch/icebath-agent:latest --version\n" +
	"\n" +
	"Notes:\n" +
	"  • If you pinned a specific tag (e.g. ``:v0.14.0``) the ``:latest`` tag\n" +
	"    won't move your container — pull the newer tag you actually want, or\n" +
	"    switch to ``:latest`` / ``:main`` for rolling updates.  See available\n" +
	"    tags at https://hub.docker.com/r/nousresearch/hermes-agent/tags\n" +
	"  • On a ``-desktop`` tag (the one carrying Bot Screen)?  Keep the suffix:\n" +
	"    the unsuffixed image has no Xvnc/Xfce and no sudo to add them, so\n" +
	"    pulling it stops the bots' screens from starting.\n" +
	"  • Your config and session history live under ``$ICEBATH_HOME`` (``/opt/data``\n" +
	"    in the container, typically bind-mounted from the host) and persist\n" +
	"    across image upgrades — re-pulling doesn't lose any state.\n" +
	"  • Running a fork?  Build your own image with this repo's ``Dockerfile``\n" +
	"    and replace the ``docker pull`` step with your build/push pipeline."

// User facing guidance for icebath update inside the published Docker image
func FormatDockerUpdateMessage() string { return dockerUpdateMessage }
