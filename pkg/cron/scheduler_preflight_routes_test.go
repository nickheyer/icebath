package cron

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
)

func TestCronPreflightEnabledUnlessLiterallyFalse(t *testing.T) {
	s := &Store{}
	for _, c := range []struct {
		cfg  config.Tree
		want bool
	}{
		{nil, true}, {config.Tree{"cron": "x"}, true}, {config.Tree{"cron": map[string]any{}}, true},
		{config.Tree{"cron": map[string]any{"preflight": 0}}, true}, {config.Tree{"cron": map[string]any{"preflight": nil}}, true},
		{config.Tree{"cron": map[string]any{"preflight": false}}, false},
	} {
		if got := s.cronPreflightEnabled(c.cfg); got != c.want {
			t.Fatalf("%v -> %v", c.cfg, got)
		}
	}
}

func TestHasCredentialPlatformIgnoresTheLocalAdapter(t *testing.T) {
	if hasCredentialPlatform(map[string]any{"local": 1}, nil) || hasCredentialPlatform(nil, nil) || hasCredentialPlatform(map[string]any{}, []any{}) {
		t.Fatal("local only or empty maps hold no credential platform")
	}
	if !hasCredentialPlatform(map[string]any{"local": 1, "telegram": 2}, nil) || !hasCredentialPlatform(map[string]any{}, []any{"telegram"}) {
		t.Fatal("a platform adapter or an owned bot is a credential platform")
	}
}

func parsedRoutes(t *testing.T, raw []any) []*gateway.ProfileRoute {
	t.Helper()
	routes, err := gateway.ParseProfileRoutes(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	return routes
}

func TestSharedRouteAdaptersResolveOnlyTargetExactRoutes(t *testing.T) {
	routes := parsedRoutes(t, []any{
		map[string]any{"platform": "telegram", "profile": "ops", "chat_id": "123"},
		map[string]any{"platform": "discord", "profile": "ops", "guild_id": "g1", "chat_id": "c1"},
		map[string]any{"platform": "slack", "profile": "ops", "guild_id": "g2"},
	})
	primary := map[string]any{"telegram": "tg-bot", "discord": "dc-bot", "slack": "sl-bot"}
	a := newSharedRouteAdapters(primary, routes)
	ctx := context.Background()
	if !a.PyBool() || newSharedRouteAdapters(primary, nil).PyBool() || newSharedRouteAdapters(nil, routes).PyBool() {
		t.Fatal("truthy only with adapters and routes")
	}
	cases := []struct {
		platform string
		target   map[string]any
		want     any
	}{
		{"telegram", map[string]any{"chat_id": "123"}, "tg-bot"},
		{"telegram", map[string]any{"chat_id": "999"}, nil},
		{"telegram", nil, nil},
		{"discord", map[string]any{"chat_id": "c1"}, "dc-bot"},
		{"slack", map[string]any{"chat_id": "anything"}, nil},
		{"matrix", map[string]any{"chat_id": "123"}, nil},
	}
	for _, c := range cases {
		if got := a.Get(ctx, c.platform, c.target, nil); got != c.want {
			t.Fatalf("%s %v -> %v", c.platform, c.target, got)
		}
	}
	primary["telegram"] = "mutated"
	if got := a.Get(ctx, "telegram", map[string]any{"chat_id": "123"}, nil); got != "tg-bot" {
		t.Fatalf("adapter map must be copied, got %v", got)
	}
}

func TestPrimaryProfileRoutesForCurrentHome(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", root)
	ops := filepath.Join(root, "profiles", "ops")
	if err := os.MkdirAll(ops, 0o700); err != nil {
		t.Fatal(err)
	}
	yaml := "gateway:\n  profile_routes:\n" +
		"    - {platform: telegram, profile: ops, chat_id: '123'}\n" +
		"    - {platform: discord, profile: other, chat_id: '9'}\n" +
		"    - {platform: slack, profile: ops, chat_id: '5', enabled: false}\n"
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := primaryProfileRoutesForCurrentHome(context.Background()); len(got) != 0 {
		t.Fatalf("primary home consults nothing, got %d", len(got))
	}
	got := primaryProfileRoutesForCurrentHome(home.WithOverride(context.Background(), ops))
	if len(got) != 1 || deref(got[0].ChatID) != "123" {
		t.Fatalf("routes = %+v", got)
	}
}
