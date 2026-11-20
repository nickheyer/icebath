package gateway

import "context"

// Adapter that renders the interactive provider then model drill down picker of the model command
type ModelPickerSender interface {
	SendModelPicker(ctx context.Context, chatID string, providers []map[string]any, currentModel, currentProvider, sessionKey string,
		onModelSelected func(ctx context.Context, chatID, modelID, providerSlug string) (string, error), metadata map[string]any) (SendResult, error)
}
