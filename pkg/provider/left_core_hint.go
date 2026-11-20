package provider

import "strings"

// Reports whether the url is at the host or a subdomain, an unparsable url is at no host
func leftCoreAtHost(rawURL, host string) bool {
	if _, err := parseURL(rawURL); err != nil {
		return false
	}
	return HostMatches(rawURL, host)
}

func leftCoreProviderFeature(name string) (*LeftCoreFeature, bool) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return nil, false
	}
	for i := range leftCoreTable {
		if containsString(leftCoreTable[i].Providers, key) {
			return &leftCoreTable[i], true
		}
	}
	return nil, false
}

// ProviderLeftCore reports a provider a plugin supplies that no registered profile serves now
func ProviderLeftCore(name string) bool {
	if _, ok := leftCoreProviderFeature(name); !ok {
		return false
	}
	_, registered := ProfileFor(strings.ToLower(strings.TrimSpace(name)))
	return !registered
}

// HostInstallHint names the install sentence for a left core host whose providers are all unregistered
func HostInstallHint(baseURL string) string {
	for i := range leftCoreTable {
		f := &leftCoreTable[i]
		matched := false
		for _, h := range f.Hosts {
			if leftCoreAtHost(baseURL, h) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		for _, name := range f.Providers {
			if !ProviderLeftCore(name) {
				return ""
			}
		}
		return leftCoreProviderInstallHint(f.Providers[0])
	}
	return ""
}
