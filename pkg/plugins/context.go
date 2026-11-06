package plugins

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Facade handed to a plugin so it can register tools, hooks and commands with its manager
type Context struct {
	manifest                 *Manifest
	manager                  *Manager
	loadAbandoned            atomic.Bool
	module                   string
	facadeMu                 sync.Mutex
	memoryProvider           any
	platformActions          *PlatformActions
	llm                      *PluginLlm
	subagentLifecycle        *SubagentLifecycleService
	delegationBackends       any
	sideQuestionDenialLogged atomic.Bool
	collector                *memoryCollector
	engineCollector          *engineCollector
}

// Context bound to one manifest and the manager loading it
func newContext(m *Manifest, mgr *Manager) *Context {
	return &Context{manifest: m, manager: mgr}
}

// Effective registry key used for every namespace of the plugin
func (c *Context) PluginID() string { return c.manifest.ID() }

// Reports whether another plugin is loaded and enabled under that registry key or manifest name
func (c *Context) HasPlugin(pluginID string) bool {
	c.manager.mu.RLock()
	defer c.manager.mu.RUnlock()
	for key, loaded := range c.manager.plugins {
		if loaded.Enabled && (key == pluginID || loaded.Manifest.Name == pluginID) {
			return true
		}
	}
	return false
}

// Validated plugin relative settings path, logging a warning on rejection
func (c *Context) segments(key string) ([]string, error) {
	segments, err := PluginRelativeSegments(key)
	if err != nil {
		logWarn(fmt.Sprintf("Rejected config path %s from plugin %s", config.PyRepr(key), c.PluginID()))
	}
	return segments, err
}

// Plugin relative setting from plugins.entries.<id>.settings, falling back to the legacy config subtree
func (c *Context) GetConfig(ctx context.Context, key string, def any) (any, error) {
	segments, err := c.segments(key)
	if err != nil {
		return nil, err
	}
	entry := pluginSettingsEntry(loadConfigOrEmpty(home.Dir(ctx)), c.PluginID())
	if entry == nil {
		return def, nil
	}
	if v, ok := nestedPluginValue(entry["settings"], segments); ok {
		return v, nil
	}
	if v, ok := nestedPluginValue(entry["config"], segments); ok {
		return v, nil
	}
	return def, nil
}

// Atomically writes one value in the settings subtree of this plugin under the context profile
func (c *Context) SetConfig(ctx context.Context, key string, value any) error {
	segments, err := c.segments(key)
	if err != nil {
		return err
	}
	return SavePluginSetting(home.Dir(ctx), c.PluginID(), segments, value)
}

// Profile scoped durable JSON state of this plugin under the context profile
func (c *Context) State(ctx context.Context) *PluginState {
	return NewPluginState(home.Dir(ctx), c.PluginID(), c.manifest.DataNamespace)
}

// Records host owned cleanup for one registration of this plugin
func (c *Context) track(kind, key string, release func() error, persistent bool) *Registration {
	return c.manager.trackRegistration(c.manifest, kind, key, release, persistent)
}

// Appends a callback under a key, warning on unknown keys, and leases its removal
func trackCallback[T any](c *Context, kind, key string, entry *T, mapping map[string][]*T, valid map[string]bool) *Registration {
	if !valid[key] {
		logWarn(fmt.Sprintf("Plugin '%s' registered unknown %s '%s' (valid: %s)", c.manifest.Name, kind, key,
			strings.Join(sortedSet(valid), ", ")))
	}
	c.manager.mu.Lock()
	mapping[key] = append(mapping[key], entry)
	c.manager.mu.Unlock()
	handle := c.track(kind, key, func() error {
		removeCallback(c.manager, mapping, key, entry)
		return nil
	}, false)
	logDebug(fmt.Sprintf("Plugin %s registered %s: %s", c.manifest.Name, kind, key))
	return handle
}

// Registers a lifecycle hook callback, hold_stream declaring the reply must not stream
func (c *Context) RegisterHook(hookName string, fn HookFunc, holdStream bool) *Registration {
	return c.manager.TrackHook(c, hookName, fn, holdStream)
}

// Registers behaviour changing middleware, unknown kinds warned about but stored
func (c *Context) RegisterMiddleware(kind string, fn MiddlewareFunc) *Registration {
	_, name := callbackNames(fn)
	return c.registerMiddlewareAs(kind, fn, name)
}

// Registers middleware under the display name that attributes it
func (c *Context) registerMiddlewareAs(kind string, fn MiddlewareFunc, name string) *Registration {
	return trackCallback(c, "middleware", kind, &middlewareEntry{fn: fn, name: name}, c.manager.middleware, validMiddleware)
}
