package provider

import "strings"

// Credential is the provider's auth.json singleton login
func (s *Store) Credential(provider string) (Credential, bool) {
	providerID := strings.ToLower(strings.TrimSpace(provider))
	if providerID == "" {
		return Credential{}, false
	}
	state, err := s.GetProviderAuthState(providerID)
	if err != nil || len(state) == 0 {
		return Credential{}, false
	}
	baseURL := optionalBaseURL(state["inference_base_url"])
	key := ""
	if tokens, ok := state["tokens"].(map[string]any); ok {
		key = strings.TrimSpace(anyString(tokens["access_token"]))
	}
	if key == "" {
		key = strings.TrimSpace(anyString(state["access_token"]))
	}
	if key == "" {
		key = strings.TrimSpace(anyString(state["api_key"]))
	}
	if key == "" {
		return Credential{}, false
	}
	return Credential{APIKey: key, BaseURL: baseURL, Source: "auth_store"}, true
}
