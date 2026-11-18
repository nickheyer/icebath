package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Skin payload of the active profile config, the gateway.ready and skin.changed skin
func resolveSkin(ctx context.Context) map[string]any {
	return tuihost.SkinPayload(ctx, loadCfg(ctx))
}

// Profile home whose skin a peer follows, nil for the launch home
func skinHomeFor(ctx context.Context, profileID string) *string {
	if h := tuihost.SkinHomeFor(profileID); h != "" {
		return &h
	}
	return nil
}
