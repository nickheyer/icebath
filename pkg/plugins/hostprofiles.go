package plugins

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

const (
	hostExtractTimeout   = 60 * time.Second
	hostProfileCacheVers = 2
)

// Model provider profile whose overridden methods and callable fields run in the plugin host
type HostedProfile struct {
	Name      string
	Type      string
	Fields    map[string]any
	Calls     map[string]HostedProfileCall
	PluginDir string
	Module    string
	// Why hermes would import this profile in process, its create_client then serving a live host client
	InProcess string
}

// One overridden method or callable field of a hosted profile
type HostedProfileCall struct {
	Async bool
	Sig   []wireParam
	Field bool
}

func (p *HostedProfile) String() string {
	return fmt.Sprintf("<plugin-host profile %s>", config.PyRepr(p.Name))
}

// Runs one overridden method or callable field in the plugin host of the active profile
func (p *HostedProfile) Call(ctx context.Context, attr string, args []any, kwargs map[string]any) (any, error) {
	m := GetPluginManager(ctx)
	if m == nil {
		return nil, &PluginHostUnavailable{Reason: "plugins.isolation is no longer 'host'; restart Icebath"}
	}
	host, err := m.hostForDir(p.PluginDir)
	if err != nil {
		return nil, err
	}
	return host.ProfileCall(ctx, p.PluginDir, p.Module, p.Name, attr, args, kwargs)
}

var hostedProfilesMu sync.Mutex
var hostedProfiles = map[string][]*HostedProfile{}

// Hosted profile proxies of a model provider plugin plus the reason each profile that must load in process cannot be hosted
func LoadHostedProfiles(ctx context.Context, pluginDir, moduleName string) ([]*HostedProfile, map[string]string, error) {
	all, err := loadProviderProfiles(ctx, pluginDir, moduleName)
	if err != nil {
		return nil, nil, err
	}
	inProcess := map[string]string{}
	hosted := []*HostedProfile{}
	for _, profile := range all {
		if profile.InProcess != "" {
			inProcess[profile.Name] = profile.InProcess
			continue
		}
		hosted = append(hosted, profile)
	}
	hostedProfilesMu.Lock()
	hostedProfiles[pluginDir] = hosted
	hostedProfilesMu.Unlock()
	return hosted, inProcess, nil
}

// Every profile a model provider plugin registers in extraction order, in process ones carrying their reason
func loadProviderProfiles(ctx context.Context, pluginDir, moduleName string) ([]*HostedProfile, error) {
	payload, err := cachedProfileExtraction(ctx, pluginDir, moduleName)
	if err != nil {
		return nil, err
	}
	if e := payload["error"]; config.PyTruthy(e) {
		return nil, &PluginHostUnavailable{Reason: config.PyStr(e)}
	}
	return hostedProfilesFrom(pluginDir, moduleName, payload["profiles"])
}

// Profile proxies of extraction entries, the in process reason kept on each profile that has one
func hostedProfilesFrom(pluginDir, moduleName string, entries any) ([]*HostedProfile, error) {
	out := []*HostedProfile{}
	for _, raw := range listOrEmpty(entries) {
		entry, _ := raw.(map[string]any)
		if entry == nil {
			continue
		}
		profile, err := hostedProfileFrom(pluginDir, moduleName, entry)
		if err != nil {
			return nil, err
		}
		if reason := entry["in_process"]; config.PyTruthy(reason) {
			profile.InProcess = config.PyStr(reason)
		}
		out = append(out, profile)
	}
	return out, nil
}

func hostedProfileFrom(pluginDir, moduleName string, entry map[string]any) (*HostedProfile, error) {
	p := &HostedProfile{Name: config.PyStr(entry["name"]), Type: wireStr(entry["type"]), Fields: map[string]any{},
		Calls: map[string]HostedProfileCall{}, PluginDir: pluginDir, Module: moduleName}
	for key, value := range mapOrEmpty(entry["fields"]) {
		decoded, err := wireDecode(value, nil)
		if err != nil {
			return nil, err
		}
		p.Fields[key] = decoded
	}
	for attr, raw := range mapOrEmpty(entry["calls"]) {
		meta := mapOrEmpty(raw)
		p.Calls[attr] = HostedProfileCall{Async: config.PyTruthy(meta["async"]), Sig: signatureFrom(meta["sig"]), Field: config.PyTruthy(meta["field"])}
	}
	return p, nil
}

// Reports whether a profile value is a hosted proxy whose code runs in the plugin host
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
