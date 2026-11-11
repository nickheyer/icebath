package provider

import "context"

// LoadPickerContext reads the provider picker inputs of the active home config
func LoadPickerContext(ctx context.Context) (PickerConfigContext, error) { return loadPickerContext(ctx) }

// BuildModelsPayload is the picker payload of providers with their model lists
func BuildModelsPayload(ctx context.Context, pc PickerConfigContext, opts ModelsPayloadOptions) (map[string]any, error) {
	return buildModelsPayload(ctx, pc, opts)
}
