package provider

import (
	"net/url"
	"strconv"
	"strings"
)

// Raw netloc of a URL as typed, userinfo included
func rawNetloc(raw string) string {
	i := strings.Index(raw, "://")
	if i < 0 {
		return ""
	}
	rest := raw[i+3:]
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// NormalizeRouteBaseURL canonicalizes only the URL components proven equivalent for route identity
func NormalizeRouteBaseURL(baseURL string) string {
	raw := baseURL
	if raw == "" {
		return ""
	}
	for _, r := range raw {
		if r <= 0x20 {
			return raw
		}
	}
	beforeFragment := raw
	if i := strings.Index(raw, "#"); i >= 0 {
		beforeFragment = raw[:i]
	}
	hadQueryDelimiter := strings.Contains(beforeFragment, "?")
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Hostname() == "" {
		return raw
	}
	scheme := strings.ToLower(parsed.Scheme)
	hostname := parsed.Hostname()
	var host string
	if i := strings.Index(hostname, "%"); i >= 0 {
		host = strings.ToLower(hostname[:i]) + hostname[i:]
	} else {
		host = strings.ToLower(hostname)
	}
	port := -1
	if p := parsed.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 65535 {
			return raw
		}
		port = n
	}
	netloc := rawNetloc(raw)
	routeHost := netloc
	if i := strings.LastIndex(netloc, "@"); i >= 0 {
		routeHost = netloc[i+1:]
	}
	if strings.HasPrefix(routeHost, "[") || strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port >= 0 && !((scheme == "http" && port == 80) || (scheme == "https" && port == 443)) {
		host = host + ":" + strconv.Itoa(port)
	}
	if i := strings.LastIndex(netloc, "@"); i >= 0 {
		host = netloc[:i] + "@" + host
	}
	path := parsed.EscapedPath()
	if strings.HasSuffix(path, "/") && !hadQueryDelimiter {
		path = path[:len(path)-1]
	}
	normalized := scheme + "://" + host + path
	if parsed.RawQuery != "" {
		normalized += "?" + parsed.RawQuery
	} else if hadQueryDelimiter {
		normalized += "?"
	}
	return normalized
}

// Built in or models.dev provider definition without any network fetch
func lookupProviderDef(canonical string) (Profile, bool) {
	if p, ok := ProfileFor(canonical); ok {
		return p, true
	}
	return modelsDevProfileOffline(canonical)
}

// ProviderOwnsRoute reports whether a base URL is the provider's own endpoint and whether that can be known
func ProviderOwnsRoute(provider, baseURL string) (owned bool, known bool) {
	return providerOwnsRouteIn(provider, baseURL, nil)
}

// Route ownership against the user's providers and custom_providers config sections
func providerOwnsRouteIn(provider, baseURL string, cfg map[string]any) (owned bool, known bool) {
	host := Hostname(baseURL)
	if host == "" {
		return false, false
	}
	raw := strings.ToLower(strings.TrimSpace(provider))
	canonical := NormalizeProvider(raw)
	if canonical == "custom" || canonical == "local" {
		return true, true
	}
	var userProviders map[string]any
	var customProviders []any
	if cfg != nil {
		userProviders, _ = cfg["providers"].(map[string]any)
		customProviders, _ = cfg["custom_providers"].([]any)
	}
	userDef := resolveUserProvider(raw, userProviders)
	if userDef == nil {
		userDef = resolveCustomProvider(raw, customProviders)
	}
	if userDef != nil && userDef.BaseURL != "" {
		return Hostname(userDef.BaseURL) == host, true
	}
	pdef, found := lookupProviderDef(canonical)
	if found && pdef.BaseURL != "" && Hostname(pdef.BaseURL) == host {
		return true, true
	}
	if !found && userDef == nil {
		return true, true
	}
	inferred := inferProviderFromURL(baseURL)
	if inferred == "" {
		return false, false
	}
	return NormalizeProvider(inferred) == canonical, true
}

// DropStaleModelRoute pops base_url and api_mode left by a previous provider and reports an unverifiable route
func DropStaleModelRoute(modelCfg map[string]any, provider string, cfg map[string]any) (map[string]any, bool) {
	if modelCfg == nil {
		return map[string]any{}, false
	}
	baseURL := strings.TrimSpace(cfgString(modelCfg["base_url"]))
	if baseURL != "" {
		owned, known := providerOwnsRouteIn(provider, baseURL, cfg)
		if known && owned {
			return map[string]any{}, false
		}
		if !known {
			return map[string]any{}, true
		}
	}
	popped := map[string]any{}
	for _, k := range []string{"base_url", "api_mode"} {
		v, ok := modelCfg[k]
		if !ok || v == nil || v == "" {
			continue
		}
		popped[k] = v
		delete(modelCfg, k)
	}
	return popped, false
}

// Exact default routes a canonical provider id is known to serve from
func providerDefaultRoutes(provider string) map[string]bool {
	routes := map[string]bool{}
	add := func(v string) {
		if r := NormalizeRouteBaseURL(v); r != "" {
			routes[r] = true
		}
	}
	if p, ok := lookupProviderDef(provider); ok {
		add(p.BaseURL)
	}
	for _, p := range Profiles() {
		if NormalizeProvider(p.Name) == provider {
			add(p.BaseURL)
		}
	}
	if provider == "gemini" {
		for r := range routes {
			routes[strings.TrimRight(r, "/")+"/openai"] = true
		}
	}
	return routes
}

// ContextRouteMismatch reports whether a context pin's configured route differs from the runtime route
func ContextRouteMismatch(configuredBaseURL, activeBaseURL, configuredProvider, activeProvider string) bool {
	configuredRoute := NormalizeRouteBaseURL(configuredBaseURL)
	activeRoute := NormalizeRouteBaseURL(activeBaseURL)
	if configuredRoute != "" {
		return configuredRoute != activeRoute
	}
	configuredProvider = strings.TrimSpace(configuredProvider)
	activeProvider = strings.TrimSpace(activeProvider)
	if configuredProvider == "" {
		return false
	}
	configuredProvider = NormalizeProvider(configuredProvider)
	activeProvider = NormalizeProvider(activeProvider)
	if activeRoute != "" {
		if configuredRoutes := providerDefaultRoutes(configuredProvider); len(configuredRoutes) > 0 {
			return !configuredRoutes[activeRoute]
		}
		return !(activeProvider != "" && configuredProvider == activeProvider)
	}
	return configuredProvider != "" && activeProvider != "" && configuredProvider != activeProvider
}

// ShouldClearContextPin reports whether a configured context_length pin no longer matches its runtime route
func ShouldClearContextPin(configuredModel, activeModel, configuredBaseURL, activeBaseURL, configuredProvider, activeProvider string) (clear bool) {
	configuredModel = strings.TrimSpace(configuredModel)
	if configuredModel != "" && configuredModel != strings.TrimSpace(activeModel) {
		return true
	}
	defer func() {
		if r := recover(); r != nil {
			clear = true
		}
	}()
	return ContextRouteMismatch(configuredBaseURL, activeBaseURL, configuredProvider, activeProvider)
}

// String form of a config scalar the way Python str(value or "") renders it
func cfgString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	}
	return ""
}
