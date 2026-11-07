package media

import (
	"context"
	"fmt"
	"log/slog"
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

var ttsRegistryLog = slog.Default().With("logger", "agent.tts_registry")

// Warns that a plugin tried to take a built in TTS name and was ignored
func warnTTSBuiltinCollision(key string) error {
	names := make([]string, 0, len(ttsRegistryBuiltinNames))
	for n := range ttsRegistryBuiltinNames {
		names = append(names, n)
	}
	sort.Strings(names)
	ttsRegistryLog.Warn(fmt.Sprintf("TTS provider '%s' shadows a built-in name; registration ignored. "+
		"Built-in TTS providers (%s) always win — pick a different name.", key, strings.Join(names, ", ")))
	return nil
}

var ttsRegistry = provider.NewProviderRegistry[TTSProvider](provider.ProviderRegistryOptions{
	Label: "TTS", Logger: ttsRegistryLog, Normalize: provider.LowerKey,
	BuiltinNames: ttsRegistryBuiltinNames, OnBuiltinCollision: warnTTSBuiltinCollision,
})

// Registers a plugin TTS provider, a built in name ignored with a warning
func RegisterTTSProvider(p TTSProvider, scope string) error {
	return ttsRegistry.Register(p, scope)
}

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

// Plugin facing registry that register_tts_provider writes into, adapting hosted Python providers
func TTSScopedRegistry() plugins.ScopedRegistry {
	return hostedTTSScopedRegistry{ttsRegistry.Scoped()}
}

// Scoped registry view that wraps plugin host objects as TTSProvider values before delegating
type hostedTTSScopedRegistry struct{ inner plugins.ScopedRegistry }

func (s hostedTTSScopedRegistry) Accepts(p any) bool {
	return s.inner.Accepts(adaptHostedTTSProvider(p))
}
func (s hostedTTSScopedRegistry) ProviderName(p any) string {
	return s.inner.ProviderName(adaptHostedTTSProvider(p))
}
func (s hostedTTSScopedRegistry) Identity(p any) error {
	return s.inner.Identity(adaptHostedTTSProvider(p))
}
func (s hostedTTSScopedRegistry) Snapshot(name, scope string) any {
	return s.inner.Snapshot(name, scope)
}
func (s hostedTTSScopedRegistry) Register(p any, scope string) (bool, error) {
	return s.inner.Register(adaptHostedTTSProvider(p), scope)
}
func (s hostedTTSScopedRegistry) Restore(name string, current, replacement any, scope string) bool {
	return s.inner.Restore(name, adaptHostedTTSProvider(current), adaptHostedTTSProvider(replacement), scope)
}
