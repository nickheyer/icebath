package provider

import (
	"context"

	"github.com/nickheyer/icebath/pkg/observability"
)

// Installs the local endpoint and named custom provider readers the shared metrics install snapshot calls
func init() {
	observability.IsLocalEndpointHook = IsLocalEndpoint
	observability.NamedCustomProviderHook = namedCustomProviderEntry
}

// Providers or custom_providers entry a name resolves to for the context home with credentials, nil when none
func namedCustomProviderEntry(ctx context.Context, name string) any {
	if entry := modelsResolver(ctx).getNamedCustomProvider(name, nil, true); entry != nil {
		return entry
	}
	return nil
}
