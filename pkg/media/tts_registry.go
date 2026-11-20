package media

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Names reserved for the native TTS handlers, kept equal to builtinTTSProviders by TestTTSBuiltinSync
var ttsRegistryBuiltinNames = map[string]bool{
	"edge": true, "elevenlabs": true, "openai": true, "minimax": true, "xai": true, "mistral": true, "gemini": true,
	"neutts": true, "kittentts": true, "piper": true, "deepinfra": true,
}

// Warns that a plugin tried to take a built in TTS name and was ignored
func warnTTSBuiltinCollision(key string) error {
	names := make([]string, 0, len(ttsRegistryBuiltinNames))
	for n := range ttsRegistryBuiltinNames {
		names = append(names, n)
	}
	sort.Strings(names)
	pkgLog().Warn(fmt.Sprintf("TTS provider '%s' shadows a built-in name; registration ignored. "+
		"Built-in TTS providers (%s) always win — pick a different name.", key, strings.Join(names, ", ")))
	return nil
}

var ttsRegistry = provider.NewProviderRegistry[TTSProvider](provider.ProviderRegistryOptions{
	Label: "TTS", Logger: pkgLog(), Normalize: provider.LowerKey,
	BuiltinNames: ttsRegistryBuiltinNames, OnBuiltinCollision: warnTTSBuiltinCollision,
})

// Registered TTS providers sorted by name
func ListTTSProviders(ctx context.Context, scope string) []TTSProvider {
	return ttsRegistry.ListProviders(ctx, scope)
}

// Provider registered under a string name, scoped registration first, nil for a non string or absent name
func GetTTSProvider(ctx context.Context, name any, scope string) TTSProvider {
	s, ok := name.(string)
	if !ok {
		return nil
	}
	return ttsRegistry.GetProvider(ctx, s, scope)
}

// Plugin facing registry that register_tts_provider writes into, a plugin host object stored as the provider adapted from it
func TTSScopedRegistry() plugins.ScopedRegistry {
	return plugins.HostedScopedRegistry{Inner: ttsRegistry.Scoped(), Adapt: adaptHostedTTSProvider,
		Object: func(held any) (*plugins.HostedObject, bool) {
			h, ok := held.(hostedTTSProvider)
			return h.obj, ok
		}}
}
