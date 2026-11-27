package provider

import (
	"context"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/provider"
)

// Where an OpenCode Go subscription's windows are read
const goUsageURL = "https://opencode.ai/zen/go/v1/usage"

// Windows of an OpenCode Go subscription: its rolling window, its week and its month
func goUsage(ctx context.Context, route provider.Route) (*provider.AccountUsage, error) {
	token := strings.TrimSpace(route.APIKey)
	if token == "" {
		return nil, nil
	}
	payload, err := models.Request(ctx, "GET", goUsageURL, map[string]string{"Authorization": "Bearer " + token, "Accept": "application/json"}, nil,
		10*time.Second)
	if err != nil {
		return nil, err
	}
	usage, _ := payload["usage"].(map[string]any)
	var windows []provider.UsageWindow
	for _, window := range [][2]string{{"rolling", "Rolling window"}, {"weekly", "Weekly"}, {"monthly", "Monthly"}} {
		entry, _ := usage[window[0]].(map[string]any)
		used, ok := entry["percent"].(float64)
		if !ok {
			continue
		}
		var resets *time.Time
		if raw, _ := entry["resetsAt"].(string); raw != "" {
			if at, err := time.Parse(time.RFC3339Nano, raw); err == nil {
				resets = &at
			}
		}
		windows = append(windows, provider.AccountWindow(window[1], &used, resets, ""))
	}
	reading := provider.NewAccountUsage(route.Provider, "go_usage_api", windows, nil)
	// The subscription is sold by these windows, shown as bars
	reading.Bars = true
	return reading, nil
}
