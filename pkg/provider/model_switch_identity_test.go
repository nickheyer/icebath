package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

const identityKey = "test-profile-credential"

// Installs one home model-provider plugin dir whose loader yields the profile, mirrored into the auth registry
func installIdentityProfile(t *testing.T, ctx context.Context, p Profile, envVar string) {
	t.Helper()
	t.Setenv(envVar, identityKey)
	h := home.Dir(ctx)
	writeProviderPluginDir(t, h, p.Name)
	manifest := "name: " + p.Name + "\nkind: model-provider\nversion: 0.0.1\ndescription: switch fixture\n"
	if err := os.WriteFile(filepath.Join(h, "plugins", "model-providers", p.Name, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	p.AuthType, p.KeyEnv = "api_key", []string{envVar}
	fakeExternalProviderPlugins(t, map[string]func(string) ([]Profile, error){
		p.Name: func(string) ([]Profile, error) { return []Profile{p}, nil },
	})
	expireHomeLayerChecks()
	syncPluginProviderRegistry(ctx)
}

func identityHome(t *testing.T) context.Context {
	t.Helper()
	snapshotProviderRegistry(t)
	ctx, _ := switchAliasHome(t, "")
	fakeSwitchPipeline(t, true)
	return ctx
}

func TestAliasSwitchCarriesTheProfilesCanonicalIdentity(t *testing.T) {
	ctx := identityHome(t)
	installIdentityProfile(t, ctx, Profile{Name: "testgw", Aliases: []string{"testgw-alias"},
		BaseURL: "https://gw.example.test/v1", Mode: ModeAnthropicMessages}, "TESTGW_API_KEY")
	pdef := ResolveProviderFull(ctx, "testgw-alias", map[string]any{}, []any{})
	if pdef == nil || pdef.ID != "testgw" || pdef.Source != "plugin-profile" {
		t.Fatalf("pdef %+v", pdef)
	}
	res := mustSwitch(t, ctx, SwitchModelOptions{RawInput: "test-model", CurrentProvider: "openrouter",
		ExplicitProvider: "testgw-alias", UserProviders: map[string]any{}, CustomProviders: []any{}})
	if !res.Success {
		t.Fatal(res.ErrorMessage)
	}
	if res.TargetProvider != "testgw" || res.APIMode != string(ModeAnthropicMessages) || res.APIKey != identityKey {
		t.Fatalf("result %q %q %q", res.TargetProvider, res.APIMode, res.APIKey)
	}
	user := map[string]any{"testgw": map[string]any{"name": "Configured", "base_url": "https://cfg.example.test/v1",
		"key_env": "TESTGW_API_KEY"}}
	if got := ResolveProviderFull(ctx, "testgw-alias", user, nil); got == nil || got.Source != "user-config" {
		t.Fatalf("user block %+v", got)
	}
}

func TestRuntimeEndpointProfileIsAKnownProviderButPlaceholdersAreNot(t *testing.T) {
	ctx := identityHome(t)
	installIdentityProfile(t, ctx, Profile{Name: "mintgw", Mode: ModeChatCompletions}, "MINTGW_API_KEY")
	pdef := ResolveProviderFull(ctx, "mintgw", map[string]any{}, []any{})
	if pdef == nil || pdef.ID != "mintgw" || pdef.BaseURL != "" || pdef.Source != "plugin-profile" {
		t.Fatalf("pdef %+v", pdef)
	}
	res := mustSwitch(t, ctx, SwitchModelOptions{RawInput: "test-model", CurrentProvider: "openrouter",
		ExplicitProvider: "mintgw", UserProviders: map[string]any{}, CustomProviders: []any{}})
	if !res.Success || res.TargetProvider != "mintgw" || res.APIKey != identityKey {
		t.Fatalf("result %+v", res)
	}
	for _, placeholder := range []string{"custom", "ollama", "custom:nope"} {
		if got := ResolveProviderFull(ctx, placeholder, map[string]any{}, []any{}); got != nil {
			t.Fatalf("%s resolved to %+v", placeholder, got)
		}
	}
}

func TestPersistedAliasConfigAppliesToTheCanonicalRuntimeProvider(t *testing.T) {
	ctx := identityHome(t)
	installIdentityProfile(t, ctx, Profile{Name: "testgw", Aliases: []string{"testgw-alias"},
		BaseURL: "https://gw.example.test/v1", Mode: ModeChatCompletions}, "TESTGW_API_KEY")
	writeConfigYAML(t, home.Dir(ctx), "model:\n  provider: testgw-alias\n  default: test-model\n"+
		"  base_url: https://cfg.example.test/v1\n")
	rt, err := modelsResolver(ctx).ResolveRuntime("testgw", "", "", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if rt.Provider != "testgw" || rt.BaseURL != "https://cfg.example.test/v1" {
		t.Fatalf("runtime %q %q", rt.Provider, rt.BaseURL)
	}
}

func TestUnknownLeftCoreProviderMessageNamesTheInstall(t *testing.T) {
	h := newTestHome(t)
	saved := leftCoreTable
	t.Cleanup(func() { leftCoreTable = saved })
	leftCoreTable = []LeftCoreFeature{{Plugin: "acme-llm", Label: "Acme LLM", Unchanged: "your ACME_KEY is unchanged",
		InUse: func(h string, processEnv bool) bool {
			return ProvidersSelected(h, []string{"acme", "acme-acp"}, processEnv)["acme"]
		},
		Providers: []string{"acme", "acme-acp"}, ProfileEnv: []string{"ACME_CLI_PATH"}}}
	writeConfigYAML(t, h, "model:\n  provider: acme\n")
	ctx := context.Background()
	text := unknownProviderMessage(ctx, "ACME")
	for _, want := range []string{"Acme LLM moved out of core into the 'acme-llm' plugin", "icebath plugins install acme-llm"} {
		if !strings.Contains(text, want) {
			t.Fatalf("%q lacks %q", text, want)
		}
	}
	if err := os.MkdirAll(filepath.Join(h, "plugins", "acme-llm"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeConfigYAML(t, h, "model:\n  provider: acme\nplugins:\n  disabled: [acme-llm]\n")
	if hint := ProviderInstallHint(ctx, "acme"); !strings.Contains(hint, "icebath plugins enable acme-llm") {
		t.Fatalf("disabled hint %q", hint)
	}
	if hint := ProviderInstallHint(ctx, "other"); hint != "" {
		t.Fatalf("other hint %q", hint)
	}
	if got := unknownProviderMessage(ctx, "other"); !strings.HasPrefix(got, "Unknown provider 'other'. Check 'icebath model'") {
		t.Fatalf("plain message %q", got)
	}
}
