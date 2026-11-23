package tools

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/settings"
)

// SandboxImageSwitch is a default image change the persisted docker sandbox still has to decide
type SandboxImageSwitch struct {
	CurrentImage string
	TargetImage  string
	Containers   []string
}

// Cached terminal environment eviction run after an approved switch, replaced by tests recording the call
var sandboxSwitchEvict = evictEnvironmentForTask

// Env whose home is the env home or the context home when the env names none
func sandboxSwitchEnv(ctx context.Context, env *Env) *Env {
	if env != nil && env.Home != "" {
		return env
	}
	scoped := &Env{}
	if env != nil {
		*scoped = *env
	}
	scoped.Home = home.Dir(ctx)
	return scoped
}

// Pending switch of the docker backend running an unpinned default with a labeled container on another image
func PendingSandboxImageSwitch(ctx context.Context, env *Env) (*SandboxImageSwitch, error) {
	env = sandboxSwitchEnv(ctx, env)
	ctx = home.WithOverride(ctx, env.Home)
	if settings.WhereCommandsRun.Get(ctx) != "docker" || GetTerminalBackend("docker") == nil {
		return nil, nil
	}
	if sandboxImagePinned(ctx, env) {
		return nil, nil
	}
	target := settings.ContainerImage.Get(ctx)
	docker := FindDocker(env)
	if docker == "" || target == "" {
		return nil, nil
	}
	identity := dockerContainerIdentity(env, settings.TerminalDockerSharedContainerKey.Get(ctx))
	listing := dockerQuery(ctx, env, []string{docker, "ps", "-a", "--filter", "label=icebath-agent=1",
		"--filter", "label=icebath-profile=" + identity, "--format", "{{.Names}}\t{{.Image}}"},
		10*time.Second, "sandbox image switch probe failed", "docker ps exited")
	if listing == nil {
		return nil, nil
	}
	stale := map[string][]string{}
	var order []string
	for _, line := range pystr.Splitlines(listing.Stdout, false) {
		name, image, _ := strings.Cut(line, "\t")
		if image != "" && image != target {
			if _, seen := stale[image]; !seen {
				order = append(order, image)
			}
			stale[image] = append(stale[image], name)
		}
	}
	if len(order) == 0 {
		return nil, nil
	}
	current := order[0]
	for _, img := range order[1:] {
		if len(stale[img]) > len(stale[current]) {
			current = img
		}
	}
	var containers []string
	for _, img := range order {
		containers = append(containers, stale[img]...)
	}
	sort.Strings(containers)
	return &SandboxImageSwitch{CurrentImage: current, TargetImage: target, Containers: containers}, nil
}

// Runtime pin verdict, an image the user or an administrator set first, then the bound terminal scope
func sandboxImagePinned(ctx context.Context, env *Env) bool {
	if v, err := settings.Resolve(ctx, settings.ContainerImage.Key); err == nil && v.Source != settings.SourceDefault {
		return true
	}
	scope := GetTerminalScope(ctx)
	if scope == nil && env != nil {
		scope = env.TerminalScope
	}
	return scope != nil && scope.Policy["TERMINAL_DOCKER_IMAGE_PINNED"] == "1"
}

// Pins the target or the current image and on approval evicts the cached sandbox
func DecideSandboxImageSwitch(ctx context.Context, env *Env, sw *SandboxImageSwitch, approve bool) (string, error) {
	env = sandboxSwitchEnv(ctx, env)
	ctx = home.WithOverride(ctx, env.Home)
	image := sw.CurrentImage
	if approve {
		image = sw.TargetImage
	}
	if _, err := settings.Set(ctx, settings.ContainerImage.Key, image, settings.Global); err != nil {
		return "", err
	}
	if approve {
		if err := sandboxSwitchEvict(env, ""); err != nil {
			return "", err
		}
	}
	return image, nil
}
