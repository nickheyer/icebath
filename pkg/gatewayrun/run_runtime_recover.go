package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Recovers every serving authority against the published routing index and the adapters connected now
func recoverGatewayNativeSessions(ctx context.Context, r *GatewayRunner) (*jsonutil.Object, error) {
	results := jsonutil.NewObject()
	authorities := gateway.ServingAuthorities(r)
	if len(authorities) == 0 {
		return results, nil
	}
	if _, err := gateway.EnsureHostedService(ctx, r); err != nil {
		return nil, err
	}
	for _, authority := range authorities {
		scoped := gateway.OwnerScope(ctx, authority, false)
		if err := gateway.RecoverLocalSessions(scoped, authority, true); err != nil {
			return nil, err
		}
		if _, err := gateway.RecoverWebhookFinalizations(scoped, authority); err != nil {
			return nil, err
		}
		outcome, err := gateway.RecoverProfileNativeSessions(ctx, r, authority)
		if err != nil {
			return nil, err
		}
		for _, sid := range outcome.Keys() {
			results.Set(sid, outcome.Value(sid))
		}
	}
	return results, nil
}
