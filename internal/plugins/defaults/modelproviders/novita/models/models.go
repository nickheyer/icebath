package models

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/models"
	novitaconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/novita/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type prices struct{}

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: novitaconfig.Name})
	registry.Register(prices{})
}

// Answered by the listing registered beside it
func (prices) Models(context.Context, provider.Route) ([]string, bool) {
	return nil, false
}

// Prices NovitaAI lists for the key, given for a million tokens in units of a hundredth of a cent
func (prices) Prices(ctx context.Context, env *models.Env) (map[string]provider.Price, bool) {
	route := env.Route
	if route.Spec != novitaconfig.Name || strings.TrimSpace(route.APIKey) == "" {
		return nil, false
	}
	data, err := models.Request(ctx, "GET", strings.TrimRight(route.BaseURL, "/")+"/models", map[string]string{"Authorization": "Bearer " + route.APIKey,
		"Accept": "application/json", "User-Agent": provider.UserAgent("icebath-cli")}, nil, 8*time.Second)
	out := map[string]provider.Price{}
	if err != nil {
		return out, true
	}
	items, _ := data["data"].([]any)
	for _, item := range items {
		row, _ := item.(map[string]any)
		id, _ := row["id"].(string)
		input, hasInput := row["input_token_price_per_m"].(float64)
		output, hasOutput := row["output_token_price_per_m"].(float64)
		if id == "" || !hasInput && !hasOutput {
			continue
		}
		out[id] = provider.Price{Input: strconv.FormatFloat(input/10_000, 'f', -1, 64), Output: strconv.FormatFloat(output/10_000, 'f', -1, 64)}
	}
	return out, true
}
