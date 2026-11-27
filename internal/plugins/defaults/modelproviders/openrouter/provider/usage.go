package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/provider"
)

// Credit balance of the account, the quota of the route's key and what the key spent
func accountUsage(ctx context.Context, route provider.Route) (*provider.AccountUsage, error) {
	token, base := strings.TrimSpace(route.APIKey), strings.TrimRight(route.BaseURL, "/")
	if token == "" {
		return nil, nil
	}
	headers := map[string]string{"Authorization": "Bearer " + token, "Accept": "application/json"}
	data := func(path string) (map[string]any, error) {
		body, err := models.Request(ctx, "GET", base+"/"+path, headers, nil, 10*time.Second)
		if err != nil {
			return nil, err
		}
		inner, _ := body["data"].(map[string]any)
		return inner, nil
	}
	credits, err := data("credits")
	if err != nil {
		return nil, err
	}
	key, err := data("key")
	if err != nil {
		key = nil
	}
	total, _ := credits["total_credits"].(float64)
	spent, _ := credits["total_usage"].(float64)
	details := []string{fmt.Sprintf("Credits balance: $%.2f", max(total-spent, 0))}
	var windows []provider.UsageWindow
	limit, hasLimit := key["limit"].(float64)
	left, hasLeft := key["limit_remaining"].(float64)
	if hasLimit && hasLeft && limit > 0 && 0 <= left && left <= limit {
		parts := []string{fmt.Sprintf("$%.2f of $%.2f remaining", left, limit)}
		if reset, _ := key["limit_reset"].(string); strings.TrimSpace(reset) != "" {
			parts = append(parts, "resets "+strings.TrimSpace(reset))
		}
		used := (limit - left) / limit * 100
		windows = append(windows, provider.AccountWindow("API key quota", &used, nil, strings.Join(parts, " • ")))
	}
	if usage, ok := key["usage"].(float64); ok {
		parts := []string{fmt.Sprintf("API key usage: $%.2f total", usage)}
		for _, period := range [][2]string{{"usage_daily", "today"}, {"usage_weekly", "this week"}, {"usage_monthly", "this month"}} {
			if v, ok := key[period[0]].(float64); ok && v > 0 {
				parts = append(parts, fmt.Sprintf("$%.2f %s", v, period[1]))
			}
		}
		details = append(details, strings.Join(parts, " • "))
	}
	return provider.NewAccountUsage(route.Provider, "credits_api", windows, details), nil
}
