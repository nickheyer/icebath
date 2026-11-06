package mcp

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// OSV malware verdict of the package an npx, uvx or pipx launch would run
var checkPackageForMalware = func(command string, args []string) (string, error) {
	return skills.CheckPackageForMalware(command, args)
}

// Env var names some profile's external secret source applied
func secretSourceNames() []string { return provider.SecretSourceNames() }

// Timeout from timeouts.<key> over the default, clamped like every configured deadline
func resolveConfiguredTimeout(tree config.Tree, key string, def time.Duration) (time.Duration, bool) {
	return provider.ResolveTimeout(tree, key, def, true, "")
}

// Why the package manager ships no build of a package for this platform, empty when it ships one
var pmMissingReason = func(name string) (string, error) {
	pkg, err := pm.GetPackage(name)
	if err != nil {
		return "", err
	}
	target, err := pm.CurrentTarget()
	if err != nil {
		return "", err
	}
	return pkg.MissingReason(target), nil
}

// Installs a package manager package when missing, naming the remedy when policy forbids it
var pmEnsure = func(ctx context.Context, name string) error {
	_, err := pm.Ensure(ctx, name, pm.EnsureOptions{})
	return err
}

// Package manager uv or uvx executable, empty when uv is not installed
var pmUVLauncher = func(ctx context.Context, name string) (string, error) { return pm.UvLauncher(ctx, name) }

// Base env with the package manager npm and its Node composed onto PATH
var pmNodeEnv = func(base map[string]string) map[string]string { return pm.EnvFor([]string{"npm"}, base) }

// Reports whether this install is a sealed Windows payload whose tools need user facing copies
var pmUserFacingApplies = func(ctx context.Context) (bool, error) { return pm.UserFacingApplies(ctx) }

// Sealed payload dirs holding a package's executables
var pmSealedDirs = func(ctx context.Context, name string) ([]string, error) {
	return pm.SealedDirs(ctx, name)
}

// Toolchain dirs swapped for the package manager copies outside a sealed payload
var pmUserFacingCopies = func(ctx context.Context, dirs []string) ([]string, error) {
	return pm.Copies(ctx, dirs)
}

// MCP servers the enabled portable plugins of a home declare
func pluginPortableServers(homeDir string) (map[string]map[string]any, error) {
	return plugins.PortableMCPServers(homeDir)
}

// Binary and version of the package manager entry for a package, empty when it has no installed binary
var pmInstalledBinary = func(ctx context.Context, name string) (binary, version string, err error) {
	installed, err := pm.Installed(ctx, name, false)
	if err != nil || installed == nil || installed.Binary == "" {
		return "", "", err
	}
	return installed.Binary, installed.Version, nil
}

// Reports whether the task runs for a profile other than the process's own
func servesRoutedProfile(ctx context.Context) bool { return provider.ServesRoutedProfile(ctx) }

// Home the bound secret scope was stamped with, empty when none is bound
func currentSecretScopeHome(ctx context.Context) string { return provider.CurrentSecretScopeHome(ctx) }

// Installs a profile secret mapping stamped with its home on the context
func withSecretScope(ctx context.Context, secrets map[string]string, profileHome string) context.Context {
	return provider.WithSecretScope(ctx, secrets, profileHome)
}

// Secret under the context's bound profile scope mapping, bound false when no mapping is bound
func boundScopeSecret(ctx context.Context, name string) (value string, found, bound bool, err error) {
	if provider.CurrentSecretScope(ctx) == nil {
		return "", false, false, nil
	}
	value, found, err = provider.GetSecret(ctx, name)
	return value, found, true, err
}

// Hydrates a profile's external secret sources, true once every source applied
var hydrateSecretSources = func(h string) bool {
	provider.HydrateProfileSecretSources(h)
	return provider.SecretHomeApplied(h)
}

// Secret mapping built from a profile's env file, external sources and managed env
func profileSecretScope(h string) map[string]string { return provider.BuildProfileSecretScope(h) }

// Fresh secret mapping of a profile, the launch profile's over the frozen launch env and a served one's from its files
func profileSecretMapping(h string) map[string]string {
	if provider.IsProcessHome(h) {
		return provider.LaunchSecretScope(h)
	}
	return provider.BuildProfileSecretScope(h)
}

// Runs a git network verb anonymously, then with each credential the user owns for the remote host
func runGitWithCredentialFallback(ctx context.Context, argv []string, url string, env map[string]string) (int, string, error) {
	return provider.RunGitWithCredentialFallback(ctx, argv, url, env)
}

// Skill requires_apps names resolve through the platform declaration registry
func init() { skills.AppAvailability = tools.PlatformSkillAppAvailability }
