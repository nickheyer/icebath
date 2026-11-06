package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Synthetic parent package of non bundled provider packages in the plugin host
const userMemoryNamespace = "_icebath_user_memory"

// Python ImportError raised when a provider package or submodule cannot be imported
type ProviderImportError struct{ Msg string }

func (e *ProviderImportError) Error() string { return e.Msg }

// Refusal the honcho plugin raises when the profile config may not be written
type ConfigWriteRefused struct{ Msg string }

func (e *ConfigWriteRefused) Error() string { return e.Msg }

// Runs a function of a provider package submodule inside the plugin host, swapped by tests
var providerModuleCall = plugins.CallProviderModule

// Module name a provider package loads under, plugins.memory for bundled, the synthetic user namespace otherwise
func providerModuleName(r *ProviderRegistration) string {
	if r.Source == SourceBundled {
		return "plugins.memory." + r.Name
	}
	resolved, err := pypath.Realpath(r.Dir, pypath.Lax)
	if err != nil {
		resolved = r.Dir
	}
	sum := sha256.Sum256([]byte(resolved))
	return fmt.Sprintf("%s.%s__source_%s", userMemoryNamespace, r.Name, hex.EncodeToString(sum[:])[:16])
}

// Calls a function of the installed copy of a provider package submodule, an ImportError when it is not installed
func CallProviderModule(ctx context.Context, name, submodule, function string, args []any, kwargs map[string]any) (any, error) {
	r := FindProvider(ctx, nil, name)
	if r == nil || !hasProviderDir(r) {
		return nil, &ProviderImportError{Msg: fmt.Sprintf("memory provider '%s' is not installed", name)}
	}
	if r.Source == SourceBundled {
		return nil, &ProviderImportError{Msg: fmt.Sprintf("No module named 'plugins.memory.%s.%s'", name, submodule)}
	}
	out, loaded, err := providerModuleCall(ctx, r.Dir, providerModuleName(r), submodule, function, args, kwargs)
	if !loaded {
		return nil, &ProviderImportError{Msg: fmt.Sprintf("memory provider '%s' failed to import", name)}
	}
	return out, err
}

// Clones the honcho peer config into a new profile, true when it cloned, a ConfigWriteRefused when the write was refused
func CloneHonchoForProfile(ctx context.Context, profileName string) (bool, error) {
	out, err := CallProviderModule(ctx, "honcho", "cli", "clone_honcho_for_profile", []any{profileName}, nil)
	if err != nil {
		var he *plugins.PluginHostError
		if errors.As(err, &he) && he.TypeName == "ConfigWriteRefused" {
			return false, &ConfigWriteRefused{Msg: he.RemoteMessage}
		}
		return false, err
	}
	return pyTruthy(out), nil
}
