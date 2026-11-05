package provider

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

const (
	ambientSlug  = "ambient-fake"
	ambientToken = "fake-ambient-token-0123456789abcdef"
)

// Plugin home with the ambient token plugin and a bare home bound as the process home
func ambientHomes(t *testing.T) (pluginHome, bareHome string) {
	t.Helper()
	snapshotProviderRegistry(t)
	bareHome = newTestHome(t)
	pluginHome = t.TempDir()
	for _, p := range Profiles() {
		for _, v := range p.KeyEnvNames() {
			t.Setenv(v, "")
		}
	}
	perHomePlugins(t, map[string]map[string]Profile{pluginHome: {ambientSlug: {Name: ambientSlug,
		Aliases: []string{ambientSlug + "-alias"}, DisplayName: "Ambient Fake", AuthType: AuthTypeAPIKey,
		KeyEnv: []string{"FAKE_AMBIENT_GH_TOKEN"}, BaseURL: "https://ambient-fake.invalid/v1", Mode: ModeChatCompletions,
		RegistryAfter: "lmstudio", AmbientCredentials: true}}})
	pluginMirroredMu.Lock()
	mirrored := map[string]bool{}
	for k, v := range pluginMirroredProviders {
		mirrored[k] = v
	}
	pluginMirroredMu.Unlock()
	t.Cleanup(func() {
		pluginMirroredMu.Lock()
		pluginMirroredProviders = mirrored
		pluginMirroredMu.Unlock()
	})
	t.Setenv("FAKE_AMBIENT_GH_TOKEN", ambientToken)
	t.Setenv("GEMINI_API_KEY", "AIza"+"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
	return pluginHome, bareHome
}

type ambientRuntime struct {
	provider, baseURL string
	ownToken          bool
	err               error
}

type ambientView struct {
	profile               string
	rows, lookup, aliased bool
	explicit, auto        string
	explicitErr, autoErr  error
	creds                 bool
	credsErr              error
	runtimeExplicit       ambientRuntime
	runtimeAuto           ambientRuntime
}

// Every provider lookup a turn makes, each read with the home bound to the context
func viewAmbientHome(t *testing.T, h string) ambientView {
	t.Helper()
	ctx := home.WithOverride(context.Background(), h)
	env := testEnv(t, h)
	env.Ctx = ctx
	var v ambientView
	if p := getProviderProfile(ctx, ambientSlug); p != nil {
		v.profile = p.Name
	}
	for _, row := range canonicalRegistryRows(ctx) {
		v.rows = v.rows || row.Name == ambientSlug
	}
	reg := providerRegistry()
	v.lookup, v.aliased = reg.contains(ctx, ambientSlug), reg.get(ctx, ambientSlug+"-alias") != nil
	v.explicit, v.explicitErr = resolveProvider(env, ambientSlug)
	v.auto, v.autoErr = resolveProvider(env, "auto")
	creds, err := authResolver(env).apiKeyProviderCredentials(ambientSlug)
	v.creds, v.credsErr = err == nil && creds.APIKey == ambientToken, err
	runtime := func(requested string) ambientRuntime {
		route, err := Resolve(ctx, config.Tree(map[string]any{}), config.NewSecrets(h), h, NewStore(h), Overrides{Provider: requested})
		return ambientRuntime{route.Provider, route.BaseURL, route.APIKey == ambientToken, err}
	}
	v.runtimeExplicit, v.runtimeAuto = runtime(ambientSlug), runtime("")
	return v
}

func assertAmbientOwner(t *testing.T, v ambientView) {
	t.Helper()
	if v.profile != ambientSlug || !v.rows || !v.lookup || !v.aliased {
		t.Fatalf("owner home lost its plugin: %+v", v)
	}
	if v.explicit != ambientSlug || v.explicitErr != nil || !v.creds || v.credsErr != nil {
		t.Fatalf("owner explicit resolution: %+v %v %v", v, v.explicitErr, v.credsErr)
	}
	want := ambientRuntime{ambientSlug, "https://ambient-fake.invalid/v1", true, nil}
	if v.runtimeExplicit != want {
		t.Fatalf("owner explicit runtime = %+v", v.runtimeExplicit)
	}
	if v.auto != "gemini" || v.autoErr != nil || v.runtimeAuto.provider != "gemini" || v.runtimeAuto.err != nil {
		t.Fatalf("ambient credentials never auto select: %+v %v %v", v, v.autoErr, v.runtimeAuto.err)
	}
}

func assertAmbientAbsent(t *testing.T, v ambientView) {
	t.Helper()
	var authErr *AuthError
	if v.profile != "" || v.rows || v.lookup || v.aliased {
		t.Fatalf("bare home sees the plugin row: %+v", v)
	}
	if !errors.As(v.explicitErr, &authErr) || !errors.As(v.credsErr, &authErr) || v.runtimeExplicit.err == nil {
		t.Fatalf("bare home resolved the slug: %+v", v)
	}
	if v.auto != "gemini" || v.runtimeAuto.provider != "gemini" || v.runtimeAuto.ownToken || v.runtimeAuto.err != nil {
		t.Fatalf("bare home auto detection: %+v", v)
	}
}

func TestMirroredRowIsAbsentInAHomeWithoutThePluginABA(t *testing.T) {
	pluginHome, bareHome := ambientHomes(t)
	assertAmbientOwner(t, viewAmbientHome(t, pluginHome))
	assertAmbientAbsent(t, viewAmbientHome(t, bareHome))
	assertAmbientOwner(t, viewAmbientHome(t, pluginHome))
	assertAmbientAbsent(t, viewAmbientHome(t, bareHome))
}

func TestHiddenRowsSurviveRegistryReordering(t *testing.T) {
	pluginHome, _ := ambientHomes(t)
	pluginCtx := home.WithOverride(context.Background(), pluginHome)
	getProviderProfile(pluginCtx, ambientSlug)
	reg := providerRegistry()
	if !reg.contains(pluginCtx, ambientSlug) {
		t.Fatal("the plugin home mirrors its row")
	}
	insertRegistryRow(reg.rawGet("gemini"), "lmstudio")
	if !reg.contains(pluginCtx, ambientSlug) {
		t.Fatal("a reorder under another home dropped the hidden row")
	}
	reg.mu.RLock()
	kept := slices.Contains(reg.keys, ambientSlug)
	reg.mu.RUnlock()
	if !kept {
		t.Fatal("stored keys lost the row")
	}
}
