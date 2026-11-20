//go:build !icebath_core

package app

import (
	"context"

	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Binds the image and video generation providers the setup summary probes to the media registries
func init() {
	setupImageGenProviders = func(ctx context.Context) []provider.CatalogProviderBase {
		var out []provider.CatalogProviderBase
		for _, p := range media.ListImageGenProviders(ctx, "") {
			out = append(out, p)
		}
		return out
	}
	setupVideoGenProviders = func(ctx context.Context) []provider.CatalogProviderBase {
		var out []provider.CatalogProviderBase
		for _, p := range media.ListVideoGenProviders(ctx, "") {
			out = append(out, p)
		}
		return out
	}
}
