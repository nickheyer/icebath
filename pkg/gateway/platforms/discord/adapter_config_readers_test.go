package discord

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Adapter in a fresh home whose config.yaml holds the given text
func configAdapter(t *testing.T, yaml string) *DiscordAdapter {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return newTestAdapter(t, nil)
}

func TestDiscordPromptTimeoutReadsAndClamps(t *testing.T) {
	def := settings.ApprovalsDiscordPromptTimeout.Default.Seconds()
	cases := []struct {
		raw  string
		want float64
	}{
		{"600", 600},
		{"'450'", 450},
		{"five minutes", def},
		{"5", DiscordPromptTimeoutMin},
		{"3600", 3600},
		{strconv.Itoa(DiscordPromptTimeoutMax + 1), float64(DiscordPromptTimeoutMax)},
		{"''", def},
		{"", def},
		{"90.5", 90.5},
		{"true", def},
		{"[1]", def},
	}
	for _, c := range cases {
		a := configAdapter(t, "approvals:\n  discord_prompt_timeout: "+c.raw+"\n")
		if got := a.promptTimeoutSeconds(context.Background()); got != c.want {
			t.Fatalf("discord_prompt_timeout %q = %v want %v", c.raw, got, c.want)
		}
	}
	if DiscordPromptTimeoutMax != 31536000 {
		t.Fatalf("max = %d", DiscordPromptTimeoutMax)
	}
	for _, yaml := range []string{"", "approvals:\n", "approvals: flat\n", "approvals: [1]\n"} {
		if got := configAdapter(t, yaml).promptTimeoutSeconds(context.Background()); got != def {
			t.Fatalf("config %q = %v", yaml, got)
		}
	}
}

func TestDiscordPromptTimeoutConfiguredThroughTheProfilesConfigYAML(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	a := newTestAdapter(t, nil)
	def := int(settings.ApprovalsDiscordPromptTimeout.Default.Seconds())
	tree, err := config.Load(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := tree.Int("approvals.discord_prompt_timeout", -1); got != def || a.promptTimeoutSeconds(context.Background()) != float64(def) {
		t.Fatalf("registered default = %d", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("approvals:\n  discord_prompt_timeout: 1800\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
	if got := a.promptTimeoutSeconds(context.Background()); got != 1800 {
		t.Fatalf("config.yaml timeout = %v", got)
	}
	tree, err = config.Load(dir, nil)
	if err != nil || tree.Int("approvals.discord_prompt_timeout", -1) != 1800 {
		t.Fatalf("loaded config = %v %v", tree.Get("approvals"), err)
	}
}

func TestDMRoleAuthGuildIsOptIn(t *testing.T) {
	cases := []struct {
		yaml string
		want int64
	}{
		{"discord:\n  dm_role_auth_guild: 123456789012345678\n", 123456789012345678},
		{"discord:\n  dm_role_auth_guild: ' 42 '\n", 42},
		{"discord:\n  dm_role_auth_guild: ''\n", 0},
		{"discord:\n  dm_role_auth_guild: 0\n", 0},
		{"discord:\n  dm_role_auth_guild: -5\n", 0},
		{"discord:\n  dm_role_auth_guild: guild\n", 0},
		{"discord:\n  dm_role_auth_guild: 99999999999999999999\n", 0},
		{"discord: {}\n", 0},
		{"discord:\n", 0},
		{"discord: flat\n", 0},
		{"", 0},
	}
	for _, c := range cases {
		if got := configAdapter(t, c.yaml).dmRoleAuthGuild(context.Background()); got != c.want {
			t.Fatalf("config %q = %d, want %d", c.yaml, got, c.want)
		}
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
