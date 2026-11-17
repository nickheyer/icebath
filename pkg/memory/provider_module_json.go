package memory

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/plugins"
)

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
