package models

import (
	"context"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/models"
	metaaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/metaai/config"
	"github.com/nickheyer/icebath/internal/provider"
)

var nonChat = []string{"muse-image-", "muse-voice-"}

type catalog struct{}

func Register(registry *models.Registry) {
	registry.Register(catalog{})
}

func (catalog) Models(ctx context.Context, route provider.Route) ([]string, bool) {
	if route.Spec != metaaiconfig.Name {
		return nil, false
	}
	live, ok := models.List(ctx, route, "")
	if !ok {
		return nil, false
	}
	ids := []string{}
	for _, id := range live {
		if !slices.ContainsFunc(nonChat, func(prefix string) bool { return strings.HasPrefix(id, prefix) }) {
			ids = append(ids, id)
		}
	}
	return ids, true
}
