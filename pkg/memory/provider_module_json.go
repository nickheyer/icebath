package memory

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/plugins"
)

// Calls a function of the installed provider package submodule with JSON text arguments, an ImportError when it is not installed
func CallProviderModuleJSON(ctx context.Context, name, submodule, function, argsJSON string, pathArgs []int, require []string, track [][]any) (*plugins.ProviderModuleJSONResult, error) {
	r, err := installedProviderPackage(ctx, name, submodule)
	if err != nil {
		return nil, err
	}
	out, loaded, err := plugins.ProviderModuleJSONCall(ctx, r.Dir, providerModuleName(r), submodule, function, argsJSON, pathArgs, require, track)
	if !loaded && err == nil {
		return nil, &ProviderImportError{Msg: fmt.Sprintf("memory provider '%s' failed to import", name)}
	}
	return out, err
}

// Enters context managers of the installed provider package submodule until the hold is released
func HoldProviderModuleManagers(ctx context.Context, name, submodule string, managers []plugins.ProviderModuleManager) (*plugins.ProviderModuleHold, error) {
	r, err := installedProviderPackage(ctx, name, submodule)
	if err != nil {
		return nil, err
	}
	hold, loaded, err := plugins.HoldProviderModuleManagers(ctx, r.Dir, providerModuleName(r), submodule, managers)
	if !loaded && err == nil {
		return nil, &ProviderImportError{Msg: fmt.Sprintf("memory provider '%s' failed to import", name)}
	}
	return hold, err
}
