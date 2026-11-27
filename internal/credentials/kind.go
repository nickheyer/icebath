package credentials

import (
	"cmp"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
)

// Route of a tool's call to a kind of model provider, such as an image service that takes the OpenAI key: the profile's provider of the
// kind with the account its choice and rotation give, renewed when due; else a credential of the kind the profile's environment holds, at
// the address its variable sets or the kind's own. False when there is neither
func (r *Registry) Kind(providers *provider.Registry, cfg *config.Config, kind string) (provider.Route, bool) {
	for _, instance := range providers.Instances(cfg) {
		if instance.Known && instance.Spec.Name == kind {
			route, err := r.Route(instance, "")
			return route, err == nil && strings.TrimSpace(route.APIKey) != ""
		}
	}
	spec, ok := providers.Spec(kind)
	if !ok {
		return provider.Route{}, false
	}
	credential, ok := r.Detect(cfg, spec)
	if !ok || strings.TrimSpace(credential.APIKey) == "" {
		return provider.Route{}, false
	}
	base := strings.TrimSpace(credential.BaseURL)
	if base == "" && spec.BaseURLEnv != "" {
		base = strings.TrimSpace(cfg.Getenv(spec.BaseURLEnv))
	}
	if base == "" {
		base = spec.BaseURL
	}
	headers := map[string]string{}
	for name, value := range spec.Headers {
		headers[name] = value
	}
	for name, value := range credential.Headers {
		headers[name] = value
	}
	return provider.Route{Provider: spec.Name, Spec: spec.Name, BaseURL: strings.TrimRight(base, "/"), APIKey: strings.TrimSpace(credential.APIKey),
		Headers: headers, Mode: spec.Mode, Auth: cmp.Or(credential.Auth, spec.Auth), Local: spec.Local}, true
}
