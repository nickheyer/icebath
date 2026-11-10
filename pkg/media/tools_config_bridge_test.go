package media

import (
	"context"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestToolsConfigBridgeExposesTheMediaCatalogs(t *testing.T) {
	hooks := tools.ToolsConfigHooks
	local, ok := hooks.STTModelCatalog("local")
	if !ok || !slices.Equal(local, sttModelCatalog["local"]) {
		t.Fatalf("local catalog %v", local)
	}
	local[0] = "mutated"
	if sttModelCatalog["local"][0] == "mutated" {
		t.Fatal("catalog handed out by reference")
	}
	if _, ok := hooks.STTModelCatalog("deepinfra"); ok {
		t.Fatal("deepinfra has no static catalog")
	}
	if key, ok := hooks.STTModelConfigKey("elevenlabs"); !ok || key != "model_id" {
		t.Fatalf("elevenlabs key %q", key)
	}
	if _, ok := hooks.STTModelConfigKey("groq"); ok {
		t.Fatal("groq has no custom model key")
	}
	if hooks.STTDefaultLocalModel() != sttDefaultLocalModel {
		t.Fatal("default local model")
	}
	ids, catalog, def := hooks.FALModels()
	if !slices.Equal(ids, falModelOrder) || def != falDefaultModel || len(catalog) != len(falModels) {
		t.Fatalf("fal models %v %s", ids, def)
	}
	ctx := context.Background()
	env := &tools.Env{Home: t.TempDir()}
	for name, reg := range map[string]*tools.PickerRegistry{"image": hooks.ImageGenRegistry, "video": hooks.VideoGenRegistry, "tts": hooks.TTSRegistry} {
		if reg == nil || reg.Get(ctx, env, "no-such-provider") != nil {
			t.Fatalf("%s registry view wrong", name)
		}
	}
	if len(hooks.ImageGenRegistry.List(ctx, env)) != len(ListImageGenProviders(ctx, "")) ||
		len(hooks.VideoGenRegistry.List(ctx, env)) != len(ListVideoGenProviders(ctx, "")) ||
		len(hooks.TTSRegistry.List(ctx, env)) != len(ListTTSProviders(ctx, "")) {
		t.Fatal("registry listings differ")
	}
	if !hooks.TTSRegistry.BuiltinNames["edge"] || hooks.ImageGenRegistry.BuiltinNames != nil {
		t.Fatal("builtin names wrong")
	}
	if hooks.DeepInfraModelIDs == nil || hooks.ManagedImageCatalog == nil {
		t.Fatal("catalog lookups not installed")
	}
}
