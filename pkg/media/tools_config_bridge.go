package media

import (
	"context"
	"slices"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Installs the STT, image catalog and media registry facts the tools picker reads
func init() {
	hooks := &tools.ToolsConfigHooks
	hooks.STTModelCatalog = func(provider string) ([]string, bool) {
		models, ok := sttModelCatalog[provider]
		return slices.Clone(models), ok
	}
	hooks.STTModelConfigKey = func(provider string) (string, bool) {
		key, ok := sttModelConfigKey[provider]
		return key, ok
	}
	hooks.STTDefaultLocalModel = func() string { return sttDefaultLocalModel }
	hooks.DeepInfraModelIDs = deepInfraModelIDs
	hooks.FALModels = func() ([]string, map[string]map[string]any, string) {
		return slices.Clone(falModelOrder), falModels, falDefaultModel
	}
	hooks.ManagedImageCatalog = managedImageCatalog
	hooks.ImageGenRegistry = &tools.PickerRegistry{
		List: func(ctx context.Context, _ *tools.Env) []tools.PickerProvider {
			return pickerProviders(ListImageGenProviders(ctx, ""))
		},
		Get: func(ctx context.Context, _ *tools.Env, name string) tools.PickerProvider {
			if p := GetImageGenProvider(ctx, name, ""); p != nil {
				return p
			}
			return nil
		},
	}
	hooks.VideoGenRegistry = &tools.PickerRegistry{
		List: func(ctx context.Context, _ *tools.Env) []tools.PickerProvider {
			return pickerProviders(ListVideoGenProviders(ctx, ""))
		},
		Get: func(ctx context.Context, _ *tools.Env, name string) tools.PickerProvider {
			if p := GetVideoGenProvider(ctx, name, ""); p != nil {
				return p
			}
			return nil
		},
	}
	hooks.TTSRegistry = &tools.PickerRegistry{
		List: func(ctx context.Context, _ *tools.Env) []tools.PickerProvider {
			return pickerProviders(ListTTSProviders(ctx, ""))
		},
		Get: func(ctx context.Context, _ *tools.Env, name string) tools.PickerProvider {
			if p := GetTTSProvider(ctx, name, ""); p != nil {
				return p
			}
			return nil
		},
		BuiltinNames: ttsRegistryBuiltinNames,
	}
}

// Registry providers as the picker lists them, order kept
func pickerProviders[P tools.PickerProvider](list []P) []tools.PickerProvider {
	out := make([]tools.PickerProvider, len(list))
	for i, p := range list {
		out[i] = p
	}
	return out
}
