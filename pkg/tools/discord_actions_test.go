package tools

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestDiscordToolsDriveTheRestAPI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	t.Setenv("DISCORD_BOT_TOKEN", "")
	env := &Env{Home: home, Values: &sync.Map{}}
	ctx := context.Background()
	discordResetCapabilityCache()
	if out := discordRunAction(ctx, env, "fetch_messages", true, "discord", nil); !strings.Contains(out, "DISCORD_BOT_TOKEN not configured.") {
		t.Fatalf("no token %s", out)
	}
	t.Setenv("DISCORD_BOT_TOKEN", "bot-a")

	var mu sync.Mutex
	var seen []string
	replies := map[string]struct {
		status int
		body   string
	}{
		"/channels/c1/messages": {200, `[{"id":"m1","content":"hi","author":{"id":"u1","username":"ann"},"reactions":[{"emoji":{"name":"x"},"count":2}]}]`},
		"/guilds/g1/channels":   {200, `[{"id":"k1","type":4,"name":"Cat","position":1},{"id":"t2","type":0,"name":"b","position":2,"parent_id":"k1"},{"id":"t1","type":0,"name":"a","position":1,"parent_id":"k1"},{"id":"v","type":2,"name":"loose"}]`},
		"/channels/c1/pins/m1":  {403, `{"message":"Missing Permissions"}`},
		"/applications/@me":     {200, `{"flags":0}`},
	}
	old := discordHTTPDo
	discordHTTPDo = func(req *http.Request, _ time.Duration) (*http.Response, error) {
		mu.Lock()
		seen = append(seen, req.Method+" "+req.URL.Path+"?"+req.URL.RawQuery+" "+req.Header.Get("Authorization"))
		mu.Unlock()
		r, ok := replies[strings.TrimPrefix(req.URL.Path, "/api/v10")]
		if !ok {
			r.status, r.body = 404, `{"message":"Unknown"}`
		}
		return &http.Response{StatusCode: r.status, Body: io.NopCloser(bytes.NewReader([]byte(r.body))), Header: http.Header{}}, nil
	}
	t.Cleanup(func() { discordHTTPDo = old })

	out := discordRunAction(ctx, env, "fetch_messages", true, "discord", map[string]any{"channel_id": "c1", "before": "m9", "limit": int64(500)})
	if out != `{"messages": [{"id": "m1", "content": "hi", "author": {"id": "u1", "username": "ann", "display_name": null, "bot": false}, "timestamp": null, "edited_timestamp": null, "attachments": [], "reactions": [{"emoji": "x", "count": 2}], "pinned": false}], "count": 1}` {
		t.Fatalf("fetch_messages %s", out)
	}
	if seen[0] != "GET /api/v10/channels/c1/messages?limit=100&before=m9 Bot bot-a" {
		t.Fatalf("request %s", seen[0])
	}
	out = discordRunAction(ctx, env, "list_channels", false, "discord_admin", map[string]any{"guild_id": "g1"})
	if !strings.Contains(out, `"channel_groups": [{"category": null, "channels": [{"id": "v", "name": "loose", "type": "voice"`) ||
		!strings.Contains(out, `{"category": {"id": "k1", "name": "Cat"}, "channels": [{"id": "t1"`) || !strings.HasSuffix(out, `"total_channels": 3}`) {
		t.Fatalf("list_channels %s", out)
	}
	if out = discordRunAction(ctx, env, "pin_message", false, "discord_admin", map[string]any{"channel_id": "c1", "message_id": "m1"}); !strings.Contains(out, "Discord API 403 (forbidden) on 'pin_message'. Bot lacks MANAGE_MESSAGES") {
		t.Fatalf("403 %s", out)
	}
	if out = discordRunAction(ctx, env, "create_thread", true, "discord", map[string]any{"channel_id": "c1"}); !strings.Contains(out, "Missing required parameters for 'create_thread': name") {
		t.Fatalf("missing %s", out)
	}
	if out = discordRunAction(ctx, env, "list_roles", true, "discord", nil); !strings.Contains(out, "Unknown action: list_roles") || !strings.Contains(out, `"available_actions": ["search_members", "fetch_messages", "create_thread"]`) {
		t.Fatalf("unknown %s", out)
	}
	env.Config = config.Tree{"discord": map[string]any{"server_actions": "fetch_messages, bogus"}}
	if out = discordRunAction(ctx, env, "search_members", true, "discord", map[string]any{"guild_id": "g", "query": "a"}); !strings.Contains(out, "Action 'search_members' is disabled by config (discord.server_actions). Allowed: fetch_messages") {
		t.Fatalf("allowlist %s", out)
	}
	path := discordCapabilityDiskCachePath(env)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	seeded := `{"` + discordTokenCacheKey("bot-a") + `": {"caps": {"has_members_intent": true, "has_message_content": true, "detected": true}, "ts": 9999999999}}`
	if err := os.WriteFile(path, []byte(seeded), 0o600); err != nil {
		t.Fatal(err)
	}
	if schema := discordGetDynamicSchema(env, true, "discord"); schema == nil || !strings.Contains(schema["description"].(string), "fetch_messages(channel_id)") || strings.Contains(schema["description"].(string), "search_members(") {
		t.Fatalf("allowlisted schema %#v", schema)
	}
	env.Config = nil

	t.Setenv("DISCORD_BOT_TOKEN", "bot-b")
	cache := `{"` + discordTokenCacheKey("bot-b") + `": {"caps": {"has_members_intent": false, "has_message_content": false, "detected": true}, "ts": 9999999999}}`
	if err := os.WriteFile(path, []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}
	schema := discordGetDynamicSchema(env, true, "discord")
	desc := schema["description"].(string)
	if strings.Contains(desc, "search_members(") || !strings.Contains(desc, "NOTE: Bot does NOT have the MESSAGE_CONTENT privileged intent. fetch_messages will return") {
		t.Fatalf("intent filtered schema %s", desc)
	}
	if td := discordRewriter(false, "discord_admin")(env, nil, nil); td == nil || td["function"].(map[string]any)["name"] != "discord_admin" {
		t.Fatalf("admin rewrite %#v", td)
	}

	t.Setenv("DISCORD_BOT_TOKEN", "bot-c")
	if caps := discordDetectCapabilitiesNonblocking(env, "bot-c"); caps.GetOr("detected", nil) != false {
		t.Fatalf("cold start caps %v", caps)
	}
	deadline := time.Now().Add(5 * time.Second)
	for discordLoadCapsFromDisk(env, "bot-c") == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if caps := discordLoadCapsFromDisk(env, "bot-c"); caps == nil || caps.GetOr("has_members_intent", nil) != false || caps.GetOr("detected", nil) != true {
		t.Fatalf("background detection saved %v", caps)
	}
	if caps := discordDetectCapabilities(env, "bot-c", false); caps.GetOr("detected", nil) != false {
		t.Fatalf("cached blocking caps %v", caps)
	}
	if caps := discordDetectCapabilities(env, "bot-c", true); caps.GetOr("detected", nil) != true || caps.GetOr("has_message_content", nil) != false {
		t.Fatalf("forced blocking caps %v", caps)
	}
	if caps := discordDetectCapabilitiesNonblocking(env, "bot-c"); caps.GetOr("detected", nil) != true {
		t.Fatalf("forced caps reach the schema lookup %v", caps)
	}
	discordResetCapabilityCache()
	if caps := discordDetectCapabilities(env, "bot-d", false); caps.GetOr("detected", nil) != true || caps.GetOr("has_members_intent", nil) != false {
		t.Fatalf("first blocking caps %v", caps)
	}
}
