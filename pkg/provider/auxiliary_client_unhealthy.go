package provider

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Default seconds a provider endpoint stays hidden after a failure
const auxUnhealthyTTLSeconds = 600.0

// Reason logged and echoed when a mark gives none
const auxUnhealthyPaymentReason = "payment / credit error"

// Resolved provider and explicit config names mapped to chain labels
var auxUnhealthyLabelAliases = map[string]string{
	"openrouter": "openrouter", "nous": "nous", "custom": "local/custom",
	"local/custom": "local/custom", "openai-codex": "openai-codex", "codex": "openai-codex",
}

// Expiry, last skip log time and reason of every hidden provider endpoint
var (
	auxUnhealthyMu       sync.Mutex
	auxUnhealthyUntil    = map[auxUnhealthyKey]float64{}
	auxUnhealthyLoggedAt = map[auxUnhealthyKey]float64{}
	auxUnhealthyReason   = map[auxUnhealthyKey]string{}
)

// Wall clock seam of the unhealthy cache in epoch seconds
var auxUnhealthyNow = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Chain label of a resolved provider, unknown providers lowercased
func auxNormalizeChainLabel(provider string) string {
	if provider == "" {
		return ""
	}
	p := strings.ToLower(strings.TrimSpace(provider))
	if label, ok := auxUnhealthyLabelAliases[p]; ok {
		return label
	}
	return p
}

// Hides one provider endpoint for ttl seconds and logs the reason at the given level
func auxMarkProviderUnhealthy(ctx context.Context, provider string, ttl float64, baseURL, reason string, level slog.Level) {
	label := auxNormalizeChainLabel(provider)
	if label == "" {
		return
	}
	key := auxUnhealthyCacheKey(ctx, label, baseURL)
	expiresAt := auxUnhealthyNow() + ttl
	auxUnhealthyMu.Lock()
	auxUnhealthyUntil[key] = expiresAt
	auxUnhealthyReason[key] = reason
	auxUnhealthyMu.Unlock()
	clock := time.Unix(0, int64(expiresAt*1e9)).Local().Format("15:04:05")
	slog.Log(ctx, level, fmt.Sprintf("Auxiliary: marking %s unhealthy for %ds (%s). "+
		"Subsequent auxiliary calls will skip it until %s.", label, int(ttl), reason, clock))
}

// True while the endpoint is hidden, expired entries dropped on read
func auxIsProviderUnhealthy(ctx context.Context, label, baseURL string) bool {
	if label == "" {
		return false
	}
	key := auxUnhealthyCacheKey(ctx, label, baseURL)
	auxUnhealthyMu.Lock()
	defer auxUnhealthyMu.Unlock()
	expiresAt, ok := auxUnhealthyUntil[key]
	if !ok {
		return false
	}
	if auxUnhealthyNow() >= expiresAt {
		delete(auxUnhealthyUntil, key)
		delete(auxUnhealthyLoggedAt, key)
		delete(auxUnhealthyReason, key)
		return false
	}
	return true
}

// Logs a skipped hidden provider at most once a minute per endpoint
func auxLogSkipUnhealthy(ctx context.Context, label, task, baseURL string) {
	now := auxUnhealthyNow()
	key := auxUnhealthyCacheKey(ctx, label, baseURL)
	auxUnhealthyMu.Lock()
	if now-auxUnhealthyLoggedAt[key] < 60 {
		auxUnhealthyMu.Unlock()
		return
	}
	auxUnhealthyLoggedAt[key] = now
	expiresAt, ok := auxUnhealthyUntil[key]
	if !ok {
		expiresAt = now
	}
	reason, ok := auxUnhealthyReason[key]
	if !ok {
		reason = auxUnhealthyPaymentReason
	}
	auxUnhealthyMu.Unlock()
	if task == "" {
		task = "call"
	}
	slog.Info(fmt.Sprintf("Auxiliary %s: skipping %s (%s, retry in %ds)", task, label, reason, max(0, int(expiresAt-now))))
}

// Clears every hidden provider endpoint
func auxResetAuxUnhealthyCache() {
	auxUnhealthyMu.Lock()
	defer auxUnhealthyMu.Unlock()
	clear(auxUnhealthyUntil)
	clear(auxUnhealthyLoggedAt)
	clear(auxUnhealthyReason)
}
