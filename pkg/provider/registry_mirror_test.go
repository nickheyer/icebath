package provider

import (
	"context"
	"strings"
	"testing"
)

func TestPostDiscoveryRegistrationIsMirrored(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	registerTestProfile(t, Profile{Name: "probe-102123-late", DisplayName: "Late", BaseURL: "https://late.example/v1",
		KeyEnv: []string{"PROBE_102123_LATE_KEY"}, Aliases: []string{"probe-102123-late-alias"}})
	row := registryLookup(ctx, "probe-102123-late")
	alias := registryLookup(ctx, "probe-102123-late-alias")
	if row == nil || alias == nil || alias.Name != row.Name {
		t.Fatalf("late registration not mirrored: %+v %+v", row, alias)
	}
}

func TestUserPluginAliasRepointsAndDisplayNameFollows(t *testing.T) {
	snapshotProviderRegistry(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	ctx := context.Background()
	const taken = "probe-116668-alias"
	registerBundledProvider(Profile{Name: "probe-116668-bundled", DisplayName: "Bundled", BaseURL: "https://bundled.example/v1",
		KeyEnv: []string{"PROBE_116668_BUNDLED_KEY"}, Aliases: []string{taken}})
	syncAuthRegistry(ctx)
	if row := registryLookup(ctx, taken); row == nil || row.Name != "probe-116668-bundled" {
		t.Fatalf("alias on the bundled row: %+v", row)
	}
	registerBundledProvider(Profile{Name: "probe-116668-other", DisplayName: "Other", BaseURL: "https://other.example/v1",
		KeyEnv: []string{"PROBE_116668_OTHER_KEY"}, Aliases: []string{taken}})
	syncAuthRegistry(ctx)
	if row := registryLookup(ctx, taken); row == nil || row.Name != "probe-116668-bundled" {
		t.Fatalf("a second bundled claim keeps the first owner: %+v", row)
	}
	writeProviderPluginDir(t, h, "probe-user")
	user := Profile{Name: "probe-116668-user", DisplayName: "Mine", BaseURL: "https://mine.example/v1",
		KeyEnv: []string{"PROBE_116668_USER_KEY"}, Aliases: []string{taken}}
	fakeExternalProviderPlugins(t, map[string]func(string) ([]Profile, error){
		"probe-user": func(string) ([]Profile, error) { return []Profile{user}, nil },
		"probe-user-2": func(string) ([]Profile, error) {
			return []Profile{{Name: "probe-116668-bundled", DisplayName: "Bundled (mine)", BaseURL: "https://mine.example/v2",
				KeyEnv: []string{"PROBE_116668_BUNDLED_KEY"}, Aliases: []string{taken}}}, nil
		},
	})
	expireHomeLayerChecks()
	if row := registryLookup(ctx, taken); row == nil || row.Name != "probe-116668-user" {
		t.Fatalf("a user plugin owns the alias it declares: %+v", row)
	}
	writeProviderPluginDir(t, h, "probe-user-2")
	expireHomeLayerChecks()
	row := registryLookup(ctx, "probe-116668-bundled")
	if row == nil || row.Label() != "Bundled (mine)" || row.BaseURL != "https://mine.example/v2" {
		t.Fatalf("same name user row: %+v", row)
	}
	if a := registryLookup(ctx, taken); a == nil || a.Name != "probe-116668-bundled" {
		t.Fatalf("the latest user claim owns the alias: %+v", a)
	}
}

func TestPluginsDiscoveredAfterAuthImportResolve(t *testing.T) {
	snapshotProviderRegistry(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	for _, n := range []string{"aaa-early-probe", "zzz-late-probe"} {
		writeProviderPluginDir(t, h, n)
	}
	fakeExternalProviderPlugins(t, map[string]func(string) ([]Profile, error){
		"aaa-early-probe": func(string) ([]Profile, error) {
			return []Profile{{Name: "aaa-early-probe", AuthType: AuthTypeAPIKey, BaseURL: "https://aaa-early-probe.example/v1", KeyEnv: []string{"AAA_EARLY_PROBE_KEY"}}}, nil
		},
		"zzz-late-probe": func(string) ([]Profile, error) {
			return []Profile{{Name: "zzz-late-probe", AuthType: AuthTypeAPIKey, Aliases: []string{"zzz-alias"},
				BaseURL: "https://zzz-late-probe.example/v1", KeyEnv: []string{"ZZZ_LATE_PROBE_KEY"}}}, nil
		},
	})
	env := testEnv(t, h)
	names := listedNames(ListProviders(context.Background()))
	if !containsString(names, "aaa-early-probe") || !containsString(names, "zzz-late-probe") {
		t.Fatalf("listed: %v", names)
	}
	for req, want := range map[string]string{"aaa-early-probe": "aaa-early-probe", "zzz-late-probe": "zzz-late-probe", "zzz-alias": "zzz-late-probe"} {
		if got, err := resolveProvider(env, req); err != nil || got != want {
			t.Fatalf("resolve_provider(%s) = %q %v", req, got, err)
		}
	}
	cfg := registryLookup(context.Background(), "zzz-late-probe")
	if cfg == nil || strings.Join(cfg.KeyEnv, ",") != "ZZZ_LATE_PROBE_KEY" || cfg.BaseURL != "https://zzz-late-probe.example/v1" {
		t.Fatalf("mirrored row: %+v", cfg)
	}
}
