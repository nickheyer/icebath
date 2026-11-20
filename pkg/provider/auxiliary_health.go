package provider

import (
	"context"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Unhealthy cache key scoped to the active profile home, endpoint specific for an explicit custom endpoint
type auxUnhealthyKey struct {
	Home  string
	Scope string
	Name  string
}

// Provider wide or custom endpoint key under the active home so one profile's 402 never hides another's provider
func auxUnhealthyCacheKey(ctx context.Context, provider, baseURL string) auxUnhealthyKey {
	label := auxNormalizeChainLabel(provider)
	endpoint := NormalizeRouteBaseURL(auxCustomHealthBaseURL(ctx, provider, baseURL))
	hk := homeKey(home.Dir(ctx))
	if endpoint != "" {
		return auxUnhealthyKey{Home: hk, Scope: "custom-endpoint", Name: endpoint}
	}
	return auxUnhealthyKey{Home: hk, Name: label}
}

// Concrete custom endpoint that scopes health and failed route checks, empty for non custom providers
func auxCustomHealthBaseURL(ctx context.Context, provider, explicitBaseURL string) string {
	explicit := strings.TrimSpace(explicitBaseURL)
	label := auxNormalizeChainLabel(provider)
	if label == "local/custom" {
		if explicit != "" {
			return explicit
		}
		return auxCurrentCustomBaseURL(ctx)
	}
	if strings.HasPrefix(label, "custom:") && explicit != "" {
		return explicit
	}
	r := modelsResolver(ctx)
	if r.resolvesToCustom(label) {
		if explicit != "" {
			return explicit
		}
		return auxCurrentCustomBaseURL(ctx)
	}
	if entry := r.getNamedCustomProvider(provider, r.cfg, true); entry != nil {
		if explicit != "" {
			return explicit
		}
		if v := entry["base_url"]; auxTruthy(v) {
			return strings.TrimSpace(auxPyStr(v))
		}
		return ""
	}
	return ""
}

// Fallback reason label naming why a candidate cannot serve this walk, auth errors excluded
func auxFallbackCandidateUnavailableReason(err error) (string, bool) {
	for _, r := range auxFallbackReasons {
		if r.Label != "auth error" && r.Predicate(err) {
			return r.Label, true
		}
	}
	return "", false
}

const auxTransientCandidateQuarantine = 60 * time.Second

var auxCandidateQuarantineTTL = map[string]time.Duration{
	"rate limit":                auxTransientCandidateQuarantine,
	"connection error":          auxTransientCandidateQuarantine,
	"invalid provider response": auxTransientCandidateQuarantine,
}

// Quarantine hold for a fallback reason label, false meaning the long default TTL
func auxFallbackCandidateQuarantineTTL(reason string) (time.Duration, bool) {
	d, ok := auxCandidateQuarantineTTL[reason]
	return d, ok
}
