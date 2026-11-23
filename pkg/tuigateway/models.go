package tuigateway

type ModelOptionsParams struct {
	Profile             *string `json:"profile,omitzero"`
	SessionID           string  `json:"session_id,omitzero"`
	ExplicitOnly        bool    `json:"explicit_only,omitzero"`
	IncludeUnconfigured bool    `json:"include_unconfigured,omitzero"`
	Refresh             bool    `json:"refresh,omitzero"`
}

// Price per million tokens as display text, empty when unknown and free for a free model
type ModelPricing struct {
	Input  string  `json:"input"`
	Output string  `json:"output"`
	Cache  *string `json:"cache,omitzero"`
	Free   bool    `json:"free"`
}

type ModelCapabilities struct {
	Fast                bool  `json:"fast"`
	Ultrafast           bool  `json:"ultrafast,omitzero"`
	Daybreak            bool  `json:"daybreak,omitzero"`
	Reasoning           bool  `json:"reasoning"`
	CanDisableReasoning *bool `json:"can_disable_reasoning,omitzero"`
}

type ProviderLimitScope string

func (ProviderLimitScope) Values() []string { return []string{"account", "models"} }

// Rate limit of a sign in, the whole account until resets_at or only the listed models each until its time
type ProviderLimit struct {
	Scope    ProviderLimitScope `json:"scope"`
	ResetsAt *string            `json:"resets_at,omitzero"`
	Models   map[string]string  `json:"models,omitzero"`
}

type ProviderUsageScope string

func (ProviderUsageScope) Values() []string { return []string{"account", "model"} }

// One subscription usage window, its spent percent and its ISO rollover, capping the account or one model family
type ProviderUsageWindow struct {
	Label       string             `json:"label"`
	UsedPercent float64            `json:"used_percent"`
	ResetsAt    *string            `json:"resets_at,omitzero"`
	Scope       ProviderUsageScope `json:"scope,omitzero"`
}

type ProviderAccountState string

func (ProviderAccountState) Values() []string {
	return []string{"ready", "limited", "unknown", "unavailable"}
}

// One account of a credential pool by a stable non secret id, its usage windows empty while unknown
type ProviderUsageAccount struct {
	ID       string                `json:"id"`
	Label    string                `json:"label,omitzero"`
	Windows  []ProviderUsageWindow `json:"windows,omitzero"`
	State    ProviderAccountState  `json:"state"`
	ResetsAt *string               `json:"resets_at,omitzero"`
}

// Subscription usage of a provider, one gauge for a single account or one row per pooled account
type ProviderUsage struct {
	Windows  []ProviderUsageWindow  `json:"windows,omitzero"`
	Accounts []ProviderUsageAccount `json:"accounts,omitzero"`
}

// One provider of the model picker with its models, sign in state, prices, limits and usage
type ModelOptionProvider struct {
	Slug               string                       `json:"slug"`
	Name               string                       `json:"name"`
	Models             []string                     `json:"models,omitzero"`
	TotalModels        int64                        `json:"total_models,omitzero"`
	IsCurrent          bool                         `json:"is_current,omitzero"`
	IsUserDefined      bool                         `json:"is_user_defined,omitzero"`
	Source             string                       `json:"source,omitzero"`
	Aliases            []string                     `json:"aliases,omitzero"`
	APIURL             string                       `json:"api_url,omitzero"`
	AuthType           string                       `json:"auth_type,omitzero"`
	Authenticated      *bool                        `json:"authenticated,omitzero"`
	KeyEnv             string                       `json:"key_env,omitzero"`
	Warning            string                       `json:"warning,omitzero"`
	FeaturedModels     []string                     `json:"featured_models,omitzero"`
	Capabilities       map[string]ModelCapabilities `json:"capabilities,omitzero"`
	Pricing            map[string]ModelPricing      `json:"pricing,omitzero"`
	PricingPending     bool                         `json:"pricing_pending,omitzero"`
	NativeCatalogEmpty bool                         `json:"native_catalog_empty,omitzero"`
	Limit              *ProviderLimit               `json:"limit,omitzero"`
	Usage              *ProviderUsage               `json:"usage,omitzero"`
	Extra              map[string]any               `json:"-" wire:"extra"`
}

// Whether the provider holds no credential, a row the picker offers to sign in to
func (p ModelOptionProvider) Unauthenticated() bool {
	return p.Authenticated != nil && !*p.Authenticated
}

type ModelOptionsResult struct {
	Providers []ModelOptionProvider `json:"providers"`
	Model     string                `json:"model,omitzero"`
	Provider  string                `json:"provider,omitzero"`
}

type ModelSaveKeyParams struct {
	Profile   *string `json:"profile,omitzero"`
	Slug      string  `json:"slug"`
	APIKey    string  `json:"api_key"`
	SessionID string  `json:"session_id,omitzero"`
	// Login name for the key, an existing owned login of that name taking the new key
	Name string `json:"name,omitzero"`
}

type ModelSaveKeyResult struct {
	Provider ModelOptionProvider `json:"provider"`
}

type ModelDisconnectParams struct {
	Profile   *string `json:"profile,omitzero"`
	Slug      string  `json:"slug"`
	SessionID string  `json:"session_id,omitzero"`
	// Login to remove, the provider's default login when omitted
	Login string `json:"login,omitzero"`
	// Every login, env key and sign in state of the provider, the old disconnect
	All bool `json:"all,omitzero"`
}

type ModelDisconnectResult struct {
	Slug         string `json:"slug"`
	Name         string `json:"name"`
	Disconnected bool   `json:"disconnected"`
	// Login removed, empty when every credential went
	Login string `json:"login,omitzero"`
	// Logins of the provider left after the removal
	Remaining int64 `json:"remaining,omitzero"`
}

// Provider and model inventory of the picker, layered over the live provider of the session when named
var ModelOptions = NewMethod[ModelOptionsParams, ModelOptionsResult]("model.options")

// Saves an API key as an owned login of the provider, its profile default, and returns its refreshed picker row
var ModelSaveKey = NewMethod[ModelSaveKeyParams, ModelSaveKeyResult]("model.save_key")

// Removes the provider's default login, or every credential, env key and sign in state with all
var ModelDisconnect = NewMethod[ModelDisconnectParams, ModelDisconnectResult]("model.disconnect")
