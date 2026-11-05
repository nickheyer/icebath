package provider

// Credential for the async twin, the sync key provider when the SDK client has one, else the static key
func auxAsyncAPIKey(sync AuxClient) any {
	if sdk, ok := sync.(*auxOpenAIClient); ok && sdk != nil && sdk.apiKeyProvider != nil {
		return sdk.apiKeyProvider
	}
	return sync.APIKey()
}

// Copy of the default headers the sync SDK client was built with, empty for adapters
func auxConfiguredDefaultHeaders(sync AuxClient) map[string]string {
	out := map[string]string{}
	if sdk, ok := sync.(*auxOpenAIClient); ok && sdk != nil {
		for k, v := range sdk.customHeaders {
			out[k] = v
		}
	}
	return out
}
