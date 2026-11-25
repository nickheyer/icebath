package browser

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Base reference of the objects register_browser_provider receives from a plugin in the host
var hostedBrowserProviderBase = plugins.HostObjectBases["register_browser_provider"]

// Cloud browser provider a plugin registered, living in the plugin host and called by reference
type hostedBrowserProvider struct{ obj *plugins.HostedObject }

// Plugin host object standing in for a BrowserProvider as a provider, any other value unchanged for the wrong type warning
func adaptHostedBrowserProvider(p any) any {
	obj, ok := p.(*plugins.HostedObject)
	if !ok || obj == nil || (obj.Base != hostedBrowserProviderBase && !obj.HasMethod("create_session")) {
		return p
	}
	return hostedBrowserProvider{obj: obj}
}

// Context a provider call runs under, the profile home of the calling session bound
func (h hostedBrowserProvider) scope(env *tools.Env) context.Context {
	return tools.WithEnvHome(context.Background(), env)
}

func (h hostedBrowserProvider) Name() string {
	v, err := h.obj.Get(context.Background(), "name")
	if err != nil {
		panic(err)
	}
	return anyutil.String(v)
}

// Label the plugin declares, its name when it declares none
func (h hostedBrowserProvider) DisplayName() string {
	if v, err := h.obj.Get(context.Background(), "display_name"); err == nil {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return h.Name()
}

// Picker row the plugin declares, the plain row of its label when it declares none
func (h hostedBrowserProvider) GetSetupSchema() map[string]any {
	if h.obj.HasMethod("get_setup_schema") {
		v, err := h.obj.Call(context.Background(), "get_setup_schema", nil, nil)
		if err != nil {
			panic(err)
		}
		if schema := plugins.FieldsOf(v); schema != nil {
			return schema
		}
	}
	return map[string]any{"name": h.DisplayName(), "badge": "", "tag": "", "env_vars": []any{}}
}

// The plugin's own cheap availability answer, raising like the plugin method does
func (h hostedBrowserProvider) IsAvailable(env *tools.Env) bool {
	v, err := h.obj.Call(h.scope(env), "is_available", nil, nil)
	if err != nil {
		panic(err)
	}
	return anyutil.IsTrue(v)
}

// Session metadata the plugin answers for a task, anything but a mapping an error
func (h hostedBrowserProvider) CreateSession(env *tools.Env, taskID string) (map[string]any, error) {
	v, err := h.obj.Call(h.scope(env), "create_session", []any{taskID}, nil)
	if err != nil {
		return nil, err
	}
	session := plugins.FieldsOf(v)
	if session == nil {
		return nil, fmt.Errorf("browser provider %s create_session returned %s where a mapping was expected", h.Name(), errchain.Kind(v))
	}
	return session, nil
}

// Releases a session, a failing call logged and read as not closed so the cleanup loop keeps moving
func (h hostedBrowserProvider) CloseSession(env *tools.Env, sessionID string) bool {
	v, err := h.obj.Call(h.scope(env), "close_session", []any{sessionID}, nil)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("browser provider %s close_session failed: %v", h.Name(), err))
		return false
	}
	return anyutil.IsTrue(v)
}

// Best effort teardown at exit, a failing call logged and never raised
func (h hostedBrowserProvider) EmergencyCleanup(env *tools.Env, sessionID string) {
	if _, err := h.obj.Call(h.scope(env), "emergency_cleanup", []any{sessionID}, nil); err != nil {
		pkgLog().Debug(fmt.Sprintf("browser provider %s emergency_cleanup failed: %v", h.Name(), err))
	}
}

// Plugin facing registry that register_browser_provider writes into, a plugin host object stored as the provider adapted from it
func BrowserProviderScopedRegistry() plugins.ScopedRegistry {
	return plugins.HostedScopedRegistry{Inner: BrowserProviderRegistry.Scoped(), Adapt: adaptHostedBrowserProvider,
		Object: func(held any) (*plugins.HostedObject, bool) {
			h, ok := held.(hostedBrowserProvider)
			return h.obj, ok
		}}
}
