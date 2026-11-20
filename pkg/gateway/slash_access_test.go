package gateway

import (
	"reflect"
	"testing"
)

func slashSet(items ...string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, s := range items {
		out[s] = struct{}{}
	}
	return out
}

func TestPolicyFromExtraDisabledPolicyTreatsAnyoneAsAdmin(t *testing.T) {
	p := PolicyFromExtra(map[string]any{}, "dm")
	if !p.IsAdmin("anyone") || !p.CanRun("anyone", "stop") {
		t.Fatalf("disabled policy = %+v", p)
	}
}

func TestPolicyFromExtraIDCoercionIntsBecomeStrings(t *testing.T) {
	p := PolicyFromExtra(map[string]any{"allow_admin_from": []any{12345, int64(67890)}}, "dm")
	if !reflect.DeepEqual(p.AdminUserIDs, slashSet("12345", "67890")) || !p.IsAdmin("12345") || !p.IsAdmin(12345) {
		t.Fatalf("ids = %v", p.AdminUserIDs)
	}
	id := "67890"
	if !p.IsAdmin(&id) || p.IsAdmin((*string)(nil)) || p.IsAdmin("") || p.IsAdmin(nil) {
		t.Fatalf("pointer and empty ids misread")
	}
}

func TestPolicyFromExtraCommandCoercionStripsLeadingSlashAndLowercases(t *testing.T) {
	p := PolicyFromExtra(map[string]any{"allow_admin_from": []any{"111"}, "user_allowed_commands": []any{"/Status", "MODEL", "/help"}}, "dm")
	if !reflect.DeepEqual(p.UserAllowedCommands, slashSet("status", "model", "help")) {
		t.Fatalf("commands = %v", p.UserAllowedCommands)
	}
}

func TestPolicyFromExtraDMAdminDoesNotImplyGroupAdmin(t *testing.T) {
	if dm := PolicyFromExtra(map[string]any{"allow_admin_from": []any{"111"}}, "dm"); !dm.IsAdmin("111") {
		t.Fatalf("dm admin = %+v", dm)
	}
	gp := PolicyFromExtra(map[string]any{"allow_admin_from": []any{"111"}, "group_allow_admin_from": []any{"222"}}, "group")
	if gp.IsAdmin("111") || !gp.IsAdmin("222") {
		t.Fatalf("group admin = %+v", gp)
	}
}

func TestCoerceSlashListShapes(t *testing.T) {
	cases := []struct {
		raw  any
		want map[string]struct{}
	}{
		{nil, slashSet()},
		{"111, 222,, ,333", slashSet("111", "222", "333")},
		{int64(42), slashSet("42")},
		{[]string{" a ", ""}, slashSet("a")},
		{[]any{nil, " "}, slashSet("None")},
	}
	for _, c := range cases {
		if got := CoerceIDList(c.raw); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("CoerceIDList(%v) = %v", c.raw, got)
		}
	}
	if got := coerceCommandList("//Plan, /HELP"); !reflect.DeepEqual(got, slashSet("plan", "help")) {
		t.Fatalf("command string = %v", got)
	}
	dm := PolicyFromExtra(map[string]any{"allow_admin_from": "1", "group_user_allowed_commands": []any{"/Model"}}, "dm")
	if !reflect.DeepEqual(dm.UserAllowedCommands, slashSet("model")) || !dm.CanRun("2", "model") || dm.CanRun("2", "") || !dm.CanRun("2", "whoami") {
		t.Fatalf("dm command fallback = %+v", dm)
	}
	if odd := PolicyFromExtra(map[string]any{"allow_admin_from": "1"}, "thread"); !odd.Enabled {
		t.Fatalf("unknown scope reads the dm keys")
	}
}

func discordSlashConfig(extra map[string]any) *GatewayConfig {
	return &GatewayConfig{Platforms: map[Platform]*PlatformConfig{PlatformDiscord: {Enabled: true, Extra: extra}}}
}

func TestPolicyForSourceDMChatTypeResolvesToDMScope(t *testing.T) {
	cfg := discordSlashConfig(map[string]any{"allow_admin_from": []any{"111"}, "user_allowed_commands": []any{"status"},
		"group_allow_admin_from": []any{"222"}, "group_user_allowed_commands": []any{"help"}})
	user := "111"
	p := PolicyForSource(cfg, &SessionSource{Platform: PlatformDiscord, ChatID: "A", ChatType: "dm", UserID: &user})
	if !p.IsAdmin("111") || !p.CanRun("999", "status") || !p.CanRun("999", "help") || p.CanRun("999", "kanban") {
		t.Fatalf("dm scope = %+v", p)
	}
}

func TestPolicyForSourceNoAdminListForDMMeansUnrestrictedInDM(t *testing.T) {
	cfg := discordSlashConfig(map[string]any{"group_allow_admin_from": []any{"222"}})
	user := "999"
	dm := PolicyForSource(cfg, &SessionSource{Platform: PlatformDiscord, ChatID: "A", ChatType: "dm", UserID: &user})
	grp := PolicyForSource(cfg, &SessionSource{Platform: PlatformDiscord, ChatID: "G", ChatType: "group", UserID: &user})
	if dm.Enabled || !dm.CanRun("999", "stop") || !grp.Enabled || grp.CanRun("999", "stop") {
		t.Fatalf("dm %+v group %+v", dm, grp)
	}
	if p := PolicyForSource(nil, &SessionSource{}); p.Enabled {
		t.Fatalf("nil config is gated")
	}
	if p := PolicyForSource(cfg, nil); p.Enabled {
		t.Fatalf("nil source is gated")
	}
	if p := PolicyForSource(cfg, &SessionSource{Platform: PlatformTelegram, ChatType: "group"}); p.Enabled {
		t.Fatalf("unconfigured platform is gated")
	}
	if p := PolicyForSource(cfg, &SessionSource{Platform: PlatformDiscord, ChatType: " Private "}); p.Enabled {
		t.Fatalf("private reads the dm scope")
	}
}

func TestPolicyForSourceBlankChatTypeResolvesToTheGatedScope(t *testing.T) {
	cases := []struct {
		extra map[string]any
		admin string
		gated bool
	}{
		{map[string]any{"allow_admin_from": []any{"111"}}, "111", true},
		{map[string]any{"allow_admin_from": []any{"111"}}, "111", true},
		{map[string]any{"group_allow_admin_from": []any{"222"}}, "222", true},
		{map[string]any{"allow_admin_from": []any{"111"}, "group_allow_admin_from": []any{"222"}}, "222", true},
		{map[string]any{}, "", false},
	}
	for i, c := range cases {
		user := "999"
		p := PolicyForSource(discordSlashConfig(c.extra), &SessionSource{Platform: PlatformDiscord, ChatID: "A", ChatType: "", UserID: &user})
		if p.Enabled != c.gated || p.CanRun("999", "stop") != !c.gated || (c.admin != "" && !p.IsAdmin(c.admin)) {
			t.Fatalf("case %d = %+v", i, p)
		}
	}
}

type fakeSlashRunner struct {
	config   *GatewayConfig
	primary  string
	profiles map[string]*GatewayConfig
}

func (r fakeSlashRunner) RunnerConfig() *GatewayConfig              { return r.config }
func (r fakeSlashRunner) PrimaryProfileName() string                { return r.primary }
func (r fakeSlashRunner) ProfileConfigs() map[string]*GatewayConfig { return r.profiles }

func TestPolicyForRunnerSourceFollowsTheTransportProfile(t *testing.T) {
	on := true
	launch := discordSlashConfig(map[string]any{})
	launch.MultiplexProfiles = &on
	work := discordSlashConfig(map[string]any{"group_allow_admin_from": []any{"7"}})
	runner := fakeSlashRunner{config: launch, profiles: map[string]*GatewayConfig{"work": work}}
	src := &SessionSource{Platform: PlatformDiscord, ChatType: "group"}
	if p := PolicyForRunnerSource(runner, src); p.Enabled {
		t.Fatalf("unowned source reads the launch profile")
	}
	src.identity = &RoutingIdentity{TransportProfile: "work"}
	if p := PolicyForRunnerSource(runner, src); !p.Enabled || !p.IsAdmin("7") {
		t.Fatalf("owner profile policy = %+v", p)
	}
	src.identity = &RoutingIdentity{TransportProfile: "ghost"}
	if p := PolicyForRunnerSource(runner, src); !p.Enabled || len(p.AdminUserIDs) != 0 || !p.CanRun("1", "help") || p.CanRun("1", "stop") {
		t.Fatalf("uncached profile = %+v", p)
	}
	src.identity = &RoutingIdentity{TransportProfile: "default"}
	if p := PolicyForRunnerSource(runner, src); p.Enabled {
		t.Fatalf("primary owner reads the launch config")
	}
	profile := "work"
	if p := PolicyForRunnerSource(runner, &SessionSource{Platform: PlatformDiscord, ChatType: "group", Profile: &profile}); !p.IsAdmin("7") || p.IsAdmin("8") {
		t.Fatalf("source profile fallback = %+v", p)
	}
	off := false
	launch.MultiplexProfiles = &off
	if p := PolicyForRunnerSource(runner, src); p.Enabled {
		t.Fatalf("single profile runner = %+v", p)
	}
	if p := PolicyForRunnerSource(nil, src); p.Enabled {
		t.Fatalf("nil runner is gated")
	}
}
