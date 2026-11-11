package gateway

// Authorities that accept work, the launch authority of a bare runner unless it is retiring
func servingAuthorities(runner any) []*SessionAuthority {
	if registry := registryOf(runner); registry != nil {
		return registry.Serving()
	}
	if bare, ok := runner.(launchAuthorityRunner); ok {
		if authority := bare.SessionAuthority(); authority != nil && !authority.Retiring {
			return []*SessionAuthority{authority}
		}
	}
	return nil
}
