package provider

import (
	"context"
	"strings"

	"github.com/google/uuid"
)

// Header the OpenCode relay pins a conversation to one upstream backend with
const OpencodeSessionHeader = "x-opencode-session"

// Named custom entry lookup seam the OpenCode transport consults for a declared api_mode
var opencodeNamedCustomEntryFn = func(ctx context.Context, provider string) map[string]any {
	tree, err := auxLoadConfigFn(ctx)
	if err != nil {
		return nil
	}
	return newResolver(ctx, tree, nil, "", nil).getNamedCustomProvider(provider, nil, true)
}

// OpencodeTransport is the api mode and base URL rederived per model for an OpenCode relay target, empty mode otherwise
func OpencodeTransport(ctx context.Context, provider, model, baseURL string) (APIMode, string) {
	family := opencodeFamilyForCustom(provider, baseURL)
	if family == "" {
		return "", baseURL
	}
	if entry := opencodeNamedCustomEntryFn(ctx, provider); entry != nil && auxTruthy(entry["api_mode"]) {
		return "", baseURL
	}
	mode := OpenCodeModelMode(family, NormalizeOpenCodeModelID(provider, model))
	return mode, NormalizeOpenCodeBaseURL(provider, mode, baseURL)
}

// IsOpencodeTarget reports whether the provider or base URL addresses the OpenCode relay
func IsOpencodeTarget(provider, baseURL string) bool {
	if OpenCodeFamily(provider) != "" {
		return true
	}
	return isOpenCodeEndpoint(baseURL)
}

// ResolveAffinityKey is the rotation stable conversation affinity key, empty when nothing names the conversation
func ResolveAffinityKey(ctx context.Context, sessionID string) string {
	return cacheScopeFromSessionID(auxFirstNonEmpty(portalAffinityScope(ctx), portalConversationContext(ctx), sessionID))
}

// OpencodeSessionHeaders is the session header for OpenCode targets, an ephemeral oneshot key when none resolves
func OpencodeSessionHeaders(ctx context.Context, provider, baseURL, sessionID string) map[string]string {
	if !IsOpencodeTarget(provider, baseURL) {
		return map[string]string{}
	}
	key := ResolveAffinityKey(ctx, sessionID)
	if key == "" {
		key = "oneshot-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	}
	return map[string]string{OpencodeSessionHeader: key}
}

// CustomProviderSessionAffinityHeaders is the opted in affinity header of the route matching entry, empty when none
func CustomProviderSessionAffinityHeaders(ctx context.Context, baseURL, sessionID string) map[string]string {
	header := ""
	if err := auxRecoverRaise(func() {
		tree, err := auxLoadConfigFn(ctx)
		if err != nil {
			panic(err)
		}
		header = CustomProviderSessionAffinityHeader(baseURL, nil, tree)
	}); err != nil || header == "" {
		return map[string]string{}
	}
	if key := ResolveAffinityKey(ctx, sessionID); key != "" {
		return map[string]string{header: key}
	}
	return map[string]string{}
}

// MergeSessionAffinityHeaders adds the affinity headers under kwargs extra_headers in place, caller pinned values winning
func MergeSessionAffinityHeaders(ctx context.Context, kwargs map[string]any, provider, baseURL, sessionID string) map[string]any {
	headers := OpencodeSessionHeaders(ctx, provider, baseURL, sessionID)
	for k, v := range CustomProviderSessionAffinityHeaders(ctx, baseURL, sessionID) {
		headers[k] = v
	}
	if len(headers) == 0 {
		return kwargs
	}
	merged := map[string]any{}
	switch existing := kwargs["extra_headers"].(type) {
	case map[string]any:
		for k, v := range existing {
			merged[k] = v
		}
	case map[string]string:
		for k, v := range existing {
			merged[k] = v
		}
	}
	for k, v := range headers {
		if _, ok := merged[k]; !ok {
			merged[k] = v
		}
	}
	kwargs["extra_headers"] = merged
	return kwargs
}
