package provider

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Host and provider pair of a credential routing table
type auxHostProvider struct{ Host, Provider string }

// Hosts whose credential pool an auxiliary client recovers
var auxPoolProviderByHost = []auxHostProvider{
	{"chatgpt.com", "openai-codex"}, {"openrouter.ai", "openrouter"},
	{"inference-api.nousresearch.com", "nous"}, {"api.anthropic.com", "anthropic"},
	{"api.kimi.com", "kimi-coding"}, {"api.x.ai", "xai-oauth"},
}

// Hosts whose provider credentials an auth failure on an auto route refreshes
var auxAuthRefreshProviderByHost = []auxHostProvider{
	{"chatgpt.com", "openai-codex"},
	{"api.anthropic.com", "anthropic"}, {"inference-api.nousresearch.com", "nous"},
	{"api.x.ai", "xai-oauth"},
}

// First table provider whose host matches the base URL, else a profile claiming the host, empty when none
func auxProviderForHost(ctx context.Context, baseURL string, table []auxHostProvider) string {
	for _, row := range table {
		if baseURLHostMatches(baseURL, row.Host) {
			return row.Provider
		}
	}
	if p, ok := ProfileForHost(ctx, baseURL, false); ok {
		return p.Name
	}
	return ""
}

// Reports whether refreshed credentials carry a non blank API key
func auxCredsHaveAPIKey(creds map[string]any) bool {
	v := creds["api_key"]
	if !auxTruthy(v) {
		return false
	}
	return pystr.Strip(auxPyStr(v)) != ""
}
