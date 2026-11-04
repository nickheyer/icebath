package provider

import "strings"

// Credential is the provider's auth.json singleton login, the usable Nous invoke JWT for nous
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
	switch providerID {
	case "nous":
		for _, pair := range [][2]any{{state["agent_key"], state["agent_key_expires_at"]}, {state["access_token"], state["expires_at"]}} {
			token := strings.TrimSpace(anyString(pair[0]))
			if token != "" && NousInvokeJWTUsable(token, state["scope"], pair[1]) {
				key = token
				break
			}
		}
	default:
		if tokens, ok := state["tokens"].(map[string]any); ok {
			key = strings.TrimSpace(anyString(tokens["access_token"]))
		}
		if key == "" {
			key = strings.TrimSpace(anyString(state["access_token"]))
		}
		if key == "" {
			key = strings.TrimSpace(anyString(state["api_key"]))
		}
	}
	if key == "" {
		return Credential{}, false
	}
	return Credential{APIKey: key, BaseURL: baseURL, Source: "auth_store"}, true
}
