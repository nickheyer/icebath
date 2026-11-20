package state

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// SetSessionModel sets the model after a mid session switch and drops the browser lock, a provider writing the whole route in both shapes
func (d *SessionDB) SetSessionModel(ctx context.Context, sessionID, model, provider, baseURL, apiMode string) error {
	d.FlushTokenCounts(5.0)
	patch := pyjson.NewObject()
	patch.Set("browser_model_lock", nil)
	if model != "" {
		patch.Set("model", model)
	}
	if provider != "" {
		route := pyjson.NewObject()
		route.Set("provider", provider)
		route.Set("base_url", orNone(baseURL))
		route.Set("api_mode", orNone(apiMode))
		patch.Update(route)
		patch.Set("gateway_runtime", route)
	}
	return d.writeModelConfigPatch(ctx, sessionID, patch, "UPDATE sessions SET model = ?, model_config = ? WHERE id = ?",
		func(merged any) []any { return []any{model, merged, sessionID} })
}
