package provider

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// VisionCapabilities serves the tools vision probes from the profile registry, the models.dev catalog and the local runtime
type VisionCapabilities struct{}

var _ tools.VisionCapabilitySource = VisionCapabilities{}

func (VisionCapabilities) ProviderVisionProfile(provider string) (supportsVision, supportsVisionToolMessages, found bool) {
	p, ok := GetProviderProfile(context.Background(), provider)
	if !ok {
		return false, false, false
	}
	return p.Vision, p.VisionToolMessages, true
}

func (VisionCapabilities) IsRoutingAggregator(provider string) bool {
	return IsRoutingAggregator(provider)
}

// Catalog verdict fetched with network allowed, unknown when the catalog has no vision flag for the model
func (VisionCapabilities) CatalogSupportsVision(ctx context.Context, provider, model string) (supported, known bool) {
	caps, ok := ModelsDevCapabilities(ctx, provider, model, true)
	if !ok || caps.SupportsVision == nil {
		return false, false
	}
	return *caps.SupportsVision, true
}

func (VisionCapabilities) StripCodexContextVariantSuffix(model string) string {
	return StripCodexContextVariantSuffix(model)
}

func (VisionCapabilities) IsManagedEndpoint(baseURL string) bool { return IsManagedEndpoint(baseURL) }

func (VisionCapabilities) ManagedModelSupportsVision(ctx context.Context, model string) (supported, known bool) {
	v := ManagedModelSupportsVision(ctx, model)
	if v == nil {
		return false, false
	}
	return *v, true
}

func (VisionCapabilities) IsLocalEndpoint(baseURL string) bool { return IsLocalEndpoint(baseURL) }

func (VisionCapabilities) DetectLocalServerType(ctx context.Context, baseURL, apiKey string) string {
	return DetectLocalServerType(ctx, baseURL, apiKey)
}

func (VisionCapabilities) QueryOllamaSupportsVision(ctx context.Context, model, baseURL, apiKey string) (supported, known bool) {
	return QueryOllamaSupportsVision(ctx, model, baseURL, apiKey)
}
