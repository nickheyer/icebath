package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

func route(name, platform, profile string, set func(r *ProfileRoute)) *ProfileRoute {
	r := &ProfileRoute{Name: name, Platform: platform, Profile: profile, Enabled: true}
	if set != nil {
		set(r)
	}
	return r
}

func mustMatch(t *testing.T, r *ProfileRoute, q RouteQuery) bool {
	t.Helper()
	ok, err := r.Matches(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func mustRoutes(t *testing.T, raw ...map[string]any) []*ProfileRoute {
	t.Helper()
	items := make([]any, len(raw))
	for i, r := range raw {
		items[i] = r
	}
	routes, err := ParseProfileRoutes(context.Background(), items)
	if err != nil {
		t.Fatal(err)
	}
	return routes
}

func matched(t *testing.T, ctx context.Context, routes []*ProfileRoute, q RouteQuery) *ProfileRoute {
	t.Helper()
	r, err := MatchProfileRoute(ctx, routes, q)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestProfileRouteMatching(t *testing.T) {
	th := route("t", "discord", "trader", func(r *ProfileRoute) { r.GuildID, r.ChatID, r.ThreadID = ptr("111"), ptr("222"), ptr("333") })
	if !mustMatch(t, th, RouteQuery{Platform: "discord", GuildID: ptr("111"), ChatID: ptr("222"), ThreadID: ptr("333")}) ||
		mustMatch(t, th, RouteQuery{Platform: "discord", GuildID: ptr("111"), ChatID: ptr("222"), ThreadID: ptr("444")}) {
		t.Fatal("exact thread match")
	}
	gc := route("gc", "discord", "scoped", func(r *ProfileRoute) { r.GuildID, r.ChatID = ptr("111"), ptr("222") })
	if !mustMatch(t, gc, RouteQuery{Platform: "discord", GuildID: ptr("111"), ChatID: ptr("222")}) ||
		!mustMatch(t, gc, RouteQuery{Platform: "discord", GuildID: ptr("111"), ChatID: ptr("333"), ParentChatID: ptr("222")}) ||
		mustMatch(t, gc, RouteQuery{Platform: "discord", GuildID: ptr("999"), ChatID: ptr("222")}) ||
		mustMatch(t, gc, RouteQuery{Platform: "discord", GuildID: ptr("111"), ChatID: ptr("333")}) {
		t.Fatal("guild and chat are conjunctive")
	}
	sender := route("sender", "teams", "owner", func(r *ProfileRoute) { r.UserID = ptr("aad-456") })
	if !mustMatch(t, sender, RouteQuery{Platform: "teams", UserID: ptr("aad-456")}) ||
		mustMatch(t, sender, RouteQuery{Platform: "teams", UserID: ptr("AAD-456")}) ||
		mustMatch(t, sender, RouteQuery{Platform: "teams"}) {
		t.Fatal("user id matches exactly")
	}
	senderChat := route("sender-chat", "teams", "owner", func(r *ProfileRoute) { r.UserID, r.ChatID = ptr("aad-456"), ptr("conversation-1") })
	if !mustMatch(t, senderChat, RouteQuery{Platform: "teams", UserID: ptr("aad-456"), ChatID: ptr("conversation-1")}) ||
		mustMatch(t, senderChat, RouteQuery{Platform: "teams", UserID: ptr("aad-456"), ChatID: ptr("conversation-2")}) {
		t.Fatal("user id is conjunctive with chat id")
	}
	owner := route("owner", "telegram", "owner", func(r *ProfileRoute) { r.UserID, r.BotProfile = ptr("640466638"), ptr("team_b") })
	if !mustMatch(t, owner, RouteQuery{Platform: "telegram", UserID: ptr("640466638"), AdapterProfile: ptr("team_b")}) ||
		mustMatch(t, owner, RouteQuery{Platform: "telegram", UserID: ptr("640466638")}) ||
		mustMatch(t, owner, RouteQuery{Platform: "telegram", UserID: ptr("other"), AdapterProfile: ptr("team_b")}) {
		t.Fatal("sender route cannot cross the receiving bot boundary")
	}
	routes := mustRoutes(t,
		map[string]any{"name": "shared", "platform": "teams", "profile": "shared", "chat_id": "shared-chat"},
		map[string]any{"name": "alice", "platform": "teams", "profile": "alice", "chat_id": "shared-chat", "user_id": "user-a"},
		map[string]any{"name": "bob", "platform": "teams", "profile": "bob", "user_id": "user-b"})
	for user, profile := range map[string]string{"user-a": "alice", "user-b": "bob", "user-c": "shared"} {
		if got := matched(t, context.Background(), routes, RouteQuery{Platform: "teams", ChatID: ptr("shared-chat"), UserID: ptr(user)}); got.Profile != profile {
			t.Fatalf("%s routed to %s", user, got.Profile)
		}
	}
}

func TestParseProfileRoutes(t *testing.T) {
	ctx := context.Background()
	for _, raw := range []any{nil, []any{}} {
		if routes, err := ParseProfileRoutes(ctx, raw); err != nil || len(routes) != 0 {
			t.Fatalf("empty %v %v", routes, err)
		}
	}
	routes := mustRoutes(t,
		map[string]any{"name": "server", "platform": "discord", "profile": "p", "guild_id": 111, "chat_id": 222, "thread_id": 333, "user_id": 444},
		map[string]any{"name": "tg", "platform": "telegram", "profile": "p", "chat_id": -1001234567890},
		map[string]any{"name": "platform-only", "platform": "discord", "profile": "p"})
	byName := map[any]*ProfileRoute{}
	for _, r := range routes {
		byName[r.Name] = r
	}
	s := byName["server"]
	if *s.GuildID != "111" || *s.ChatID != "222" || *s.ThreadID != "333" || *s.UserID != "444" {
		t.Fatalf("server ids %+v", s)
	}
	if matched(t, ctx, routes, RouteQuery{Platform: "discord", GuildID: ptr("111"), ChatID: ptr("222"), ThreadID: ptr("333"), UserID: ptr("444")}).Name != "server" ||
		matched(t, ctx, routes, RouteQuery{Platform: "telegram", ChatID: ptr("-1001234567890")}).Name != "tg" {
		t.Fatal("yaml native int ids match")
	}
	if po := byName["platform-only"]; po.GuildID != nil || po.ChatID != nil || po.ThreadID != nil {
		t.Fatalf("platform only %+v", po)
	}
	for _, invalid := range []any{nil, "", "   "} {
		logs := captureLogs(t)
		routes := mustRoutes(t,
			map[string]any{"name": "invalid", "platform": "teams", "profile": "owner", "user_id": invalid},
			map[string]any{"name": "missing", "platform": "teams", "profile": "shared"})
		if len(routes) != 1 || routes[0].Name != "missing" || matched(t, ctx, routes, RouteQuery{Platform: "teams", UserID: ptr("anyone")}).Name != "missing" {
			t.Fatalf("invalid user id %v %+v", invalid, routes)
		}
		if !strings.Contains(logs.String(), "user_id cannot be null or empty") {
			t.Fatalf("log %q", logs.String())
		}
		if s, ok := invalid.(string); ok && mustMatch(t, route("direct", "teams", "owner", func(r *ProfileRoute) { r.UserID = ptr(s) }), RouteQuery{Platform: "teams", UserID: ptr(s)}) {
			t.Fatalf("blank user id %q matched", s)
		}
	}
	logs := captureLogs(t)
	routes = mustRoutes(t,
		map[string]any{"name": "f", "platform": "discord", "profile": "p", "chat_id": 123.0},
		map[string]any{"name": "b", "platform": "discord", "profile": "p", "guild_id": true})
	if len(routes) != 2 || matched(t, ctx, routes, RouteQuery{Platform: "discord", ChatID: ptr("123")}) != nil {
		t.Fatalf("non int ids %+v", routes)
	}
	if n := strings.Count(logs.String(), "can never match"); n != 2 {
		t.Fatalf("%d warnings in %q", n, logs.String())
	}
}

func TestMatchProfileRoute(t *testing.T) {
	ctx := context.Background()
	routes := mustRoutes(t,
		map[string]any{"name": "thread", "platform": "discord", "profile": "thread", "guild_id": "g", "chat_id": "c", "thread_id": "t"},
		map[string]any{"name": "sender", "platform": "discord", "profile": "sender", "user_id": "u"},
		map[string]any{"name": "sender-chat", "platform": "discord", "profile": "sender-chat", "user_id": "u", "chat_id": "c"})
	for _, c := range []struct{ chat, user, want string }{{"c", "u", "sender-chat"}, {"other", "u", "sender"}, {"c", "other", "thread"}} {
		got := matched(t, ctx, routes, RouteQuery{Platform: "discord", GuildID: ptr("g"), ChatID: ptr(c.chat), ThreadID: ptr("t"), UserID: ptr(c.user)})
		if got.Profile != c.want {
			t.Fatalf("%+v routed to %s", c, got.Profile)
		}
	}
	if matched(t, ctx, []*ProfileRoute{route("r", "telegram", "p", nil)}, RouteQuery{Platform: "discord"}) != nil {
		t.Fatal("no match returns nil")
	}
	if !mustMatch(t, route("ch", "discord", "trader", func(r *ProfileRoute) { r.ChatID = ptr("222") }), RouteQuery{Platform: "discord", ChatID: ptr("333"), ParentChatID: ptr("222")}) {
		t.Fatal("channel route matches via parent chat id")
	}
	forum := []*ProfileRoute{
		route("forum", "discord", "forum_profile", func(r *ProfileRoute) { r.ChatID = ptr("forum_channel_123") }),
		route("post", "discord", "post_profile", func(r *ProfileRoute) { r.ThreadID = ptr("post_thread_456") }),
	}
	if m := matched(t, ctx, forum, RouteQuery{Platform: "discord", ChatID: ptr("post_thread_456"), ParentChatID: ptr("forum_channel_123")}); m == nil || m.Profile != "forum_profile" {
		t.Fatalf("forum post %+v", m)
	}
}

// Home whose whatsapp session dir maps the phone to the LID and back
func lidMappingHome(t *testing.T, phone, phoneTarget, lid, lidTarget string) context.Context {
	t.Helper()
	h := t.TempDir()
	dir := filepath.Join(h, "platforms", "whatsapp", "session")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, target string) {
		data, _ := json.Marshal(target)
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if phone != "" {
		write("lid-mapping-"+phone+".json", phoneTarget)
	}
	if lid != "" {
		write("lid-mapping-"+lid+"_reverse.json", lidTarget)
	}
	t.Setenv("ICEBATH_HOME", h)
	return home.WithOverride(context.Background(), h)
}

func TestWhatsAppChatIDIdentityMatching(t *testing.T) {
	const phone, lid = "15551234567", "999999999999999"
	ctx := lidMappingHome(t, phone, lid+"@lid", lid, phone+"@s.whatsapp.net")
	match := func(r *ProfileRoute, q RouteQuery) bool {
		ok, err := r.Matches(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	for _, platform := range []string{"whatsapp", "whatsapp_cloud"} {
		r := route("owner", platform, "owner", func(r *ProfileRoute) { r.ChatID = ptr(phone) })
		for _, chat := range []string{phone + "@s.whatsapp.net", phone + ":47@s.whatsapp.net", lid + "@lid"} {
			if !match(r, RouteQuery{Platform: platform, ChatID: ptr(chat)}) {
				t.Fatalf("%s %s", platform, chat)
			}
		}
		if !match(r, RouteQuery{Platform: platform, ChatID: ptr("thread-1"), ParentChatID: ptr(lid + "@lid")}) ||
			match(r, RouteQuery{Platform: platform, ChatID: ptr("15550001111@s.whatsapp.net")}) {
			t.Fatalf("%s parent and stranger", platform)
		}
	}
	group := "120363012345678901@g.us"
	owner := route("owner", "whatsapp", "owner", func(r *ProfileRoute) { r.ChatID = ptr(phone) })
	grp := route("grp", "whatsapp", "grp", func(r *ProfileRoute) { r.ChatID = ptr(group) })
	oops := route("oops", "whatsapp", "owner", func(r *ProfileRoute) { r.ChatID = ptr(strings.SplitN(group, "@", 2)[0]) })
	tg := route("tg", "telegram", "owner", func(r *ProfileRoute) { r.ChatID = ptr("640466638") })
	if match(owner, RouteQuery{Platform: "whatsapp", ChatID: ptr(group)}) ||
		!match(grp, RouteQuery{Platform: "whatsapp", ChatID: ptr(group)}) ||
		match(grp, RouteQuery{Platform: "whatsapp", ChatID: ptr(phone + "@s.whatsapp.net")}) ||
		match(oops, RouteQuery{Platform: "whatsapp", ChatID: ptr(group)}) ||
		!match(tg, RouteQuery{Platform: "telegram", ChatID: ptr("640466638")}) ||
		match(tg, RouteQuery{Platform: "telegram", ChatID: ptr("640466638@s.whatsapp.net")}) {
		t.Fatal("groups and other platforms stay exact")
	}
}

func TestProfileRoutesSurviveGatewayConfigRoundtrip(t *testing.T) {
	ctx := context.Background()
	cfg := DefaultGatewayConfig(ctx)
	cfg.ProfileRoutes = mustRoutes(t,
		map[string]any{"name": "sender", "platform": "teams", "profile": "owner", "user_id": "aad-456"},
		map[string]any{"name": "off", "platform": "teams", "profile": "owner", "chat_id": "conversation-1", "enabled": false})
	raw := cfg.ToDict()
	if _, ok := raw["profile_routes"].([]any)[1].(map[string]any)["user_id"]; ok {
		t.Fatal("user_id emitted for a route without one")
	}
	restored, err := GatewayConfigFromDict(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	var got [][4]any
	for _, r := range restored.ProfileRoutes {
		var user any
		if r.UserID != nil {
			user = *r.UserID
		}
		got = append(got, [4]any{r.Name, user, r.Enabled, r.Specificity()})
	}
	want := [][4]any{{"sender", "aad-456", true, 16}, {"off", nil, false, 4}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestWhatsAppAliasesAndJIDs(t *testing.T) {
	ctx := lidMappingHome(t, "", "", "", "")
	h := home.Dir(ctx)
	dir := filepath.Join(h, "platforms", "whatsapp", "session")
	data, _ := json.Marshal("15551234567@s.whatsapp.net")
	if err := os.WriteFile(filepath.Join(dir, "lid-mapping-999999999999999.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	aliases, err := expandWhatsAppAliases(ctx, "999999999999999@lid")
	if err != nil || !reflect.DeepEqual(aliases, map[string]bool{"999999999999999": true, "15551234567": true}) {
		t.Fatalf("%v %v", aliases, err)
	}
	for raw, want := range map[string]string{
		"+50766715226": "50766715226@s.whatsapp.net", "50766715226": "50766715226@s.whatsapp.net",
		"+1 (555) 123-4567": "15551234567@s.whatsapp.net", "+1.555.123.4567": "15551234567@s.whatsapp.net",
	} {
		if got := ToWhatsAppJID(raw); got != want {
			t.Fatalf("%q -> %q", raw, got)
		}
	}
	for _, jid := range []string{"50766715226@s.whatsapp.net", "123456789-987654321@g.us", "130631430344750@lid", "status@broadcast", "123@newsletter"} {
		if got := ToWhatsAppJID(jid); got != jid {
			t.Fatalf("%q -> %q", jid, got)
		}
	}
}
