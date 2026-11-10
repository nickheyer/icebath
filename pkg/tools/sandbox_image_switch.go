package tools

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
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

// Text of a config value through str, empty for a falsy value
func sandboxSwitchStr(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// Pending switch of the docker backend running an unpinned default with a labeled container on another image
func PendingSandboxImageSwitch(ctx context.Context, env *Env) (*SandboxImageSwitch, error) {
	env = sandboxSwitchEnv(ctx, env)
	ctx = home.WithOverride(ctx, env.Home)
	cfg, err := config.LoadReadonly(ctx, env.Home, config.NewSecrets(env.Home).Lookup())
	if err != nil {
		return nil, err
	}
	terminal := map[string]any{}
	if raw := cfg["terminal"]; pyrepr.Truthy(raw) {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(raw))
		}
		terminal = m
	}
	if backend, ok := terminal["backend"].(string); !ok || backend != "docker" {
		return nil, nil
	}
	if sandboxImagePinned(ctx, env, terminal) {
		return nil, nil
	}
	target := sandboxSwitchStr(terminal["docker_image"])
	docker := FindDocker(env)
	if docker == "" || target == "" {
		return nil, nil
	}
	identity := dockerContainerIdentity(env, sandboxSwitchStr(terminal["docker_shared_container_key"]))
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

// Runtime pin verdict, the file key first, then the bound terminal scope, else a differing launch env image
func sandboxImagePinned(ctx context.Context, env *Env, terminalCfg map[string]any) bool {
	if raw, err := readRawConfig(sandboxSwitchEnv(ctx, env)); err == nil {
		if t, ok := raw["terminal"].(map[string]any); ok {
			if _, pinned := t["docker_image"]; pinned {
				return true
			}
		}
	}
	scope := GetTerminalScope(ctx)
	if scope == nil && env != nil {
		scope = env.TerminalScope
	}
	if scope != nil {
		return scope.Policy["TERMINAL_DOCKER_IMAGE_PINNED"] == "1"
	}
	envImage := os.Getenv("TERMINAL_DOCKER_IMAGE")
	return envImage != "" && envImage != sandboxSwitchStr(terminalCfg["docker_image"])
}

// Pins the target or the current image, re-bridges the process env and on approval evicts the cached sandbox
func DecideSandboxImageSwitch(ctx context.Context, env *Env, sw *SandboxImageSwitch, approve bool) (string, error) {
	env = sandboxSwitchEnv(ctx, env)
	ctx = home.WithOverride(ctx, env.Home)
	image := sw.CurrentImage
	if approve {
		image = sw.TargetImage
	}
	cfg, err := config.LoadContext(ctx, env.Home, config.NewSecrets(env.Home).Lookup())
	if err != nil {
		return "", err
	}
	if _, has := cfg["terminal"]; !has {
		cfg["terminal"] = map[string]any{}
	}
	terminal, ok := cfg["terminal"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("TypeError: '%s' object does not support item assignment", pyrepr.TypeName(cfg["terminal"]))
	}
	terminal["docker_image"] = image
	if err := config.SaveConfig(ctx, env.Home, map[string]any(cfg), config.SaveOptions{PreserveKeys: [][]string{{"terminal", "docker_image"}}}); err != nil {
		return "", err
	}
	override := true
	if err := config.ApplyTerminalConfigToEnv(ctx, env.Home, config.TerminalEnvOptions{Override: &override}); err != nil {
		return "", err
	}
	if approve {
		if err := sandboxSwitchEvict(env, ""); err != nil {
			return "", err
		}
	}
	return image, nil
}

// Notice lines stating what the switch changes and what carries over
func SandboxImageSwitchExplainLines(sw *SandboxImageSwitch) []string {
	n := len(sw.Containers)
	plural := ""
	if n != 1 {
		plural = "s"
	}
	return []string{
		"Your Docker sandbox (" + strconv.Itoa(n) + " container" + plural + ") runs " + sw.CurrentImage + ".",
		"The default sandbox image is now " + sw.TargetImage + ": Python 3.13, Node 26, and a desktop",
		"so Bot Screen, computer_use and the browser run inside the sandbox instead of on this machine.",
		"Switching recreates the container on the next terminal call. Files in /root and /workspace",
		"stay (they live on this machine); packages installed with apt/pip/npm -g inside the container",
		"are reinstalled on demand; Python 3.11 virtualenvs need a rebuild.",
	}
}
