package plugins

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
)

const contextEngineBase = "agent.context_engine:ContextEngine"

// ContextEngineInfo is one discovered context engine with its description and availability
type ContextEngineInfo struct {
	Name        string
	Description string
	Available   bool
}

// Guest engine dirs of the user plugins directory by name
func iterEngineDirs(ctx context.Context) [][2]string {
	userDir, ok := UserPluginsDir(ctx)
	if !ok {
		return nil
	}
	var dirs [][2]string
	for _, child := range GuestProviderDirs(userDir, "context_engine") {
		dirs = append(dirs, [2]string{filepath.Base(child), child})
	}
	return dirs
}

// Every user context engine with its plugin.yaml description and availability
func DiscoverContextEngines(ctx context.Context) ([]ContextEngineInfo, error) {
	dirs := iterEngineDirs(ctx)
	out := make([]ContextEngineInfo, 0, len(dirs))
	for _, d := range dirs {
		child := d[1]
		out = append(out, ContextEngineInfo{Name: d[0], Description: ReadPluginDescription(child),
			Available: ProbeAvailability(func() (any, error) { return loadEngineFromDir(ctx, child) })})
	}
	return out, nil
}

// Engine dir of a name, a user guest plugin that supplies a context engine
func FindEngineDir(ctx context.Context, name string) (string, bool) {
	for _, d := range iterEngineDirs(ctx) {
		if d[0] == name {
			return d[1], true
		}
	}
	return "", false
}

// Context engine instance by name, nil when it is not found or fails to load
func LoadContextEngine(ctx context.Context, name string) *HostedObject {
	engineDir, ok := FindEngineDir(ctx, name)
	if !ok {
		logDebug(fmt.Sprintf("Context engine '%s' not found in user plugins", name))
		return nil
	}
	instance, _ := LoadNamed(name, engineDir, func(dir string) (any, error) {
		obj, err := loadEngineFromDir(ctx, dir)
		if obj == nil {
			return nil, err
		}
		return obj, err
	}, "Context engine", "engine").(*HostedObject)
	return instance
}

// Context engine of a guest dir, its slash commands routed through the engine collector
func loadEngineFromDir(ctx context.Context, engineDir string) (*HostedObject, error) {
	m := GetPluginManager(ctx)
	return LoadGuestProvider(ctx, engineDir, "context_engine", contextEngineBase, newEngineCollectorContext(m, filepath.Base(engineDir)), true, nil)
}

// Collector of a bundled context engine forwarding its slash commands to the plugin command registry
type engineCollector struct {
	name string
}

// Plugin context whose registrations go through the engine collector of engineName
func newEngineCollectorContext(m *Manager, engineName string) *Context {
	if engineName == "" {
		engineName = "context_engine"
	}
	c := newContext(&Manifest{Name: engineName, Key: engineName}, m)
	c.engineCollector = &engineCollector{name: engineName}
	return c
}

// Answers one context call of the engine as the collector attribute of that name would
func (e *engineCollector) serve(ctx context.Context, h *PluginHost, c *Context, method string, args []any, kwargs map[string]any) (any, error) {
	switch method {
	case "register_command":
		return hostMethod(method, []pyParam{req("name"), req("handler"), opt("description", ""), opt("args_hint", "")},
			func(_ context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
				handler, err := argCallable(method, "handler", a["handler"])
				if err != nil {
					return nil, err
				}
				e.registerCommand(c.manager, a["name"], hostedCommandHandler(handler), a["description"], a["args_hint"])
				return nil, nil
			})(ctx, h, c, args, kwargs)
	case "register_tool", "register_hook", "register_cli_command", "register_memory_provider":
		return nil, nil
	}
	if !strings.HasPrefix(method, "register_") {
		return hostCtxCall(ctx, h, c, method, args, kwargs)
	}
	return nil, &HostBuiltinError{Type: "AttributeError", Message: fmt.Sprintf("a context engine plugin cannot call %s", method)}
}

// Registers an engine slash command unless its name is empty, built in or taken by a plugin
func (e *engineCollector) registerCommand(m *Manager, rawName any, handler PluginCommandHandler, description, argsHint any) {
	nameText := ""
	if config.PyTruthy(rawName) {
		nameText = config.PyStr(rawName)
	}
	clean := strings.ReplaceAll(strings.TrimLeft(pyStrip(strings.ToLower(nameText)), "/"), " ", "-")
	if clean == "" {
		logWarn(fmt.Sprintf("Context engine '%s' tried to register a command with an empty name.", e.name))
		return
	}
	conflict := "Context engine '%s' tried to register command '/%s' which %s Skipping."
	if s := installedServices(); s != nil && s.ResolveBuiltinCommand != nil {
		builtin := false
		if callGuarded(func() error { builtin = s.ResolveBuiltinCommand(clean); return nil }) == nil && builtin {
			logWarn(fmt.Sprintf(conflict, e.name, clean, "conflicts with a built-in command."))
			return
		}
	}
	desc := "Context engine command"
	if config.PyTruthy(description) {
		desc = config.PyStr(description)
	}
	hint := ""
	if config.PyTruthy(argsHint) {
		hint = pyStrip(config.PyStr(argsHint))
	}
	m.mu.Lock()
	if _, taken := m.pluginCommands[clean]; taken {
		m.mu.Unlock()
		logWarn(fmt.Sprintf(conflict, e.name, clean, "is already registered by a plugin."))
		return
	}
	m.notePluginCommandOrderLocked(clean)
	m.pluginCommands[clean] = &PluginCommand{Handler: handler, Description: desc, Plugin: "context-engine:" + e.name, ArgsHint: hint}
	m.mu.Unlock()
	logDebug(fmt.Sprintf("Context engine '%s' registered command: /%s", e.name, clean))
}
