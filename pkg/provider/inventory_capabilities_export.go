package provider

import "context"

// Attaches the capability map of every model per picker row, metadata config nil when the caller has none
func ApplyCapabilities(ctx context.Context, rows []map[string]any, metadataConfig map[string]any) error {
	return applyCapabilities(ctx, rows, metadataConfig)
}
