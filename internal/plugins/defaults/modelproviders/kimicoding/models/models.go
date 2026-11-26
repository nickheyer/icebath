package models

import (
	"cmp"
	"context"
	"net/url"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/models"
	kimicodingconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/kimicoding/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type catalog struct {
	name string
	base string
}

func Register(registry *models.Registry) {
	registry.Register(catalog{name: kimicodingconfig.GlobalName, base: kimicodingconfig.GlobalEndpoint})
	registry.Register(catalog{name: kimicodingconfig.CNName, base: kimicodingconfig.CNEndpoint})
}

func (c catalog) Models(ctx context.Context, route provider.Route) ([]string, bool) {
	if route.Spec != c.name {
		return nil, false
	}
	target, confirmed := endpoint(cmp.Or(route.BaseURL, c.base))
	ids, ok := models.List(ctx, route, target)
	if !ok || confirmed {
		return ids, ok
	}
	kept := []string{}
	for _, id := range ids {
		if strings.ToLower(strings.TrimSpace(id)) != "k3" {
			kept = append(kept, id)
		}
	}
	return kept, true
}

func endpoint(base string) (string, bool) {
	effective := strings.TrimRight(base, "/")
	address, err := url.Parse(effective)
	confirmed := err == nil && coding(address)
	if confirmed && strings.TrimRight(address.Path, "/") == "/coding" {
		effective += "/v1"
	}
	return strings.TrimRight(strings.TrimSpace(effective), "/") + "/models", confirmed
}

func coding(address *url.URL) bool {
	if port := address.Port(); port != "" {
		if number, err := strconv.Atoi(port); err != nil || number != 443 {
			return false
		}
	}
	path := strings.TrimRight(address.Path, "/")
	return strings.ToLower(address.Scheme) == "https" && strings.ToLower(address.Hostname()) == "api.kimi.com" && address.User == nil &&
		(path == "/coding" || path == "/coding/v1") && address.RawQuery == "" && address.Fragment == ""
}
