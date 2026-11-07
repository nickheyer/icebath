package botdesktop

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Placement values the screen resolves to
const (
	Gateway  = "gateway"
	Terminal = "terminal"
	Refused  = "refused"
)

var streamBackends = []string{"docker", "ssh"}

// Where the screen runs, the configured terminal backend and the sentence a refusal shows
type Placement struct {
	Where   string
	Backend string
	Reason  string
}

// Profile home the env stands for, the process home when it names none
func envHome(env *tools.Env) string {
	if env != nil && env.Home != "" {
		return env.Home
	}
	return home.Process()
}

// Lease key of the env, the profile home path
func ProfileKey(env *tools.Env) string { return envHome(env) }

// Context carrying the env home as the home override
func envCtx(env *tools.Env) context.Context {
	return home.WithOverride(context.Background(), envHome(env))
}

// Read only effective config of the env home
func loadConfigReadonly(env *tools.Env) (config.Tree, error) {
	if env != nil && env.Config != nil {
		return env.Config, nil
	}
	h := envHome(env)
	return config.LoadReadonly(envCtx(env), h, nil)
}

// The bot_desktop section, empty when absent or not a mapping
func botDesktopSection(env *tools.Env) (config.Tree, error) {
	cfg, err := loadConfigReadonly(env)
	if err != nil {
		return nil, err
	}
	return cfg.Sub("bot_desktop"), nil
}

// Configured bot_desktop.placement lowered, auto for anything unknown
func placementSetting(env *tools.Env) (string, error) {
	cfg, err := botDesktopSection(env)
	if err != nil {
		return "", err
	}
	value := "auto"
	if raw := cfg["placement"]; pyrepr.Truthy(raw) {
		value = strings.ToLower(strings.TrimSpace(pyrepr.Str(raw)))
	}
	if value == "auto" || value == Terminal || value == Gateway {
		return value, nil
	}
	return "auto", nil
}

// Configured terminal backend, local when unset
func terminalBackend(env *tools.Env) (string, error) {
	cfg, err := tools.GetEnvConfig(env)
	if err != nil {
		return "", err
	}
	if raw := cfg["env_type"]; pyrepr.Truthy(raw) {
		return pyrepr.Str(raw), nil
	}
	return "local", nil
}

// True when a plugin registered backend declares stream support
func pluginStreams(backend string) bool {
	if backend == "" || slices.Contains(tools.BuiltinTerminalBackends(), backend) {
		return false
	}
	p := tools.GetTerminalBackend(backend)
	return p != nil && p.SupportsStreams()
}

// True when plugins run in the plugin host process
func pluginHostIsolated(env *tools.Env) bool {
	cfg, err := loadConfigReadonly(env)
	var m map[string]any
	if err == nil {
		m = map[string]any(cfg)
	}
	return plugins.IsolationMode(envCtx(env), m) == plugins.IsolationHost
}

// Gateway, terminal or refused from config and backend class alone, never starting a sandbox
func Resolve(env *tools.Env) (Placement, error) {
	backend, err := terminalBackend(env)
	if err != nil {
		return Placement{}, err
	}
	if backend == "local" {
		return Placement{Gateway, backend, "terminal.backend is local, so the terminal IS the gateway host"}, nil
	}
	setting, err := placementSetting(env)
	if err != nil {
		return Placement{}, err
	}
	if setting == Gateway {
		return Placement{Where: Gateway, Backend: backend}, nil
	}
	if slices.Contains(streamBackends, backend) {
		return Placement{Where: Terminal, Backend: backend}, nil
	}
	if pluginStreams(backend) {
		if !pluginHostIsolated(env) {
			return Placement{Where: Terminal, Backend: backend}, nil
		}
		return Placement{Refused, backend, fmt.Sprintf("terminal.backend is %s, a plugin that runs in the plugin host (plugins.isolation: host), "+
			"and the host cannot hand its sandbox's exec channel to Icebath, so the screen cannot follow it. "+
			"Set plugins.isolation: in_process, or bot_desktop.placement: gateway to run the desktop on the "+
			"gateway host.", backend)}, nil
	}
	reason := fmt.Sprintf("terminal.backend is %s, which cannot host a screen yet, and running the desktop on the "+
		"gateway host would put the agent's screen, browser and computer_use outside the sandbox you chose "+
		"for it. Set bot_desktop.placement: gateway to allow that explicitly.", backend)
	return Placement{Refused, backend, reason}, nil
}

// Seam over the terminal tool run that brings the profile sandbox up
var runTerminalTrue = func(env *tools.Env) {
	tools.TerminalTool(envCtx(env), env, tools.TerminalRequest{Command: "true"})
}

// The profile terminal environment, created through the terminal tool when asked and absent
func TerminalEnvironment(env *tools.Env, create bool) (tools.Environment, error) {
	e, err := tools.TouchTaskActivity(env, "")
	if err != nil {
		return nil, err
	}
	if e != nil || !create {
		return e, nil
	}
	runTerminalTrue(env)
	return tools.TouchTaskActivity(env, "")
}
