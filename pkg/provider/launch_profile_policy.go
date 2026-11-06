package provider

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/tools"
)

var launchEnvSnapshot struct {
	sync.Mutex
	env map[string]string
}

// Profile listing the eager activation reads, replaced by tests that simulate an unreadable profiles directory
var launchProfilesToServe = profiles.ProfilesToServe

// Process env frozen as the launch profile's own, the first capture wins and later calls return a copy
func CaptureLaunchEnv() map[string]string {
	launchEnvSnapshot.Lock()
	defer launchEnvSnapshot.Unlock()
	if launchEnvSnapshot.env == nil {
		launchEnvSnapshot.env = processEnvMap()
	}
	out := make(map[string]string, len(launchEnvSnapshot.env))
	for k, v := range launchEnvSnapshot.env {
		out[k] = v
	}
	return out
}

func processEnvMap() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env
}

// Freezes the launch env and makes unscoped credential reads fail closed for a host serving a second home
func ActivateMultiProfileHosting() {
	CaptureLaunchEnv()
	SetMultiplexActive(true)
}

// Resolved homes this host could serve, the launch home plus every profile carrying a servability marker
func servableProfileHomes(ctx context.Context) (map[string]bool, error) {
	served, err := launchProfilesToServe(ctx, true, true, true)
	if err != nil {
		return nil, err
	}
	homes := map[string]bool{}
	for _, p := range served {
		if p.Name == "default" || home.ProfileHasServableIdentity(p.Home) {
			homes[launchResolvedPath(p.Home)] = true
		}
	}
	launch := os.Getenv(home.EnvHome)
	if launch == "" {
		launch = home.PlatformDefault()
	}
	homes[launchResolvedPath(launch)] = true
	return homes, nil
}

// Absolute path with every existing symlink resolved, the non strict Path.resolve form
func launchResolvedPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	rest := ""
	cur := abs
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// Activates multi profile hosting at host startup when more than one servable home exists, true when it activated
func ActivateMultiProfileHostingEagerly(ctx context.Context) bool {
	if IsMultiplexActive(ctx) {
		return false
	}
	homes, err := servableProfileHomes(ctx)
	if err != nil {
		slog.Warn("Could not enumerate this host's profile homes; activating multi-profile hosting fail-closed (unscoped credential reads will raise instead of borrowing the launch profile's)", "error", err)
		ActivateMultiProfileHosting()
		return true
	}
	if len(homes) < 2 {
		return false
	}
	slog.Info("Multi-profile hosting activated at startup", "homes", len(homes))
	ActivateMultiProfileHosting()
	sorted := make([]string, 0, len(homes))
	for h := range homes {
		sorted = append(sorted, h)
	}
	sort.Strings(sorted)
	enableProfileLogRouting(sorted)
	return true
}

// Launch profile env, frozen once multiplexing is active and the live process env before
func launchEnv(ctx context.Context) map[string]string {
	if IsMultiplexActive(ctx) {
		return CaptureLaunchEnv()
	}
	return processEnvMap()
}

// Frozen launch TERMINAL_ overlay for a launch profile turn's terminal scope
func LaunchTerminalEnv() map[string]string {
	out := map[string]string{}
	for k, v := range CaptureLaunchEnv() {
		if strings.HasPrefix(k, "TERMINAL_") {
			out[k] = v
		}
	}
	return out
}

// Launch profile secret mapping, its env file and external sources winning over the launch env
func LaunchSecretScope(launchHome string) map[string]string {
	return LaunchSecretScopeCtx(context.Background(), launchHome)
}

// Launch profile secret mapping with the task's multiplex semantics deciding frozen or live launch env
func LaunchSecretScopeCtx(ctx context.Context, launchHome string) map[string]string {
	scope := map[string]string{}
	for k, v := range launchEnv(ctx) {
		if !IsGlobalEnv(k) {
			scope[k] = v
		}
	}
	for k, v := range BuildProfileSecretScope(launchHome) {
		scope[k] = v
	}
	return scope
}

// Secret mapping for a body owned by the home, the launch scope for this process's own home
func OwnerSecretScope(ctx context.Context, h string) map[string]string {
	if isProcessHome(h) {
		return LaunchSecretScopeCtx(ctx, h)
	}
	return BuildProfileSecretScope(h)
}

// Marks a standalone launch profile body so a mid body fail closed flip resolves from the launch scope
func WithLaunchBody(ctx context.Context) context.Context {
	h := home.Process()
	return WithLaunchBodyScope(ctx, func() map[string]string { return LaunchSecretScopeCtx(ctx, h) })
}

// Binds the launch home override, launch secret scope and terminal policy over the frozen launch overlay
func WithLaunchProfileRuntimeScope(ctx context.Context, launchHome string) context.Context {
	ctx = home.WithOverride(ctx, launchHome)
	ctx = WithSecretScope(ctx, LaunchSecretScopeCtx(ctx, launchHome), "")
	return tools.WithProfileTerminalScope(ctx, launchHome, LaunchTerminalEnv())
}

// Launch profile runtime scope once the process multiplexes, the unchanged context before
func withLaunchProfileScopeIfMultiplexed(ctx context.Context) context.Context {
	if !IsMultiplexActive(ctx) {
		return ctx
	}
	return WithLaunchProfileRuntimeScope(ctx, home.Process())
}

// Exported form of the launch profile scope binding for the gateway and serve hosts
func WithLaunchProfileScopeIfMultiplexed(ctx context.Context) context.Context {
	return withLaunchProfileScopeIfMultiplexed(ctx)
}
