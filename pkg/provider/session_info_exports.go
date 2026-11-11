package provider

import "context"

// Nearest weaker supported effort, the request verbatim when supported or outside the ladder
func ClampEffort(effort string, supported []string) string {
	return clampEffort(effort, supported, nil)
}

// Reasoning efforts the route of a provider, model and API mode accepts
func RouteSupportedEfforts(provider, model, apiMode string) []string {
	return routeSupportedEfforts(provider, model, apiMode)
}

// True when the free tier carries inference and the auto resolver lands on Nous
func FreeTierRoute(ctx context.Context) bool { return freeTierRoute(modelsPoolEnv(ctx)) }

// True while a secret scope is bound to the context
func SecretScopeBound(ctx context.Context) bool { return boundScope(ctx) != nil }
