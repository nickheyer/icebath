package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Multiplex runtime scope of an API request profile, the default profile's under multiplex when unset
func apiProfileScope(ctx context.Context, profile string) (context.Context, error) {
	if profile == "" {
		if provider.IsMultiplexActive(ctx) {
			return profileRuntimeScope(ctx, home.Dir(ctx), true), nil
		}
		return ctx, nil
	}
	dir, err := profiles.GetProfileDir(profile)
	if err != nil {
		return nil, err
	}
	return profileRuntimeScope(ctx, dir, true), nil
}

// Execution policy and catalog the API server adapter advertises for one served profile
func localRoomCatalog(ctx context.Context, adapter PlatformAdapter, profile, installationID string) (*jsonutil.Object, *jsonutil.Object, error) {
	if adapter == nil || adapter.Base() == nil || adapter.Base().Platform != PlatformAPIServer {
		return nil, nil, &errorutil.Error{Kind: "AttributeError", Msg: "the API server adapter has no profile scope"}
	}
	scoped, err := apiProfileScope(ctx, profile)
	if err != nil {
		return nil, nil, err
	}
	policy, err := ExecutionPolicyMapping(scoped, profile, nil)
	if err != nil {
		return nil, nil, err
	}
	text := true
	catalog, err := CatalogMapping(ctx, CatalogOptions{InstallationID: installationID,
		ProtocolVersions: []any{int64(HostedProtocolVersion)}, LinkModes: []string{"direct"}, PersistentProcess: true,
		Text: &text, Attachments: true, TargetProfile: profile, ExecutionPolicy: policy})
	if err != nil {
		return nil, nil, err
	}
	return policy, catalog, nil
}
