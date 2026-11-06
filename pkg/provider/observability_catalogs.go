package provider

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/observability"
)

// Installs the provider owned shared metrics catalogs and the models.dev and api key env lookups
func registerObservabilityProviderCatalogs() {
	observability.SetCatalogSource("provider_names", shippedProviderNames)
	observability.SetCatalogSource("user_named_model_providers", userNamedModelProviders)
	observability.SetCatalogSource("custom_provider_aliases", customProviderAliasNames)
	observability.SetCatalogSource("public_model_ids", publicModelIDs)
	observability.SetCatalogSource("aux_task_names", func() ([]string, error) { return builtinAuxTaskKeys(), nil })
	observability.ModelsDevProviderHook = func(name string) bool {
		_, ok := ModelsDevProviderInfo(context.Background(), name, false)
		return ok
	}
	observability.ProviderForAPIKeyEnvHook = func(envVar string) (string, bool) {
		for _, it := range providerRegistry().items(context.Background()) {
			for _, key := range it.Row.KeyEnv {
				if key == envVar {
					return it.Key, true
				}
			}
		}
		return "", false
	}
}

// The three alias tables the catalogs fold, providers, auth and models catalog spellings
func providerAliasTables() []map[string]string {
	return []map[string]string{providerDefAliases, authProviderAliases, catalogProviderAliases}
}

// Built in auth ids, static overlays, every alias table, bundled profiles with aliases, openrouter and custom
func shippedProviderNames() ([]string, error) {
	ctx := context.Background()
	listProviders(ctx)
	reg := providerRegistry()
	reg.mu.RLock()
	out := make([]string, 0, len(reg.builtin)+64)
	for name := range reg.builtin {
		out = append(out, name)
	}
	reg.mu.RUnlock()
	for name := range icebathOverlays {
		out = append(out, name)
	}
	for _, table := range providerAliasTables() {
		for alias := range table {
			out = append(out, alias)
		}
	}
	regMu.RLock()
	for name, p := range regProfiles {
		if regSources[name] == "bundled" {
			out = append(out, name)
			out = append(out, p.Aliases...)
		}
	}
	regMu.RUnlock()
	return append(out, "openrouter", observability.Custom), nil
}

// Registry rows served from a loopback host, their aliases in every table, and custom
func userNamedModelProviders() ([]string, error) {
	loopback := map[string]bool{}
	for _, it := range providerRegistry().items(context.Background()) {
		u, err := pyurl.Parse(it.Row.BaseURL)
		if err != nil {
			return nil, err
		}
		if loopbackHosts[u.Hostname()] {
			loopback[it.Key] = true
		}
	}
	out := []string{observability.Custom}
	for name := range loopback {
		out = append(out, name)
	}
	for _, table := range providerAliasTables() {
		for alias, canon := range table {
			if loopback[canon] {
				out = append(out, alias)
			}
		}
	}
	return out, nil
}

// Aliases any table routes through the generic custom provider
func customProviderAliasNames() ([]string, error) {
	var out []string
	for _, table := range providerAliasTables() {
		for alias, canon := range table {
			if canon == observability.Custom {
				out = append(out, alias)
			}
		}
	}
	return out, nil
}

// Lower cased static catalog and cached models.dev model ids, each also without its vendor prefix
func publicModelIDs() ([]string, error) {
	providerModelsOnce.Do(buildProviderModels)
	ids := map[string]bool{}
	for _, models := range providerModels {
		for _, m := range models {
			ids[m] = true
		}
	}
	for _, entry := range fetchModelsDevIn(home.Dir(context.Background()), false) {
		if e, ok := entry.(map[string]any); ok {
			if models, ok := e["models"].(map[string]any); ok {
				for m := range models {
					ids[m] = true
				}
			}
		}
	}
	out := make([]string, 0, 2*len(ids))
	for m := range ids {
		out = append(out, pystr.Lower(m), pystr.Lower(m[strings.LastIndex(m, "/")+1:]))
	}
	return out, nil
}
