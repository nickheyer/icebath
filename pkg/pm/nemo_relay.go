//go:build !icebath_core && !icebath_noextras

package pm

// Locked nemo-relay wheel of a target, false where the lock carries no wheel for it
func managedNemoRelayWheel(target string) (Wheel, bool) {
	for _, p := range extrasLock().Extras[relayExtra].Packages {
		if CanonicalizeName(p.Name) != relayExtra {
			continue
		}
		for _, w := range p.Wheels {
			if containsString(wheelTargets(w.URL), target) {
				return Wheel{URL: w.URL, SHA256: w.SHA256}, true
			}
		}
	}
	return Wheel{}, false
}
