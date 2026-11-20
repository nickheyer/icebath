package discord

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func patchRawConfig(t *testing.T, cfg map[string]any, err error) {
	t.Helper()
	orig := readRawConfig
	readRawConfig = func(context.Context) (map[string]any, error) { return cfg, err }
	t.Cleanup(func() { readRawConfig = orig })
}

func promptTimeout(t *testing.T) int {
	t.Helper()
	got, err := ReadDiscordPromptTimeout(context.Background())
	if err != nil {
		t.Fatalf("ReadDiscordPromptTimeout: %v", err)
	}
	return got
}

func TestDiscordPromptTimeoutReadsAndClamps(t *testing.T) {
	cases := []struct {
		raw  any
		want int
	}{
		{int64(600), 600},
		{"450", 450},
		{"five minutes", DiscordPromptTimeoutDefault},
		{int64(5), DiscordPromptTimeoutMin},
		{int64(3600), 3600},
		{int64(DiscordPromptTimeoutMax) + 1, DiscordPromptTimeoutMax},
		{"", DiscordPromptTimeoutDefault},
		{nil, DiscordPromptTimeoutDefault},
		{90.9, 90},
		{true, DiscordPromptTimeoutMin},
		{math.NaN(), DiscordPromptTimeoutDefault},
		{[]any{1}, DiscordPromptTimeoutDefault},
	}
	for _, c := range cases {
		patchRawConfig(t, map[string]any{"approvals": map[string]any{"discord_prompt_timeout": c.raw}}, nil)
		if got := promptTimeout(t); got != c.want {
			t.Fatalf("discord_prompt_timeout %v = %d want %d", c.raw, got, c.want)
		}
	}
	if DiscordPromptTimeoutMax != 31536000 {
		t.Fatalf("max = %d", DiscordPromptTimeoutMax)
	}
	for _, cfg := range []map[string]any{nil, {"approvals": nil}, {"approvals": "flat"}, {"approvals": []any{1}}} {
		patchRawConfig(t, cfg, nil)
		if got := promptTimeout(t); got != DiscordPromptTimeoutDefault {
			t.Fatalf("config %v = %d", cfg, got)
		}
	}
	patchRawConfig(t, nil, errors.New("unreadable"))
	if got := promptTimeout(t); got != DiscordPromptTimeoutDefault {
		t.Fatalf("read failure = %d", got)
	}
	patchRawConfig(t, map[string]any{"approvals": map[string]any{"discord_prompt_timeout": math.Inf(1)}}, nil)
	if _, err := ReadDiscordPromptTimeout(context.Background()); err == nil || err.Error() != "OverflowError: cannot convert float infinity to integer" {
		t.Fatalf("infinity = %v", err)
	}
}

func TestDiscordPromptTimeoutConfiguredThroughTheProfilesConfigYAML(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	tree, err := config.Load(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := tree.Int("approvals.discord_prompt_timeout", -1); got != DiscordPromptTimeoutDefault {
		t.Fatalf("registered default = %d", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("approvals:\n  discord_prompt_timeout: 1800\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
	if got := promptTimeout(t); got != 1800 {
		t.Fatalf("config.yaml timeout = %d", got)
	}
	tree, err = config.Load(dir, nil)
	if err != nil || tree.Int("approvals.discord_prompt_timeout", -1) != 1800 {
		t.Fatalf("loaded config = %v %v", tree.Get("approvals"), err)
	}
}

func TestDMRoleAuthGuildIsOptIn(t *testing.T) {
	cases := []struct {
		discord any
		want    string
	}{
		{map[string]any{"dm_role_auth_guild": int64(123456789012345678)}, "123456789012345678"},
		{map[string]any{"dm_role_auth_guild": " 42 "}, "42"},
		{map[string]any{"dm_role_auth_guild": ""}, ""},
		{map[string]any{"dm_role_auth_guild": int64(0)}, ""},
		{map[string]any{"dm_role_auth_guild": int64(-5)}, ""},
		{map[string]any{"dm_role_auth_guild": "guild"}, ""},
		{map[string]any{}, ""},
		{nil, ""},
		{"flat", ""},
	}
	for _, c := range cases {
		patchRawConfig(t, map[string]any{"discord": c.discord}, nil)
		got, err := ReadDMRoleAuthGuild(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if (c.want == "" && got != nil) || (c.want != "" && (got == nil || got.String() != c.want)) {
			t.Fatalf("discord %v = %v", c.discord, got)
		}
	}
	patchRawConfig(t, nil, errors.New("unreadable"))
	if got, err := ReadDMRoleAuthGuild(context.Background()); got != nil || err != nil {
		t.Fatalf("read failure = %v %v", got, err)
	}
}

func TestExecApprovalAdminGateResolverParsesAdmins(t *testing.T) {
	requireAdmin, admins := ResolveExecApprovalAdminGate(map[string]any{"require_admin_for_exec_approval": true, "allow_admin_from": "111, 222"})
	want := map[string]struct{}{"111": {}, "222": {}}
	if !requireAdmin || !reflect.DeepEqual(admins, want) {
		t.Fatalf("string admins = %v %v", requireAdmin, admins)
	}
	if _, listed := ResolveExecApprovalAdminGate(map[string]any{"require_admin_for_exec_approval": "true", "allow_admin_from": []any{111, 222}}); !reflect.DeepEqual(listed, want) {
		t.Fatalf("list admins = %v", listed)
	}
	for _, extra := range []map[string]any{nil, {}, {"require_admin_for_exec_approval": "off", "allow_admin_from": "1"}, {"require_admin_for_exec_approval": int64(0)}} {
		if on, ids := ResolveExecApprovalAdminGate(extra); on || len(ids) != 0 {
			t.Fatalf("gate off %v = %v %v", extra, on, ids)
		}
	}
	if on, ids := ResolveExecApprovalAdminGate(map[string]any{"require_admin_for_exec_approval": " YES "}); !on || len(ids) != 0 {
		t.Fatalf("fail closed = %v %v", on, ids)
	}
	if on, _ := ResolveExecApprovalAdminGate(map[string]any{"require_admin_for_exec_approval": int64(1)}); !on {
		t.Fatalf("numeric toggle")
	}
}
