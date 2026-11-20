package cron

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

func writeTestFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSharedViewIsFalsyWithoutRoutesOrPrimaryAdapters(t *testing.T) {
	if newSharedRouteAdapters(map[string]any{}, nil).PyBool() {
		t.Fatal("empty shared view is truthy")
	}
	shared := newSharedRouteAdapters(map[string]any{string(gateway.PlatformDiscord): struct{}{}}, nil)
	if got := shared.Get(context.Background(), string(gateway.PlatformDiscord), nil, nil); got != nil {
		t.Fatalf("target-less get = %v", got)
	}
}

const (
	satWhatsAppRoute = "  - {name: sat-wa, platform: whatsapp, chat_id: '123@g.us', profile: sat}\n"
	satTelegramRoute = "  - {name: sat-tg, platform: telegram, chat_id: '-100', profile: sat}\n"
)

// Served profile sat under a primary root whose managed scope pins the routes
func satelliteHome(t *testing.T) (string, string, context.Context) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "root")
	sat := filepath.Join(root, "profiles", "sat")
	managed := filepath.Join(base, "managed")
	for _, dir := range []string{sat, managed} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", root)
	t.Setenv(config.EnvManagedDir, managed)
	config.InvalidateManagedCache()
	t.Cleanup(config.InvalidateManagedCache)
	return root, managed, home.WithOverride(context.Background(), sat)
}

// Gateway YAML holding multiplexed profile routes
func routesYAML(entries ...string) string {
	return "gateway:\n  multiplex_profiles: true\n  profile_routes:\n" + strings.Join(entries, "")
}

func TestManagedScopeRoutesReachSatellitePreflightAndDelivery(t *testing.T) {
	root, managed, ctx := satelliteHome(t)
	writeTestFile(t, filepath.Join(root, "config.yaml"), "platforms:\n  whatsapp:\n    enabled: true\n")
	writeTestFile(t, filepath.Join(managed, "config.yaml"), routesYAML(satWhatsAppRoute))
	if !openCronStore(home.Dir(ctx), nil).deliveryPlatformRoutedFromPrimaryGateway(ctx, "whatsapp") {
		t.Fatal("managed route did not reach preflight")
	}
	shared := newSharedRouteAdapters(map[string]any{"whatsapp": struct{}{}}, primaryProfileRoutesForCurrentHome(ctx))
	if !shared.PyBool() {
		t.Fatal("delivery-time shared view failed closed")
	}
	if shared.Get(ctx, "whatsapp", map[string]any{"chat_id": "123@g.us"}, nil) == nil {
		t.Fatal("routed target missed the primary adapter")
	}
}

func TestManagedRoutesReplaceUserFileRoutes(t *testing.T) {
	root, managed, ctx := satelliteHome(t)
	writeTestFile(t, filepath.Join(root, "config.yaml"), routesYAML(satTelegramRoute))
	writeTestFile(t, filepath.Join(managed, "config.yaml"), routesYAML(satWhatsAppRoute))
	var platforms []string
	for _, route := range primaryProfileRoutesForCurrentHome(ctx) {
		platforms = append(platforms, pyrepr.Str(route.Platform))
	}
	if len(platforms) != 1 || platforms[0] != "whatsapp" {
		t.Fatalf("platforms = %v", platforms)
	}
}

var credentialScopeRe = regexp.MustCompile(`\[profile '([^']+)', ICEBATH_HOME (.+?)\]`)

func TestMissingCodexCredentialVerdictNamesTheHomeItRead(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	alpha := filepath.Join(root, "profiles", "alpha")
	for _, h := range []string{root, alpha} {
		writeTestFile(t, filepath.Join(h, "config.yaml"), "model:\n  default: gpt-5.6-sol\n  provider: openai-codex\n")
	}
	writeTestFile(t, filepath.Join(root, "auth.json"), `{"version": 1, "providers": {}}`)
	t.Setenv("ICEBATH_HOME", root)
	t.Setenv("HOME", filepath.Join(base, "home"))
	t.Setenv("ICEBATH_PROFILE", "")
	os.Unsetenv("ICEBATH_PROFILE")
	job := &Job{ID: "7a6ae427c1d8", Name: "radar", Provider: strPtr("openai-codex"), Model: strPtr("gpt-5.6-sol")}
	cfg := config.Tree{"cron": map[string]any{}}
	scope := func(reason *string) [2]string {
		t.Helper()
		if reason == nil {
			t.Fatal("no verdict")
		}
		m := credentialScopeRe.FindStringSubmatch(*reason)
		if m == nil {
			t.Fatal(*reason)
		}
		return [2]string{m[1], m[2]}
	}
	s := openCronStore(root, nil)
	ctx := context.Background()
	if got := scope(s.preflightCheckProviderKey(ctx, job, cfg)); got != [2]string{"default", root} {
		t.Fatalf("launch scope = %v", got)
	}
	alphaCtx, alphaStore := s.profileCronScope(ctx, alpha)
	if got := scope(alphaStore.preflightCheckProviderKey(alphaCtx, job, cfg)); got != [2]string{"alpha", alpha} {
		t.Fatalf("satellite scope = %v", got)
	}
}

func TestCronEnvSettingsResolveFromTheServedProfile(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "icebath")
	alpha := filepath.Join(root, "profiles", "alpha")
	writeTestFile(t, filepath.Join(root, ".env"), "ICEBATH_CRON_TIMEOUT=111\nICEBATH_MODEL=default-model\nTERMINAL_ENV=docker\n"+
		"TERMINAL_DOCKER_IMAGE=default-only-image\nICEBATH_LANGUAGE=en\n")
	writeTestFile(t, filepath.Join(alpha, ".env"), "ICEBATH_CRON_TIMEOUT=222\nICEBATH_LANGUAGE=zh\n")
	for _, h := range []string{root, alpha} {
		if err := os.MkdirAll(filepath.Join(h, "cron"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", root)
	for _, kv := range [][2]string{{"ICEBATH_CRON_TIMEOUT", "111"}, {"ICEBATH_MODEL", "default-model"},
		{"TERMINAL_ENV", "docker"}, {"TERMINAL_DOCKER_IMAGE", "default-only-image"}, {"ICEBATH_LANGUAGE", "en"}} {
		t.Setenv(kv[0], kv[1])
	}
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	ctx := home.WithOverride(context.Background(), alpha)
	captured := map[string]string{}
	prevResolve := preflightResolveRuntime
	preflightResolveRuntime = func(_ context.Context, _, requested, targetModel, _, _ string) (*provider.Runtime, error) {
		captured["requested"], captured["target_model"] = requested, targetModel
		return &provider.Runtime{}, nil
	}
	t.Cleanup(func() { preflightResolveRuntime = prevResolve })
	s := openCronStore(alpha, nil)
	if got := s.cronInactivitySeconds(ctx); got != 222.0 {
		t.Fatalf("tick inactivity = %v", got)
	}
	if got := s.oneshotRunClaimTTL(ctx); got != 1800.0 {
		t.Fatalf("claim ttl = %v", got)
	}
	fire := provider.WithSecretScope(ctx, provider.BuildProfileSecretScope(alpha), alpha)
	if got := s.cronInactivitySeconds(fire); got != 222.0 {
		t.Fatalf("fire inactivity = %v", got)
	}
	s.preflightCheckProviderKey(fire, &Job{ID: "j"}, config.Tree{"cron": map[string]any{}})
	if tm, ok := captured["target_model"]; !ok || tm != "" {
		t.Fatalf("target_model = %q (%v)", tm, ok)
	}
	_, err := s.loadCronJobConfig(fire, &Job{ID: "j", Name: "j", Prompt: "x"}, "j", "j")
	if err == nil || cronExceptionName(err) != "RuntimeError" || !strings.Contains(err.Error(), "no model configured") {
		t.Fatalf("err = %v", err)
	}
}
