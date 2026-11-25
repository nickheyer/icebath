package models

import (
	"context"
	"sync"

	"github.com/nickheyer/icebath/internal/models"
	openrouterconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openrouter/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type catalog struct {
	mu  sync.Mutex
	ids []string
	set bool
}

func Register(registry *models.Registry) {
	registry.Register(new(catalog))
}

func (c *catalog) Models(ctx context.Context, route provider.Route) ([]string, bool) {
	if route.Provider != openrouterconfig.Name {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.set {
		return c.ids, true
	}
	public := route
	public.APIKey = ""
	ids, ok := models.List(ctx, public, openrouterconfig.ModelsURL)
	if ok {
		c.ids, c.set = ids, true
	}
	return ids, ok
}
