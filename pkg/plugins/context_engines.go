package plugins

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

const (
	contextEngineUserNamespace = "_icebath_user_context_engine"
	contextEngineBase          = "agent.context_engine:ContextEngine"
	contextEngineCapture       = "register_context_engine"
)

// ContextEngineInfo is one discovered context engine with its description and availability
type ContextEngineInfo struct {
	Name        string
	Description string
	Available   bool
}

// Bundled context engine plugins dir under the override tree, empty when the compiled tree serves since it ships no engine
func contextEnginePluginsDir() string {
	if dir := bundledOverrideDir(); dir != "" {
		return filepath.Join(dir, "context_engine")
	}
	return ""
}

// Whether the first 8192 characters of __init__.py mention the context engine contract
func isContextEngineDir(path string) bool {
	raw, err := os.ReadFile(filepath.Join(path, "__init__.py"))
	if err != nil {
		return false
	}
	source := strings.ToValidUTF8(strings.TrimPrefix(string(raw), "\ufeff"), "�")
	if utf8.RuneCountInString(source) > 8192 {
		source = string([]rune(source)[:8192])
	}
	return strings.Contains(source, "register_context_engine") || strings.Contains(source, "ContextEngine")
}

// Bundled then user engine dirs by name, a bundled name winning a collision
func iterEngineDirs(ctx context.Context) ([][2]string, error) {
	var bundled []string
	if dir := contextEnginePluginsDir(); dir != "" {
		var err error
		if bundled, err = IterPluginDirs(dir); err != nil {
			return nil, err
		}
	}
	var dirs [][2]string
	seen := map[string]bool{}
	for _, child := range bundled {
		dirs = append(dirs, [2]string{filepath.Base(child), child})
		seen[filepath.Base(child)] = true
	}
	if userDir, ok := UserPluginsDir(ctx); ok {
		user, err := IterPluginDirs(userDir)
		if err != nil {
			return nil, err
		}
		for _, child := range user {
			if !seen[filepath.Base(child)] && isContextEngineDir(child) {
				dirs = append(dirs, [2]string{filepath.Base(child), child})
			}
		}
	}
	return dirs, nil
}

// Every bundled and user context engine with its plugin.yaml description and availability
func DiscoverContextEngines(ctx context.Context) ([]ContextEngineInfo, error) {
	dirs, err := iterEngineDirs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ContextEngineInfo, 0, len(dirs))
	for _, d := range dirs {
		child := d[1]
		out = append(out, ContextEngineInfo{Name: d[0], Description: ReadPluginDescription(child),
			Available: ProbeAvailability(func() (any, error) { return loadEngineFromDir(ctx, child) })})
	}
	return out, nil
}

// Engine dir of a name, bundled first then a user plugin dir that holds a context engine
func FindEngineDir(ctx context.Context, name string) (string, bool) {
	if dir := contextEnginePluginsDir(); dir != "" {
		if bundled := pypath.Join(dir, name); isDir(bundled) {
			return bundled, true
		}
	}
	if userDir, ok := UserPluginsDir(ctx); ok {
		if user := pypath.Join(userDir, name); isDir(user) && isContextEngineDir(user) {
			return user, true
		}
	}
	return "", false
}

// Context engine instance by name, nil when it is not found or fails to load
func LoadContextEngine(ctx context.Context, name string) *HostedObject {
	engineDir, ok := FindEngineDir(ctx, name)
	if !ok {
		logDebug(fmt.Sprintf("Context engine '%s' not found in bundled or user plugins", name))
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

// Context engine instance of a dir, a bundled engine through the in process loader and its collector
func loadEngineFromDir(ctx context.Context, engineDir string) (*HostedObject, error) {
	name := filepath.Base(engineDir)
	m := GetPluginManager(ctx)
	host, err := m.pluginHost()
	if err != nil {
		return nil, err
	}
	if filepath.Dir(engineDir) != contextEnginePluginsDir() {
		return host.LoadInstance(ctx, engineDir, contextEngineUserNamespace+"."+name, contextEngineBase, contextEngineCapture, nil, nil)
	}
	return host.loadInstance(ctx, engineDir, "plugins.context_engine."+name, contextEngineBase, contextEngineCapture,
		newEngineCollectorContext(m, name), nil, map[string]any{
			"in_process": true, "parents": []any{"plugins", "plugins.context_engine"}, "plugins_root": GetBundledPluginsDir(),
			"collector_class": "_EngineCollector", "ctx_methods": anyList(engineCollectorMethods),
		})
}

// Registrations the engine collector answers, every other attribute an AttributeError
var engineCollectorMethods = []string{"register_command", "register_tool", "register_hook", "register_cli_command", "register_memory_provider"}

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
	return nil, &HostBuiltinError{Type: "AttributeError", Message: fmt.Sprintf("'_EngineCollector' object has no attribute %s", config.PyRepr(method))}
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
