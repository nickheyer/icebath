package models

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/models"
	kilocodeconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/kilocode/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type prices struct{}

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: kilocodeconfig.Name})
	registry.Register(prices{})
}

// Answered by the listing registered beside it
func (prices) Models(context.Context, provider.Route) ([]string, bool) {
	return nil, false
}

// Prices the Kilo gateway lists, nothing for the models it marks free; those it can't price are left out
func (prices) Prices(ctx context.Context, env *models.Env) (map[string]provider.Price, bool) {
	route := env.Route
	if route.Spec != kilocodeconfig.Name {
		return nil, false
	}
	headers := map[string]string{"Accept": "application/json", "User-Agent": provider.UserAgent("icebath-cli")}
	if key := strings.TrimSpace(route.APIKey); key != "" {
		headers["Authorization"] = "Bearer " + key
	}
	data, err := models.Request(ctx, "GET", strings.TrimSuffix(strings.TrimRight(route.BaseURL, "/"), "/v1")+"/models", headers, nil, 8*time.Second)
	out := map[string]provider.Price{}
	if err != nil {
		return out, true
	}
	items, _ := data["data"].([]any)
	for _, item := range items {
		row, _ := item.(map[string]any)
		id, _ := row["id"].(string)
		listed, ok := row["pricing"].(map[string]any)
		if id == "" || !ok {
			continue
		}
		r := models.Row{Pricing: map[string]string{"prompt": models.Scalar(listed["prompt"]), "completion": models.Scalar(listed["completion"]),
			"cache_read": models.Scalar(listed["input_cache_read"]), "cache_write": models.Scalar(listed["input_cache_write"])}}
		if free, _ := row["isFree"].(bool); free {
			r.Pricing = map[string]string{"prompt": "0", "completion": "0"}
		}
		if _, err := strconv.ParseFloat(r.Pricing["prompt"], 64); err != nil {
			continue
		}
		if _, err := strconv.ParseFloat(r.Pricing["completion"], 64); err != nil {
			continue
		}
		if price, ok := r.Price(); ok {
			out[id] = price
		}
	}
	return out, true
}
