package gateway

// Authority serving a profile home, nil when this runtime does not serve it
func AuthorityForHome(runner any, profileHome string) (*SessionAuthority, error) {
	return authorityForHome(runner, profileHome)
}

// Binds a verified native actor as a native legacy transport of the authority
func BindNativeTransport(a *SessionAuthority, actor Principal, identity map[string]any) {
	bindNativeTransport(a, actor, identity)
}
