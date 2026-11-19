package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/state"
)

// Parent agent surface a side agent copies its route, toolsets and store from
type sideAgentParent interface {
	BaseURL() string
	APIKey() string
	Provider() string
	APIMode() string
	ACPCommand() string
	ACPArgs() []string
	EphemeralSystemPrompt() string
	ProviderRouting() agent.ProviderRouting
	Model() string
	EnabledToolsets() []string
	DisabledToolsets() []string
	ReasoningConfig() map[string]any
	ServiceTier() string
	RequestOverrides() map[string]any
	SessionStore() agent.SessionStore
	FallbackChain() []map[string]any
}

var _ sideAgentParent = (*agent.AIAgent)(nil)

// Construction options of a detached side agent on the parent agent's route, persisting into the parent's store
func backgroundAgentKwargs(ctx context.Context, parent sideAgentParent, taskID string) (agent.AIAgentOptions, error) {
	cfg := loadCfg(ctx)
	fallback := parent.FallbackChain()
	if fallback == nil {
		fallback = []map[string]any{}
	}
	routing := parent.ProviderRouting()
	model := parent.Model()
	if model == "" {
		model = resolveModel(ctx)
	}
	enabled := parent.EnabledToolsets()
	if len(enabled) == 0 {
		enabled = loadEnabledToolsets(ctx, "tui")
	}
	disabled := parent.DisabledToolsets()
	if len(disabled) == 0 {
		disabled = loadDisabledToolsets(ctx)
	}
	reasoning := parent.ReasoningConfig()
	if len(reasoning) == 0 {
		reasoning = loadReasoningConfig(ctx, parent.Model())
	}
	tier := parent.ServiceTier()
	if tier == "" {
		tier = loadServiceTier(ctx)
	}
	overrides := map[string]any{}
	for key, value := range parent.RequestOverrides() {
		overrides[key] = value
	}
	var store agent.SessionStore
	if parentStore := parent.SessionStore(); parentStore != nil && !isNilStore(parentStore) {
		store = parentStore
	} else {
		db, err := getDB(ctx)
		if err != nil {
			return agent.AIAgentOptions{}, err
		}
		store = db
	}
	return agent.AIAgentOptions{BaseURL: parent.BaseURL(), APIKey: parent.APIKey(), Provider: parent.Provider(),
		APIMode: parent.APIMode(), ACPCommand: parent.ACPCommand(), ACPArgs: parent.ACPArgs(),
		EphemeralSystemPrompt: parent.EphemeralSystemPrompt(), ProvidersAllowed: routing.Allowed, ProvidersIgnored: routing.Ignored,
		ProvidersOrder: routing.Order, ProviderSort: routing.Sort, ProviderDataCollection: routing.DataCollection,
		OpenRouterMinCodingScore: routing.MinCodingScore, Model: model, MaxIterations: new(cfgMaxTurns(cfg, 25)),
		EnabledToolsets: enabled, DisabledToolsets: disabled, QuietMode: true, VerboseLogging: false,
		ProviderRequireParameters: routing.RequireParameters, SessionID: taskID, ReasoningConfig: reasoning, ServiceTier: tier,
		RequestOverrides: overrides, Platform: "tui", SessionDB: store, FallbackModel: fallback, SideAgent: true}, nil
}

// Whether a session store interface holds a nil pointer
func isNilStore(store agent.SessionStore) bool {
	db, isDB := store.(*state.DB)
	return isDB && db == nil
}

// Side agent's own registry reference on the parent's store for its turn, released by the returned call
func sideAgentSessionDB(ctx context.Context, parentStore agent.SessionStore) (agent.SessionStore, func(), error) {
	db, isDB := parentStore.(*state.DB)
	if parentStore == nil || !isDB || db == nil || db.Path() == "" {
		return parentStore, func() {}, nil
	}
	acquired, err := state.Acquire(ctx, db.Path())
	if err != nil {
		return nil, nil, err
	}
	return acquired, func() { state.ReleaseOrClose(acquired) }, nil
}
