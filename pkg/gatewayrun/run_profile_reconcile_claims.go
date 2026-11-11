package gatewayrun

// Startup claim map rebuilt from what is live now, primary claims plus every connected secondary's
func (r *GatewayRunner) liveResourceClaims(active string) map[claimKey]string {
	claimed := r.primaryResourceClaims(active)
	for profileName, adapters := range r.profileAdapters.All() {
		for platform, adapter := range adapters.All() {
			for _, claim := range r.adapterClaims(platform, adapter) {
				if claim != nil {
					claimed[*claim] = profileName
				}
			}
		}
	}
	return claimed
}
