package provider

// Whether a provider is one of the aggregators that want vendor prefixed model names
func IsAggregatorModelProvider(name string) bool { return aggregatorModelProviders[name] }
