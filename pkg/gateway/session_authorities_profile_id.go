package gateway

// Authority serving the profile id home, nil when this runtime does not serve it
func AuthorityForProfileID(runner any, profileID string) (*SessionAuthority, error) {
	return authorityForHome(runner, profileID)
}
