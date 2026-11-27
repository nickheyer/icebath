package config

const (
	Key = "models"
	// Where the models.dev registry of model details is read, the public one when empty
	DevURL = Key + ".dev_url"
	// Limits and abilities of models over what icebath knows of them, by provider and then model, _default filling what nothing knows
	Overrides = Key + ".overrides"
)

// Online model catalog: the curated lists the pickers start from
const (
	CatalogKey = Key + ".catalog"
	// Whether the catalog is fetched; the one shipped with icebath stands in when not
	CatalogEnabled = CatalogKey + ".enabled"
	// Address of the catalog
	CatalogURL = CatalogKey + ".url"
	// Minutes a fetched catalog is used before it is fetched again
	CatalogTTL = CatalogKey + ".ttl_minutes"
	// Catalogs of single providers over the main one, by provider and then url
	CatalogProviders = CatalogKey + ".providers"
	// Kinds of provider left out of the model pickers
	CatalogExcluded = CatalogKey + ".excluded_providers"
)

// Catalog shipped with icebath, fetched again from these addresses
const (
	DefaultCatalogURL = "https://icebath.app/api/model-catalog.json"
	// Copy of the same catalog where the repository keeps it, tried when the main address fails
	MirrorCatalogURL  = "https://raw.githubusercontent.com/nickheyer/icebath/main/internal/models/model-catalog.json"
	DefaultCatalogTTL = 20
)
