package memory

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/plugins"
)

var errTransitionalProviderModule = errors.New("memory provider submodules are not importable")

func ImportProviderModule(ctx context.Context, name, submodule string) (*plugins.HostedObject, error) {
	return nil, errTransitionalProviderModule
}

func ImportProviderModuleIn(ctx context.Context, host *plugins.PluginHost, name, submodule string) (*plugins.HostedObject, error) {
	return nil, errTransitionalProviderModule
}

func CallProviderModuleJSON(ctx context.Context, name, submodule, function, argsJSON string, pathArgs []int, require []string, track [][]any) (*plugins.ProviderModuleJSONResult, error) {
	return nil, errTransitionalProviderModule
}

func HoldProviderModuleManagers(ctx context.Context, name, submodule string, managers []plugins.ProviderModuleManager) (*plugins.ProviderModuleHold, error) {
	return nil, errTransitionalProviderModule
}
