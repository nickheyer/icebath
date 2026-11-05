package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Plugin profiles keyed by home key then plugin dir, each home serving its own copy of a dir name
func perHomePlugins(t *testing.T, byHome map[string]map[string]Profile) {
	t.Helper()
	byDir := map[string]func(string) ([]Profile, error){}
	keyed := map[string]map[string]Profile{}
	for h, dirs := range byHome {
		keyed[home.Key(h)] = dirs
		for dir := range dirs {
			writeProviderPluginDir(t, h, dir)
			name := dir
			byDir[name] = func(homeKey string) ([]Profile, error) {
				if p, ok := keyed[homeKey][name]; ok {
					return []Profile{p}, nil
				}
				return nil, nil
			}
		}
	}
	fakeExternalProviderPlugins(t, byDir)
}

func scopePluginProfile(name, url, env string, aliases ...string) Profile {
	return Profile{Name: name, AuthType: AuthTypeAPIKey, KeyEnv: []string{env}, BaseURL: url, Aliases: aliases,
		CatalogAliases: true}
}

func TestEachHomeReadsItsOwnProviderRowsAndAliases(t *testing.T) {
	snapshotProviderRegistry(t)
	root := t.TempDir()
	a, b, c := filepath.Join(root, "a"), filepath.Join(root, "b"), filepath.Join(root, "c")
	perHomePlugins(t, map[string]map[string]Profile{
		a: {"acme-scope": scopePluginProfile("acme-scope", "https://a.invalid/v1", "ACME_SCOPE_A_KEY", "acs"),
			"deepseek": scopePluginProfile("deepseek", "https://deepseek-a.invalid/v1", "DEEPSEEK_API_KEY")},
		b: {"acme-scope": scopePluginProfile("acme-scope", "https://b.invalid/v1", "ACME_SCOPE_B_KEY"),
			"zeta-scope": scopePluginProfile("zeta-scope", "https://z.invalid/v1", "ZETA_SCOPE_KEY", "acs")},
	})
	if err := os.MkdirAll(c, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", c)
	type view struct {
		URL      string
		Env      []string
		Alias    string
		Norm     string
		Deepseek string
	}
	viewOf := func(h string) view {
		ctx := home.WithOverride(context.Background(), h)
		var v view
		if acme := registryLookup(ctx, "acme-scope"); acme != nil {
			v.URL, v.Env = acme.BaseURL, acme.KeyEnv
		}
		if alias := providerRegistry().get(ctx, "acs"); alias != nil {
			v.Alias = alias.Name
		}
		v.Norm = normalizeProviderCtx(ctx, "acs")
		v.Deepseek = providerRegistry().get(ctx, "deepseek").BaseURL
		return v
	}
	builtinDeepseek := viewOf(c).Deepseek
	homeA := view{"https://a.invalid/v1", []string{"ACME_SCOPE_A_KEY"}, "acme-scope", "acme-scope", "https://deepseek-a.invalid/v1"}
	if got := viewOf(a); !reflect.DeepEqual(got, homeA) {
		t.Fatalf("home a = %+v", got)
	}
	homeB := view{"https://b.invalid/v1", []string{"ACME_SCOPE_B_KEY"}, "zeta-scope", "zeta-scope", builtinDeepseek}
	if got := viewOf(b); !reflect.DeepEqual(got, homeB) {
		t.Fatalf("home b = %+v", got)
	}
	if got := viewOf(a); !reflect.DeepEqual(got, homeA) {
		t.Fatalf("home a reread = %+v", got)
	}
	if got := viewOf(c); !reflect.DeepEqual(got, view{Norm: "acs", Deepseek: builtinDeepseek}) {
		t.Fatalf("home c = %+v", got)
	}
}

func aliasCatalogEntry(key, api string) map[string]any {
	return map[string]any{"id": key, "name": strings.ToUpper(key[:1]) + key[1:],
		"env": []any{strings.ToUpper(strings.ReplaceAll(key, "-", "_")) + "_API_KEY"}, "api": api, "models": map[string]any{}}
}

var pluginAliasCatalog = map[string]any{
	"meta":          aliasCatalogEntry("meta", "https://api.meta.ai/v1"),
	"azure":         aliasCatalogEntry("azure", "https://example.openai.azure.com/openai"),
	"google":        aliasCatalogEntry("google", "https://generativelanguage.googleapis.com/v1beta"),
	"google-vertex": aliasCatalogEntry("google-vertex", "https://aiplatform.googleapis.com/v1"),
}

// Serves registry as the models.dev catalog, an empty registry answering as an unreachable network
func serveModelsDev(t *testing.T, registry map[string]any) *modelsDevFixture {
	f := newModelsDevFixture(t)
	if len(registry) == 0 {
		f.respond(0, nil, "", errors.New("offline"))
		return f
	}
	f.setMemory(registry, time.Now())
	return f
}

func TestPluginAliasResolvesToThePluginID(t *testing.T) {
	aliases := map[string]string{"meta": "meta-ai", "azure": "azure-foundry", "google": "gemini", "google-vertex": "vertex"}
	for _, reg := range []struct {
		id      string
		catalog map[string]any
	}{{"catalog", pluginAliasCatalog}, {"offline", map[string]any{}}} {
		for _, alias := range []string{"azure", "google", "google-vertex", "meta"} {
			t.Run(reg.id+"/"+alias, func(t *testing.T) {
				f := serveModelsDev(t, reg.catalog)
				pdef := ResolveProviderFull(f.ctx, alias, nil, nil)
				if pdef == nil || pdef.ID != aliases[alias] {
					t.Fatalf("resolve_provider_full(%q) = %+v", alias, pdef)
				}
				env := testEnv(t, f.home)
				env.Ctx = f.ctx
				if got, err := resolveProvider(env, alias); err != nil || got != pdef.ID {
					t.Fatalf("resolve_provider(%q) = %q %v", alias, got, err)
				}
			})
		}
	}
}

func TestUserProviderNamedLikeTheAliasStillWins(t *testing.T) {
	f := serveModelsDev(t, pluginAliasCatalog)
	user := map[string]any{"meta": map[string]any{"base_url": "https://relay.example.com/v1", "api_key": "sk-test"}}
	pdef := ResolveProviderFull(f.ctx, "meta", user, nil)
	if pdef == nil || pdef.BaseURL != "https://relay.example.com/v1" || pdef.ID == "meta-ai" {
		t.Fatalf("user provider lost its identity: %+v", pdef)
	}
}

func TestHomePluginClaimingTheAliasWinsOverTheBundledOne(t *testing.T) {
	for _, reg := range []struct {
		id      string
		catalog map[string]any
	}{{"catalog", pluginAliasCatalog}, {"offline", map[string]any{}}} {
		t.Run(reg.id, func(t *testing.T) {
			snapshotProviderRegistry(t)
			f := serveModelsDev(t, reg.catalog)
			writeProviderPluginDir(t, f.home, "my-meta")
			fakeExternalProviderPlugins(t, map[string]func(string) ([]Profile, error){
				"my-meta": func(string) ([]Profile, error) {
					return []Profile{{Name: "my-meta", Aliases: []string{"meta"}, AuthType: AuthTypeAPIKey,
						KeyEnv: []string{"MY_META_KEY"}, BaseURL: "https://relay.invalid/v1"}}, nil
				},
			})
			expireHomeLayerChecks()
			pdef := ResolveProviderFull(f.ctx, "meta", nil, nil)
			if p := getProviderProfile(f.ctx, "meta"); p == nil || p.Name != "my-meta" {
				t.Fatalf("get_provider_profile(meta) = %+v", p)
			}
			if pdef == nil || pdef.ID != "my-meta" || pdef.BaseURL != "https://relay.invalid/v1" {
				t.Fatalf("resolve_provider_full(meta) = %+v", pdef)
			}
		})
	}
}

// Registers a profile the way plugin discovery does, the registry and the auth mirror restored afterwards
func registerPluginForTest(t *testing.T, p Profile) Profile {
	t.Helper()
	RegisterProvider(p)
	registerPluginProvider(context.Background(), &p)
	return p
}

func addOAuthPoolEntry(t *testing.T, env PoolEnv, provider string, e PooledCredential) {
	t.Helper()
	e.Provider, e.ID, e.Label, e.AuthType, e.Source, e.BaseURL = provider, "e1", "acme", AuthTypeOAuth, "manual:example_device", "https://example.invalid/v1"
	if _, err := LoadPool(env, provider).AddEntry(e); err != nil {
		t.Fatal(err)
	}
}

func fakeBinaryOnPath(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestOAuthPluginStatusFollowsTheCredentialPool(t *testing.T) {
	snapshotProviderRegistry(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	registerPluginForTest(t, Profile{Name: "example-oauth", AuthType: AuthTypeOAuthExternal, BaseURL: "https://example.invalid/v1",
		RefreshCredential: func(PooledCredential) (map[string]any, error) { return map[string]any{}, nil }})
	registerPluginForTest(t, Profile{Name: "example-device", AuthType: AuthTypeOAuthDeviceCode, BaseURL: "https://d.invalid/v1"})
	env := testEnv(t, h)
	signedOut := GetAuthStatus(env, "example-oauth")
	if signedOut["logged_in"] != false || signedOut["configured"] != true ||
		!strings.Contains(signedOut["hint"].(string), "icebath auth add example-oauth") {
		t.Fatalf("signed out = %v", signedOut)
	}
	if !ProviderHasCredentials(context.Background(), "example-oauth") {
		t.Fatal("a registered plugin provider is usable with provider:model")
	}
	addOAuthPoolEntry(t, env, "example-oauth", PooledCredential{AccessToken: "tok-1", RefreshToken: "rt-1"})
	if GetAuthStatus(env, "example-oauth")["logged_in"] != true {
		t.Fatal("a live pool token signs the plugin in")
	}
	addOAuthPoolEntry(t, env, "example-device", PooledCredential{AccessToken: "tok-old", RefreshToken: "rt-old", ExpiresAtMS: 1000})
	if expired := GetAuthStatus(env, "example-device"); expired["logged_in"] != false || expired["needs_refresh"] != false {
		t.Fatalf("expired without a refresh hook = %v", expired)
	}
	addOAuthPoolEntry(t, env, "example-oauth", PooledCredential{AccessToken: "tok-old", RefreshToken: "rt-old", ExpiresAtMS: 1000})
	if GetAuthStatus(env, "example-oauth")["logged_in"] != true {
		t.Fatal("the live entry still wins")
	}
	if got := getPluginOAuthAuthStatus(env, "openai-codex"); !reflect.DeepEqual(got, map[string]any{"logged_in": false}) {
		t.Fatalf("bundled oauth provider reached the plugin builder: %v", got)
	}
}

func TestAnyExternalProcessPluginCountsAsSignedInWhenItsBinaryResolves(t *testing.T) {
	snapshotProviderRegistry(t)
	h, bin := t.TempDir(), t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	registerPluginForTest(t, Profile{Name: "example-acp", AuthType: AuthTypeExternalProcess, BaseURL: "acp://example",
		ProcessCommand: "example-acp-bin", FallbackModels: []string{"acp-a"}})
	env := testEnv(t, h)
	t.Setenv("PATH", filepath.Join(bin, "nowhere"))
	if status := GetExternalProcessProviderStatus(env, "example-acp"); status["auth_verified"] != false {
		t.Fatalf("missing binary is no sign in evidence: %v", status)
	}
	fakeBinaryOnPath(t, bin, "example-acp-bin")
	status := GetExternalProcessProviderStatus(env, "example-acp")
	if status["auth_verified"] != true || !strings.HasPrefix(fmt.Sprint(status["auth_source"]), "command: ") {
		t.Fatalf("resolving binary = %v", status)
	}
	registerPluginForTest(t, Profile{Name: "example-acp-tokens", AuthType: AuthTypeExternalProcess, BaseURL: "acp://tokens",
		ProcessCommand: "example-acp-tokens-bin", ExternalAuthEvidenceHook: func() (bool, string, bool) { return false, "", true }})
	verified, source := externalProcessAuthEvidence(context.Background(), "example-acp-tokens", filepath.Join(bin, "example-acp-tokens-bin"))
	want, wantSource, _ := getProviderProfile(context.Background(), "example-acp-tokens").ExternalAuthEvidence()
	if verified != want || source != wantSource {
		t.Fatalf("token store evidence = %v %q", verified, source)
	}
}

func TestAuthStatusReadsTheRegistryOfTheEnvBoundHome(t *testing.T) {
	snapshotProviderRegistry(t)
	launch, secondary, bin := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("ICEBATH_HOME", launch)
	writeProviderPluginDir(t, secondary, "bound-acp")
	fakeExternalProviderPlugins(t, map[string]func(string) ([]Profile, error){
		"bound-acp": func(string) ([]Profile, error) {
			return []Profile{{Name: "bound-acp", AuthType: AuthTypeExternalProcess, BaseURL: "acp://bound", ProcessCommand: "bound-acp-bin"}}, nil
		},
	})
	fakeBinaryOnPath(t, bin, "bound-acp-bin")
	env := testEnv(t, secondary)
	env.Ctx = home.WithOverride(context.Background(), secondary)
	if status := GetAuthStatus(env, "bound-acp"); status["configured"] != true || status["auth_verified"] != true {
		t.Fatalf("bound home status = %v", status)
	}
	if status := GetAuthStatus(testEnv(t, launch), "bound-acp"); !reflect.DeepEqual(status, map[string]any{"logged_in": false}) {
		t.Fatalf("launch home must not see the bound home plugin: %v", status)
	}
}

func TestCNVariantsResolveInAuthRegistry(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	for _, k := range []string{"DASHSCOPE_API_KEY", "ALIBABA_CODING_PLAN_API_KEY", "ALIBABA_TOKEN_PLAN_API_KEY"} {
		t.Setenv(k, "sk-test")
	}
	ctx := context.Background()
	env := testEnv(t, h)
	for _, pid := range []string{"alibaba-cn", "alibaba-coding-plan-cn", "alibaba-token-plan", "alibaba-token-plan-cn"} {
		row := providerRegistry().get(ctx, pid)
		if row == nil {
			t.Fatalf("%s missing from the auth registry", pid)
		}
		if got, err := resolveProvider(env, pid); err != nil || got != pid {
			t.Fatalf("resolve_provider(%s) = %q %v", pid, got, err)
		}
		if p := getProviderProfile(ctx, pid); p == nil || row.BaseURL != p.BaseURL {
			t.Fatalf("%s row base %q differs from its profile %+v", pid, row.BaseURL, p)
		}
	}
}

const hostSeamName = "l4-fake-hosted"

// Registers the fake hosted profile after discovery with a live context hook recording its calls
func hostSeamProfile(t *testing.T) *[][3]string {
	t.Helper()
	snapshotProviderRegistry(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	calls := &[][3]string{}
	ListProviders(context.Background())
	RegisterProvider(Profile{Name: hostSeamName, BaseURL: "https://api.fake-llm.test/v1", Hostnames: []string{"fake-llm.test"},
		MetadataHostnames: []string{"catalog.fake-meta.test"},
		LiveContextLengthHook: func(_ context.Context, model, apiKey, baseURL string) int {
			*calls = append(*calls, [3]string{model, apiKey, baseURL})
			return 777_000
		}})
	return calls
}

func TestProfileForHostMatchesClaimedHostsExactOrSubdomain(t *testing.T) {
	hostSeamProfile(t)
	ctx := context.Background()
	for _, tc := range []struct {
		url      string
		metadata bool
		claimed  bool
	}{
		{"https://api.fake-llm.test/v1", false, true}, {"https://api.enterprise.fake-llm.test", false, true},
		{"fake-llm.test", false, true}, {"https://fake-llm.test.attacker.invalid", false, false},
		{"https://notfake-llm.test", false, false}, {"https://catalog.fake-meta.test/inference", false, false},
		{"https://catalog.fake-meta.test/inference", true, true},
	} {
		p, ok := ProfileForHost(ctx, tc.url, tc.metadata)
		if ok != tc.claimed || (ok && p.Name != hostSeamName) {
			t.Fatalf("profile_for_host(%q, metadata=%v) = %v %q", tc.url, tc.metadata, ok, p.Name)
		}
	}
}

func TestClaimedHostsDriveURLInference(t *testing.T) {
	hostSeamProfile(t)
	for _, u := range []string{"https://api.business.fake-llm.test/v1", "https://catalog.fake-meta.test/inference"} {
		if got := inferProviderFromBaseURL(u); got != hostSeamName {
			t.Fatalf("_infer_provider_from_url(%q) = %q", u, got)
		}
	}
}

func TestLiveContextLengthAnswersAfterCacheWithCredentials(t *testing.T) {
	calls := hostSeamProfile(t)
	got := resolveProviderAwareContextLength(context.Background(), "m-1", "https://api.fake-llm.test/v1", "sk-x", hostSeamName, hostSeamName)
	if got != 777_000 || !reflect.DeepEqual(*calls, [][3]string{{"m-1", "sk-x", "https://api.fake-llm.test/v1"}}) {
		t.Fatalf("context = %d calls = %v", got, *calls)
	}
}

func TestProfileForHostAnswersFollowRegistryChanges(t *testing.T) {
	snapshotProviderRegistry(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	host := "https://api.late-claim.test/v1"
	ListProviders(ctx)
	if _, ok := ProfileForHost(ctx, host, false); ok {
		t.Fatal("no profile claims the host yet")
	}
	RegisterProvider(Profile{Name: "l4-late-claim", Hostnames: []string{"late-claim.test"}})
	if p, ok := ProfileForHost(ctx, host, false); !ok || p.Name != "l4-late-claim" {
		t.Fatal("a later registration is seen at once")
	}
	unregisterTestProvider("l4-late-claim")
	if _, ok := ProfileForHost(ctx, host, false); ok {
		t.Fatal("the removed registration still answers")
	}
}

// Drops one registration the way the Python cleanup pops _REGISTRY and clears the list cache
func unregisterTestProvider(name string) {
	regMu.Lock()
	delete(regProfiles, name)
	regOrder = slices.DeleteFunc(slices.Clone(regOrder), func(n string) bool { return n == name })
	regListCache = nil
	regMu.Unlock()
}

const fakeHint = "Run `fake-cli login` to sign in."

// Resolves the runtime for the fake hinted api key provider whose key is unset
func resolveHintedProvider(t *testing.T) error {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("FAKE_HINTED_KEY", "")
	_, err := Resolve(context.Background(), config.Tree(map[string]any{}), config.NewSecrets(h), h, &fakeCreds{},
		Overrides{Provider: "fake-hinted"})
	return err
}

func hintedProfile(hint func() string) Profile {
	return Profile{Name: "fake-hinted", DisplayName: "Fake", AuthType: AuthTypeAPIKey, KeyEnv: []string{"FAKE_HINTED_KEY"},
		BaseURL: "https://api.fake-hinted.test/v1", MissingCredentialsHintHook: hint}
}

func TestRuntimeResolutionAppendsTheProfileHint(t *testing.T) {
	for _, tc := range []struct {
		hint   func() string
		suffix string
	}{{func() string { return fakeHint }, " " + fakeHint}, {nil, ""}} {
		t.Run(fmt.Sprintf("hinted=%v", tc.hint != nil), func(t *testing.T) {
			snapshotProviderRegistry(t)
			registerPluginForTest(t, hintedProfile(tc.hint))
			err := resolveHintedProvider(t)
			want := "No usable credentials found for provider 'fake-hinted'. Set FAKE_HINTED_KEY." + tc.suffix
			if err == nil || err.Error() != want {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestABrokenHintKeepsTheAuthError(t *testing.T) {
	snapshotProviderRegistry(t)
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	missingHintsMu.Lock()
	missingHintFailuresLogged = map[string]bool{}
	missingHintsMu.Unlock()
	registerPluginForTest(t, hintedProfile(func() string { panic("plugin host is down") }))
	for range 2 {
		err := resolveHintedProvider(t)
		if err == nil || err.Error() != "No usable credentials found for provider 'fake-hinted'. Set FAKE_HINTED_KEY." {
			t.Fatalf("error = %v", err)
		}
	}
	if n := strings.Count(logs.String(), "missing_credentials_hint failed"); n != 1 {
		t.Fatalf("warned %d times, once per provider expected", n)
	}
}

// Swaps the named custom config probe for one test
func fakeNamedCustomProbe(t *testing.T, fn func(name string) bool) {
	t.Helper()
	prev := namedCustomProviderConfiguredFn
	namedCustomProviderConfiguredFn = func(_ context.Context, name, _ string) bool { return fn(name) }
	t.Cleanup(func() { namedCustomProviderConfiguredFn = prev })
}

func TestBareNamedCustomProviderGetsCustomProfile(t *testing.T) {
	snapshotProviderRegistry(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	custom := getProviderProfile(ctx, "custom")
	if custom == nil {
		t.Fatal("custom profile missing")
	}
	fakeNamedCustomProbe(t, func(name string) bool { return name == "my-endpoint" })
	if got := getProviderProfile(ctx, "my-endpoint"); got != custom {
		t.Fatalf("named custom = %+v", got)
	}
}

func TestNamedCustomFallbackDoesNotOverrideRegisteredRoutes(t *testing.T) {
	snapshotProviderRegistry(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	getProviderProfile(ctx, "custom")
	RegisterProvider(Profile{Name: "custom:fixture"})
	if p := getProviderProfile(ctx, "custom:fixture"); p == nil || p.Name != "custom:fixture" {
		t.Fatalf("dedicated route = %+v", p)
	}
	if getProviderProfile(ctx, "CUSTOM:unregistered") != getProviderProfile(ctx, "custom") {
		t.Fatal("an unregistered custom: route reads the custom profile")
	}
	if getProviderProfile(ctx, "NONEXISTENT") != nil {
		t.Fatal("an unknown name has no profile")
	}
}

func TestBareNamedCustomMemoCachesTheHelperResult(t *testing.T) {
	snapshotProviderRegistry(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	var calls []string
	fakeNamedCustomProbe(t, func(name string) bool { calls = append(calls, name); return false })
	ctx := context.Background()
	for range 2 {
		if hasNamedCustomProvider(ctx, "zzz-unknown", h, home.Key(h)) {
			t.Fatal("unknown name answered true")
		}
	}
	if !reflect.DeepEqual(calls, []string{"zzz-unknown"}) {
		t.Fatalf("second lookup must be a memo hit, got calls=%v", calls)
	}
}

func TestBareNamedCustomMemoInvalidatesOnConfigChange(t *testing.T) {
	snapshotProviderRegistry(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	ctx, key, cfg := context.Background(), home.Key(h), filepath.Join(h, "config.yaml")
	for _, step := range []struct {
		yaml string
		want bool
	}{{"", false}, {"providers:\n  my-endpoint:\n    base_url: http://127.0.0.1:1/v1\n", true}, {"{}\n", false}} {
		if step.yaml != "" {
			if err := os.WriteFile(cfg, []byte(step.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if got := hasNamedCustomProvider(ctx, "my-endpoint", h, key); got != step.want {
			t.Fatalf("after %q got %v", step.yaml, got)
		}
	}
}

func TestBareNamedCustomMemoIsKeyedByHome(t *testing.T) {
	snapshotProviderRegistry(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	answers, current := map[string]bool{"key-a": true, "key-b": false}, "key-a"
	fakeNamedCustomProbe(t, func(string) bool { return answers[current] })
	ctx := context.Background()
	if !hasNamedCustomProvider(ctx, "n", h, "key-a") {
		t.Fatal("key-a answers true")
	}
	current = "key-b"
	if hasNamedCustomProvider(ctx, "n", h, "key-b") || !hasNamedCustomProvider(ctx, "n", h, "key-a") {
		t.Fatal("home keys borrowed each other's answers")
	}
}

type missTTLClock struct {
	now   time.Time
	ns    int64
	stats int
	scans int
}

// Fresh home with hand driven registry clocks, counted stamp reads and counted home scans
func missTTLLayerEnv(t *testing.T, names ...string) (string, *missTTLClock) {
	t.Helper()
	h := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	snapshotProviderRegistry(t)
	homeLayersMu.Lock()
	homeLayers = map[string]*homeLayer{}
	homeLayersMu.Unlock()
	clock := &missTTLClock{now: time.Unix(1000, 0), ns: time.Now().UnixNano()}
	prevMono, prevWall, prevStamps, prevScan := registryMonotonic, registryWallNanos, pluginDirStampsFn, scanHomeLayerFn
	registryMonotonic = func() time.Time { return clock.now }
	registryWallNanos = func() int64 { return clock.ns }
	pluginDirStampsFn = func(h string) []int64 { clock.stats++; return prevStamps(h) }
	scanHomeLayerFn = func(ctx context.Context, layer *homeLayer, h, key string) {
		clock.scans++
		prevScan(ctx, layer, h, key)
	}
	t.Cleanup(func() {
		registryMonotonic, registryWallNanos, pluginDirStampsFn, scanHomeLayerFn = prevMono, prevWall, prevStamps, prevScan
	})
	byDir := map[string]func(string) ([]Profile, error){}
	for _, n := range names {
		byDir[n] = func(string) ([]Profile, error) {
			return []Profile{{Name: n, AuthType: AuthTypeAPIKey, KeyEnv: []string{"X_KEY"}, BaseURL: "https://" + n + ".invalid/v1"}}, nil
		}
	}
	fakeExternalProviderPlugins(t, byDir)
	return h, clock
}

func TestRepeatedMissesDoNotRestatWithinTheTTL(t *testing.T) {
	_, clock := missTTLLayerEnv(t)
	ctx := context.Background()
	if getProviderProfile(ctx, "no-profile-id") != nil {
		t.Fatal("no profile expected")
	}
	clock.stats = 0
	clock.now = clock.now.Add(100 * time.Millisecond)
	for range 50 {
		if getProviderProfile(ctx, "no-profile-id") != nil {
			t.Fatal("no profile expected")
		}
	}
	if clock.stats > 1 {
		t.Fatalf("restated %d times inside the TTL", clock.stats)
	}
	clock.now = clock.now.Add(pluginDirStampTTL)
	clock.stats = 0
	if getProviderProfile(ctx, "no-profile-id") != nil || clock.stats != 1 {
		t.Fatalf("TTL elapsed checks once, got %d", clock.stats)
	}
}

func TestNewInstallFoundByNewNameAndAfterTheTTL(t *testing.T) {
	h, clock := missTTLLayerEnv(t, "late-a", "late-b")
	ctx := context.Background()
	if getProviderProfile(ctx, "late-a") != nil {
		t.Fatal("late-a not installed yet")
	}
	clock.now = clock.now.Add(100 * time.Millisecond)
	if getProviderProfile(ctx, "late-a") != nil {
		t.Fatal("late-a not installed yet")
	}
	writeProviderPluginDir(t, h, "late-b")
	clock.now = clock.now.Add(100 * time.Millisecond)
	if getProviderProfile(ctx, "late-b") == nil {
		t.Fatal("a new name forces a check")
	}
	writeProviderPluginDir(t, h, "late-a")
	clock.now = clock.now.Add(pluginDirStampTTL)
	if getProviderProfile(ctx, "late-a") == nil {
		t.Fatal("found once the TTL passed")
	}
}

func TestRuntimeRegistrationIsFoundDespiteACachedMiss(t *testing.T) {
	missTTLLayerEnv(t)
	ctx := context.Background()
	if getProviderProfile(ctx, "late-runtime") != nil {
		t.Fatal("not registered yet")
	}
	RegisterProvider(Profile{Name: "late-runtime", AuthType: AuthTypeAPIKey})
	if getProviderProfile(ctx, "late-runtime") == nil {
		t.Fatal("runtime registration hidden by the cached miss")
	}
}

func TestInstallInTheSameTimestampTickIsFoundAfterTheTTL(t *testing.T) {
	h, clock := missTTLLayerEnv(t, "tick-a", "tick-b")
	prevScan := scanHomeLayerFn
	scanHomeLayerFn = func(ctx context.Context, layer *homeLayer, h, key string) {
		prevScan(ctx, layer, h, key)
		clock.ns += 3_000_000_000
	}
	ctx := context.Background()
	writeProviderPluginDir(t, h, "tick-b")
	modelProviders := filepath.Join(h, "plugins", "model-providers")
	for _, p := range []string{filepath.Join(h, "plugins"), modelProviders} {
		if err := os.Chtimes(p, time.Unix(0, clock.ns), time.Unix(0, clock.ns)); err != nil {
			t.Fatal(err)
		}
	}
	if getProviderProfile(ctx, "tick-b") == nil {
		t.Fatal("tick-b not found")
	}
	before, err := os.Stat(modelProviders)
	if err != nil {
		t.Fatal(err)
	}
	writeProviderPluginDir(t, h, "tick-a")
	if err := os.Chtimes(modelProviders, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(pluginDirStampTTL)
	if getProviderProfile(ctx, "tick-a") == nil {
		t.Fatal("an install inside the stamp tick must be found after the TTL")
	}
}

func TestSettledUnchangedPluginDirsAreNotRescanned(t *testing.T) {
	h, clock := missTTLLayerEnv(t, "settled")
	writeProviderPluginDir(t, h, "settled")
	hourAgo := time.Unix(0, clock.ns-3600*1_000_000_000)
	for _, p := range []string{filepath.Join(h, "plugins"), filepath.Join(h, "plugins", "model-providers")} {
		if err := os.Chtimes(p, hourAgo, hourAgo); err != nil {
			t.Fatal(err)
		}
	}
	clock.scans = 0
	ctx := context.Background()
	if getProviderProfile(ctx, "settled") == nil {
		t.Fatal("settled not found")
	}
	for range 3 {
		clock.now = clock.now.Add(pluginDirStampTTL)
		if getProviderProfile(ctx, "settled") == nil || getProviderProfile(ctx, "no-profile-id") != nil {
			t.Fatal("lookup changed")
		}
	}
	if clock.scans != 1 {
		t.Fatalf("scanned %d times, settled stamps are trusted", clock.scans)
	}
}
