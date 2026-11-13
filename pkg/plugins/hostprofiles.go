package plugins

import (
	"context"
	"fmt"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
)

// Model provider profile a guest plugin registered, its methods and callable fields running in the guest
type HostedProfile struct {
	Name      string
	Type      string
	Fields    map[string]any
	Calls     map[string]HostedProfileCall
	PluginDir string
	inst      *guestInstance
	creates   bool
}

// One method or callable field of a hosted profile
type HostedProfileCall struct {
	Async bool
	Sig   []wireParam
	Field bool
}

func (p *HostedProfile) String() string {
	return fmt.Sprintf("<plugin profile %s>", config.PyRepr(p.Name))
}

// Whether the profile builds its own client with create_client
func (p *HostedProfile) CreatesClient() bool { return p.creates }

// Parameters of one profile call, the guest loaded again first when the guest that registered the profile exited
func (p *HostedProfile) params(ctx context.Context, attr string, args []any, kwargs map[string]any) (map[string]any, error) {
	if _, _, err := p.inst.current(ctx); err != nil {
		return nil, err
	}
	h := p.inst.host
	return map[string]any{"plugin_key": p.inst.key(), "profile": p.Name, "attr": attr,
		"args": h.encodeArgs(nonNilArgs(args)), "kwargs": h.encodeArgs(nonNilKwargs(kwargs))}, nil
}

// Runs one method or callable field of the profile in its guest
func (p *HostedProfile) Call(ctx context.Context, attr string, args []any, kwargs map[string]any) (any, error) {
	params, err := p.params(ctx, attr, args, kwargs)
	if err != nil {
		return nil, err
	}
	return p.inst.host.call(ctx, "profile_call", params, nil, 0, nil)
}

// Runs create_client in the guest and returns the client it built as a hosted object rebuilt after a guest restart
func (p *HostedProfile) CreateClient(ctx context.Context, args []any, kwargs map[string]any) (*HostedObject, error) {
	h := p.inst.host
	build := func(ctx context.Context) (map[string]any, error) {
		params, err := p.params(ctx, "create_client", args, kwargs)
		if err != nil {
			return nil, err
		}
		raw, err := h.callIn(ctx, hostedRawRefBase, "profile_call", params, nil, 0, nil)
		if err != nil {
			return nil, err
		}
		ref, _ := raw.(map[string]any)
		if _, ok := ref["__object__"]; !ok {
			return nil, fmt.Errorf("profile %s create_client returned %T, not a client object", config.PyRepr(p.Name), raw)
		}
		return h.stampGeneration(ref), nil
	}
	ref, err := build(ctx)
	if err != nil {
		return nil, err
	}
	return h.objectProxy(ref, hostedClientBase, build), nil
}

var hostedProfilesMu sync.Mutex
var hostedProfiles = map[string][]*HostedProfile{}

// Every profile a model provider guest plugin directory registers, in registration order
func LoadProviderPluginProfiles(ctx context.Context, pluginDir string) ([]*HostedProfile, error) {
	mf, err := requireGuestManifest(pluginDir)
	if err != nil {
		return nil, err
	}
	inst, err := GetPluginManager(ctx).guestProviderLoad(mf, "model_provider", nil, false, nil)
	if err != nil {
		return nil, err
	}
	items, _, err := inst.current(ctx)
	if err != nil {
		return nil, err
	}
	profiles := []*HostedProfile{}
	for _, item := range items {
		ref, _ := item["value"].(map[string]any)
		if _, isObject := ref["__object__"]; !isObject {
			return nil, fmt.Errorf("plugin '%s' registered a model provider that is not a profile object", mf.Name)
		}
		profile, err := hostedProfileFrom(pluginDir, inst, ref, mapOrEmpty(item["attrs"]))
		if err != nil {
			return nil, err
		}
		profiles = append(profiles, profile)
	}
	hostedProfilesMu.Lock()
	hostedProfiles[pluginDir] = profiles
	hostedProfilesMu.Unlock()
	return profiles, nil
}

// Profile of one captured profile object, callable attributes kept as calls and every other attribute as a field
func hostedProfileFrom(pluginDir string, inst *guestInstance, ref, attrs map[string]any) (*HostedProfile, error) {
	p := &HostedProfile{Type: wireStr(ref["type"]), Fields: map[string]any{}, Calls: map[string]HostedProfileCall{}, PluginDir: pluginDir, inst: inst}
	for key, value := range attrs {
		if wire, ok := value.(map[string]any); ok {
			if _, callable := wire["__callable__"]; callable {
				p.Calls[key] = HostedProfileCall{Async: config.PyTruthy(wire["async"]), Sig: signatureFrom(wire["sig"]), Field: true}
				continue
			}
		}
		decoded, err := wireDecode(value, nil)
		if err != nil {
			return nil, err
		}
		p.Fields[key] = decoded
	}
	for name, raw := range mapOrEmpty(ref["methods"]) {
		if name == "create_client" {
			p.creates = true
			continue
		}
		meta := mapOrEmpty(raw)
		p.Calls[name] = HostedProfileCall{Async: config.PyTruthy(meta["async"]), Sig: signatureFrom(meta["sig"])}
	}
	p.Name = config.PyStr(p.Fields["name"])
	if p.Name == "" {
		return nil, fmt.Errorf("a model provider profile of %s has no name attribute", pluginDir)
	}
	return p, nil
}

// Reports whether a profile value is a hosted proxy whose code runs in a guest
func IsHostedProfile(profile any) bool {
	_, ok := profile.(*HostedProfile)
	return ok
}

// Env var names of every hosted profile that reads ambient credentials
func hostedAmbientEnvNames() []string {
	hostedProfilesMu.Lock()
	defer hostedProfilesMu.Unlock()
	names := map[string]bool{}
	for _, profiles := range hostedProfiles {
		for _, p := range profiles {
			if !config.PyTruthy(p.Fields["ambient_credentials"]) {
				continue
			}
			for _, name := range argStrings(p.Fields["env_vars"]) {
				names[name] = true
			}
		}
	}
	return sortedKeys(names)
}
