package agent

// ACP command of the agent's route, empty for an HTTP route
func (a *AIAgent) ACPCommand() string { return a.acpCommand }

// ACP arguments of the agent's route
func (a *AIAgent) ACPArgs() []string { return a.acpArgs }

// Provider routing preferences a side agent inherits, OpenRouter's only, ignore, order, sort and data collection
type ProviderRouting struct {
	Allowed, Ignored, Order, Sort any
	RequireParameters             bool
	DataCollection                any
	MinCodingScore                any
}

// Provider routing preferences the agent was built with
func (a *AIAgent) ProviderRouting() ProviderRouting {
	return ProviderRouting{Allowed: a.providersAllowed, Ignored: a.providersIgnored, Order: a.providersOrder, Sort: a.providerSort,
		RequireParameters: a.providerRequireParameters, DataCollection: a.providerDataCollection, MinCodingScore: a.openrouterMinCodingScore}
}

// Fallback chain the agent fails over through, empty when deliberately cleared
func (a *AIAgent) FallbackChain() []map[string]any { return a.fallbackChain }

// Callback the background self improvement review reports its summary through, nil when none is wired
func (a *AIAgent) BackgroundReviewCallback() any { return a.backgroundReviewCallback }
