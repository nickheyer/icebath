package gateway

// Authority serving a profile home, nil when this runtime does not serve it
func AuthorityForHome(runner any, profileHome string) (*SessionAuthority, error) {
	return authorityForHome(runner, profileHome)
}
