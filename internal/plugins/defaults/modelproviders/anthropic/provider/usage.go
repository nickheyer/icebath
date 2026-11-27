package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/provider"
)

// Where a Claude subscription's limits are read
const usageURL = "https://api.anthropic.com/api/oauth/usage"

// Windows of a Claude account signed in with OAuth: its session, its week and the weeks of single model families, then what it may spend
// past them; none for an API key, whose limits this can't read
func accountUsage(ctx context.Context, route provider.Route) (*provider.AccountUsage, error) {
	token := strings.TrimSpace(route.APIKey)
	if token == "" {
		return nil, nil
	}
	if !oauthToken(token) {
		reading := provider.NewAccountUsage(route.Provider, "oauth_usage_api", nil, nil)
		reading.Unavailable = "Anthropic account limits are only available for OAuth-backed Claude accounts."
		return reading, nil
	}
	headers := map[string]string{"Authorization": "Bearer " + token, "Accept": "application/json", "Content-Type": "application/json",
		"anthropic-beta": "oauth-2025-04-20", "User-Agent": "claude-code/2.1.0"}
	payload, err := models.Request(ctx, "GET", usageURL, headers, nil, 15*time.Second)
	if err != nil {
		return nil, err
	}
	var windows []provider.UsageWindow
	for _, window := range [][2]string{{"five_hour", "Current session"}, {"seven_day", "Current week"}, {"seven_day_opus", "Opus week"},
		{"seven_day_sonnet", "Sonnet week"}} {
		entry, _ := payload[window[0]].(map[string]any)
		used, ok := entry["utilization"].(float64)
		if !ok {
			continue
		}
		if used <= 1 {
			used *= 100
		}
		w := provider.AccountWindow(window[1], &used, resetTime(entry["resets_at"]), "")
		w.Model = strings.HasPrefix(window[0], "seven_day_")
		windows = append(windows, w)
	}
	var details []string
	extra, _ := payload["extra_usage"].(map[string]any)
	spent, hasSpent := extra["used_credits"].(float64)
	limit, hasLimit := extra["monthly_limit"].(float64)
	if enabled, _ := extra["is_enabled"].(bool); enabled && hasSpent && hasLimit {
		currency, _ := extra["currency"].(string)
		if currency == "" {
			currency = "USD"
		}
		details = append(details, fmt.Sprintf("Extra usage: %.2f / %.2f %s", spent, limit, currency))
	}
	return provider.NewAccountUsage(route.Provider, "oauth_usage_api", windows, details), nil
}

// Whether a credential is an OAuth or setup token rather than a Console API key
func oauthToken(key string) bool {
	for _, prefix := range []string{"sk-ant-api", "sk-ant-usr", "sk-ant-svc", "sk-ant-admin"} {
		if strings.HasPrefix(key, prefix) {
			return false
		}
	}
	return strings.HasPrefix(key, "sk-ant-") || strings.HasPrefix(key, "eyJ") || strings.HasPrefix(key, "cc-")
}

// Time a window resets, from seconds since the epoch or ISO text; nil when not known
func resetTime(value any) *time.Time {
	switch v := value.(type) {
	case float64:
		at := time.Unix(0, int64(v*1e9)).UTC()
		return &at
	case string:
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999", "2006-01-02T15:04:05"} {
			if at, err := time.Parse(layout, strings.TrimSpace(v)); err == nil {
				return &at
			}
		}
	}
	return nil
}
