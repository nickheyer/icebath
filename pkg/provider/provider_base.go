package provider

import (
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/providerregistry"
)

// Abstract identity every registered provider implements, the stable lowercase config key value
type NamedProvider = providerregistry.NamedProvider

// Identity and icebath tools picker metadata common to every pluggable backend provider
type ProviderBase interface {
	NamedProvider
	DisplayName() string
	GetSetupSchema() map[string]any
}

// Provider with a model catalog that is available by default and shows a title cased label
type CatalogProviderBase interface {
	ProviderBase
	IsAvailable() bool
	ListModels() []map[string]any
	DefaultModel() string
}

// Embeddable ProviderBase defaults dispatching through Self, the outer provider that supplies Name
type ProviderDefaults struct {
	Self ProviderBase
}

// Embeddable CatalogProviderBase defaults dispatching through Self, the outer provider that supplies Name
type CatalogProviderDefaults struct {
	Self CatalogProviderBase
}

// Outer provider the defaults dispatch to, panicking when the embedding provider never bound itself
func (d ProviderDefaults) self() ProviderBase {
	if d.Self == nil {
		panic("provider.ProviderDefaults.Self is not bound to its provider")
	}
	return d.Self
}

// Label shown in icebath tools, the provider name
func (d ProviderDefaults) DisplayName() string { return d.self().Name() }

// Picker row naming the provider by its display name with no badge, tag or env vars
func (d ProviderDefaults) GetSetupSchema() map[string]any { return defaultSetupSchema(d.self()) }

// Outer provider the defaults dispatch to, panicking when the embedding provider never bound itself
func (d CatalogProviderDefaults) self() CatalogProviderBase {
	if d.Self == nil {
		panic("provider.CatalogProviderDefaults.Self is not bound to its provider")
	}
	return d.Self
}

// Label shown in icebath tools, the provider name title cased
func (d CatalogProviderDefaults) DisplayName() string { return pystr.Title(d.self().Name()) }

// Picker row naming the provider by its display name with no badge, tag or env vars
func (d CatalogProviderDefaults) GetSetupSchema() map[string]any { return defaultSetupSchema(d.self()) }

// Provider can service calls, true unless overridden
func (CatalogProviderDefaults) IsAvailable() bool { return true }

// Model catalog entries, empty unless overridden
func (CatalogProviderDefaults) ListModels() []map[string]any { return []map[string]any{} }

// Id of the first catalog entry, empty when the catalog is empty or the entry has no string id
func (d CatalogProviderDefaults) DefaultModel() string {
	models := d.self().ListModels()
	if len(models) == 0 {
		return ""
	}
	id, _ := models[0]["id"].(string)
	return id
}

// Picker row shape shared by both default sets
func defaultSetupSchema(p ProviderBase) map[string]any {
	return map[string]any{"name": p.DisplayName(), "badge": "", "tag": "", "env_vars": []any{}}
}
