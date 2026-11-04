package provider

import "strings"

// Provider names and hosts a left core plugin serves
type leftCoreHostFeature struct {
	plugin    string
	label     string
	providers []string
	hosts     []string
}

var copilotAPIProviders = []string{"copilot", "github-copilot", "github-models", "github-model", "github"}

var copilotACPProviders = []string{"copilot-acp", "github-copilot-acp", "copilot-acp-agent"}

// Left core features whose providers own hosts
var leftCoreHostFeatures = []leftCoreHostFeature{
	{plugin: "github-copilot", label: "GitHub Copilot", providers: append(append([]string{}, copilotAPIProviders...), copilotACPProviders...), hosts: []string{"githubcopilot.com", "models.github.ai"}},
}

// Reports whether the url is at the host or a subdomain, an unparsable url is at no host
func leftCoreAtHost(rawURL, host string) bool {
	if _, err := parseURL(rawURL); err != nil {
		return false
	}
	return HostMatches(rawURL, host)
}

func leftCoreProviderFeature(name string) (leftCoreHostFeature, bool) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return leftCoreHostFeature{}, false
	}
	for _, f := range leftCoreHostFeatures {
		for _, p := range f.providers {
			if p == key {
				return f, true
			}
		}
	}
	return leftCoreHostFeature{}, false
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
	for _, f := range leftCoreHostFeatures {
		matched := false
		for _, h := range f.hosts {
			if leftCoreAtHost(baseURL, h) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		for _, name := range f.providers {
			if !ProviderLeftCore(name) {
				return ""
			}
		}
		return leftCoreProviderInstallHint(f.providers[0])
	}
	return ""
}
