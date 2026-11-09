package provider

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/providerregistry"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Python TypeError a provider registry raises for an object that is not an instance of its provider class
type ProviderTypeError = providerregistry.ProviderTypeError

// Registry key of a name stripped of whitespace and lowercased for case insensitive registries
func LowerKey(name string) string { return providerregistry.LowerKey(name) }

// Construction options of a provider registry, Normalize defaulting to whitespace stripping
type ProviderRegistryOptions = providerregistry.Options

// Cache fingerprint of a registry, the global generation and the active scope generation
type ProviderRegistryGeneration = providerregistry.Generation

// Global and per profile scope provider maps with plugin snapshot and restore support
type ProviderRegistry[P NamedProvider] = providerregistry.Registry[P]

// Registry for providers of type P, an all uppercase label keeping its case in log lines
func NewProviderRegistry[P NamedProvider](opts ProviderRegistryOptions) *ProviderRegistry[P] {
	return providerregistry.New[P](opts)
}

// Historical module level API of a provider registry module plus its providers, scoped providers and lock test hooks
type ProviderRegistryAPI[P NamedProvider] = providerregistry.API[P]

// The untyped registry view satisfies the plugin context contract
var _ plugins.ScopedRegistry = providerregistry.ScopedRegistry(nil)

// Provider whose availability probe is_available_safe guards
type AvailabilityProvider = providerregistry.AvailabilityProvider

// Availability of a provider, a panicking probe reading as unavailable and logged at level with its traceback when excInfo
func IsAvailableSafe(provider AvailabilityProvider, logger *slog.Logger, format string, level slog.Level, excInfo bool) bool {
	return providerregistry.IsAvailableSafe(provider, logger, format, level, excInfo)
}

// Configured section provider from the read only config, stripped, with the managed Nous selection read as fal
func ConfiguredProviderName(ctx context.Context, section string, logger *slog.Logger) string {
	configured := ""
	cfg, err := config.LoadReadonly(ctx, home.Dir(ctx), nil)
	if err != nil {
		if logger == nil {
			logger = slog.Default()
		}
		logger.Debug(fmt.Sprintf("Could not read %s.provider from config: %s", section, err))
	} else if raw, ok := configBlock(configBlock(map[string]any(cfg))[section])["provider"].(string); ok && pystr.Strip(raw) != "" {
		configured = pystr.Strip(raw)
	}
	if configured != "" && pystr.Lower(configured) == tools.NousManagedProvider {
		configured = "fal"
	}
	return configured
}

// Mapping value of a config node, nil for any non mapping
func configBlock(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case config.Tree:
		return m
	}
	return nil
}
