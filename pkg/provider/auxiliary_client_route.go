package provider

// Concrete provider the resolver stamped on an auto routed client, else the fallback
func auxEffectiveProviderForClient(client AuxClient, fallback string) string {
	if p := auxClientEffectiveProvider(client); p != "" {
		return p
	}
	return fallback
}
