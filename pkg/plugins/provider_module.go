package plugins

import (
	"context"
	"errors"
)

// Refusal of every former Python provider submodule call
var errPythonProviderModule = errors.New("memory provider submodules are not importable; icebath runs memory providers compiled in or as runtime plugins")

// Plugin guest of every manager this process created whose guest is running
func RunningPluginHosts() []*PluginHost {
	var hosts []*PluginHost
	for _, m := range knownPluginManagers() {
		for _, host := range m.GuestHosts() {
			if host.Alive() {
				hosts = append(hosts, host)
			}
		}
	}
	return hosts
}

// Calls one function of a memory provider submodule, refused for every provider
func CallProviderModule(ctx context.Context, pluginDir, moduleName, submodule, function string, args []any, kwargs map[string]any) (result any, loaded bool, err error) {
	return nil, false, errPythonProviderModule
}

// Live module of a memory provider submodule, refused for every provider
func ImportProviderModule(ctx context.Context, name, pluginDir, moduleName, submodule string) (*HostedObject, error) {
	return nil, errPythonProviderModule
}

// Live module of a memory provider submodule in one guest, refused for every provider
func (h *PluginHost) ImportProviderModule(ctx context.Context, name, pluginDir, moduleName, submodule string) (*HostedObject, error) {
	return nil, errPythonProviderModule
}
