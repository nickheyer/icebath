package plugins

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/config"
)

const (
	// Object base of a live SDK client a profile's create_client returned, every nested object staying in the host
	hostedClientBase = "profile_client"
	// Result base that hands object refs back undecoded, used to reload an attribute reached object
	hostedRawRefBase = "raw_ref"
	// Profile key prefix of the profiles a provider entry point registered in the host
	entryPointProfileKey = "entrypoint:"
)

// Every profile a model provider plugin directory registers, a create_client override keeping its client in the host
func LoadProviderPluginProfiles(ctx context.Context, pluginDir, moduleName string) ([]*HostedProfile, error) {
	profiles, err := loadProviderProfiles(ctx, pluginDir, moduleName)
	if err != nil {
		return nil, err
	}
	for _, p := range profiles {
		if p.InProcess != "" {
			logWarn(fmt.Sprintf("Provider plugin %s: profile %s cannot hand its client across the plugin host (%s); "+
				"the client stays in the plugin host behind a live object proxy", filepath.Base(pluginDir), config.PyRepr(p.Name), p.InProcess))
		}
	}
	hosted := []*HostedProfile{}
	for _, p := range profiles {
		if p.InProcess == "" {
			hosted = append(hosted, p)
		}
	}
	hostedProfilesMu.Lock()
	hostedProfiles[pluginDir] = hosted
	hostedProfilesMu.Unlock()
	return profiles, nil
}

// Runs the overridden create_client in the plugin host and returns the live client it built as a hosted object
func (p *HostedProfile) CreateClient(ctx context.Context, args []any, kwargs map[string]any) (*HostedObject, error) {
	m := GetPluginManager(ctx)
	if m == nil {
		return nil, &PluginHostUnavailable{Reason: "plugins.isolation is no longer 'host'; restart Icebath"}
	}
	host, err := m.pluginHost()
	if err != nil {
		return nil, err
	}
	params := map[string]any{"path": p.PluginDir, "module_name": p.Module, "profile": p.Name,
		"args": host.encodeArgs(nonNilArgs(args)), "kwargs": host.encodeArgs(nonNilKwargs(kwargs))}
	reload := func(ctx context.Context) (map[string]any, error) {
		if _, err := host.ensureStarted(ctx); err != nil {
			return nil, err
		}
		raw, err := host.callIn(ctx, hostedRawRefBase, "profile_client", params, nil, 0, nil)
		if err != nil {
			return nil, err
		}
		ref, _ := raw.(map[string]any)
		return host.stampGeneration(ref), nil
	}
	ref, err := reload(ctx)
	if err != nil {
		return nil, err
	}
	return host.objectProxy(ref, hostedClientBase, reload), nil
}

// Base nested results of a live client resolve under, empty for every other hosted object
func (o *HostedObject) liveClientBase() string {
	if o.Base == hostedClientBase {
		return hostedClientBase
	}
	return ""
}

// Reload of an object read as an attribute of a live client, the parent reloaded first then the attribute read again
func (o *HostedObject) attributeReload(name string) func(context.Context) (map[string]any, error) {
	return func(ctx context.Context) (map[string]any, error) {
		ref, generation, err := o.addr(ctx)
		if err != nil {
			return nil, err
		}
		raw, err := o.host.callIn(ctx, hostedRawRefBase, "obj_getattr", map[string]any{"ref": ref, "name": name}, &generation, 0, nil)
		if err != nil {
			return nil, err
		}
		fresh, _ := raw.(map[string]any)
		if _, ok := fresh["__object__"]; !ok {
			return nil, &HostBuiltinError{Type: "AttributeError", Message: name}
		}
		return o.host.stampGeneration(fresh), nil
	}
}

// Entry points of the provider plugin group on the import path, the first distribution of each name winning
func ProviderEntryPoints() []PipEntryPoint {
	return pipEntryPoints(processImportPath())
}

// What loading a provider entry point did in the plugin host
type ProviderEntryPointLoad struct {
	Profiles          []*HostedProfile
	Callable          bool
	RequiresArguments bool
}

// Loads a provider entry point in the plugin host, profiles registered before a failure returned with its error
func LoadProviderEntryPoint(ctx context.Context, ep PipEntryPoint) (*ProviderEntryPointLoad, error) {
	reply, err := providerEntryPointCall(ctx, "provider_entry_point_load", ep)
	if reply == nil {
		return &ProviderEntryPointLoad{}, err
	}
	profiles, decodeErr := hostedProfilesFrom(entryPointProfileKey+ep.Name, ep.Value, reply["profiles"])
	if decodeErr != nil {
		return &ProviderEntryPointLoad{}, decodeErr
	}
	return &ProviderEntryPointLoad{Profiles: profiles, Callable: config.PyTruthy(reply["callable"]),
		RequiresArguments: config.PyTruthy(reply["requires_arguments"])}, err
}

// Calls a loaded provider entry point with no arguments in the plugin host and returns the profiles it registered
func InvokeProviderEntryPoint(ctx context.Context, ep PipEntryPoint) ([]*HostedProfile, error) {
	reply, err := providerEntryPointCall(ctx, "provider_entry_point_invoke", ep)
	if reply == nil {
		return nil, err
	}
	profiles, decodeErr := hostedProfilesFrom(entryPointProfileKey+ep.Name, ep.Value, reply["profiles"])
	if decodeErr != nil {
		return nil, decodeErr
	}
	return profiles, err
}

// Reply of one provider entry point request plus the exception the plugin code raised there
func providerEntryPointCall(ctx context.Context, op string, ep PipEntryPoint) (map[string]any, error) {
	m := GetPluginManager(ctx)
	if m == nil {
		return nil, &PluginHostUnavailable{Reason: "no plugin manager is bound to this profile"}
	}
	host, err := m.pluginHost()
	if err != nil {
		return nil, err
	}
	if _, err := host.ensureStarted(ctx); err != nil {
		return nil, err
	}
	raw, err := host.call(ctx, op, map[string]any{"name": ep.Name, "value": ep.Value}, nil, 0, nil)
	if err != nil {
		return nil, err
	}
	reply, _ := raw.(map[string]any)
	if failure, ok := reply["error"].(map[string]any); ok {
		return reply, &HostBuiltinError{Type: wireStr(failure["type"]), Message: wireStr(failure["message"])}
	}
	return reply, nil
}

// Object ref marked with the child generation that issued it, so the slot addresses that child
func (h *PluginHost) stampGeneration(ref map[string]any) map[string]any {
	if ref == nil {
		return nil
	}
	h.mu.Lock()
	ref["generation"] = int64(h.generation)
	h.mu.Unlock()
	return ref
}
