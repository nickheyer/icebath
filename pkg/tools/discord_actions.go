package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Discord API failure with its HTTP status and body
func newDiscordAPIError(status int, body string) *DiscordAPIError {
	return &DiscordAPIError{Status: status, Body: body}
}

// Bot token of the active profile secret scope, empty when unset
func discordBotToken(env *Env) string { return secretValue(env, "DISCORD_BOT_TOKEN") }

var discordChannelTypeNames = map[int64]string{0: "text", 2: "voice", 4: "category", 5: "announcement", 10: "announcement_thread",
	11: "public_thread", 12: "private_thread", 13: "stage", 15: "forum", 16: "media"}

// Readable name of a Discord channel type id, unknown(id) for an id outside the table
func discordChannelTypeName(typeID any) string {
	if n, ok := typeID.(int64); ok {
		if name, known := discordChannelTypeNames[n]; known {
			return name
		}
	}
	return "unknown(" + pyrepr.Str(typeID) + ")"
}

// Day long disk cache of privileged intents, intents change only when the user flips them in the portal
const discordCapabilityDiskTTLSeconds = 24 * 3600

var (
	discordCapsMu        sync.Mutex
	discordCapsCache     = map[string]*pyjson.Object{}
	discordCapsBgStarted = map[string]bool{}
)

// All actions exposed while detection failed or is pending, call time 403s turned into guidance
func discordPermissiveCaps() *pyjson.Object {
	return pyjson.ObjectOf("has_members_intent", true, "has_message_content", true, "detected", false)
}

// Disk cache of detected capabilities under the profile cache folder
func discordCapabilityDiskCachePath(env *Env) string {
	return filepath.Join(envHome(env), "cache", "discord_capabilities.json")
}

// Stable non reversible cache key of a bot token
func discordTokenCacheKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:16]
}

// Disk cache contents keyed by token, empty when missing or corrupt
func discordReadCapsFile(path string) *pyjson.Object {
	data, err := os.ReadFile(path)
	if err != nil {
		return pyjson.NewObject()
	}
	text, err := pystr.DecodeUTF8(data)
	if err != nil {
		return pyjson.NewObject()
	}
	parsed, err := pyjson.Loads(strings.TrimPrefix(text, string(rune(0xFEFF))))
	if err != nil {
		return pyjson.NewObject()
	}
	if o, ok := parsed.(*pyjson.Object); ok {
		return o
	}
	return pyjson.NewObject()
}

// Fresh disk cached capabilities of the token, nil when stale, missing or malformed
func discordLoadCapsFromDisk(env *Env, token string) *pyjson.Object {
	entry, ok := discordReadCapsFile(discordCapabilityDiskCachePath(env)).GetOr(discordTokenCacheKey(token), nil).(*pyjson.Object)
	if !ok {
		return nil
	}
	ts, valid := portalFloat(entry.GetOr("ts", int64(0)))
	if !valid || portalNow()-ts > discordCapabilityDiskTTLSeconds {
		return nil
	}
	caps, ok := entry.GetOr("caps", nil).(*pyjson.Object)
	if !ok {
		return nil
	}
	if _, has := caps.Get("has_members_intent"); !has {
		return nil
	}
	return caps
}

// Writes the token's capabilities into the disk cache through a temp file, logging any failure
func discordSaveCapsToDisk(env *Env, token string, caps *pyjson.Object) {
	path := discordCapabilityDiskCachePath(env)
	err := func() error {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		data := discordReadCapsFile(path)
		data.Set(discordTokenCacheKey(token), pyjson.ObjectOf("caps", caps, "ts", portalNow()))
		text, err := pyjson.Dumps(data)
		if err != nil {
			return err
		}
		tmp := strings.TrimSuffix(path, ".json") + ".json.tmp"
		if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
			return err
		}
		return os.Rename(tmp, path)
	}()
	if err != nil {
		envLog(env).Debug("discord capability disk-cache write failed", "error", err)
	}
}

// Capabilities for a schema build from memory, then fresh disk, else the permissive default with a background detection
func discordDetectCapabilitiesNonblocking(env *Env, token string) *pyjson.Object {
	discordCapsMu.Lock()
	defer discordCapsMu.Unlock()
	if cached := discordCapsCache[token]; cached != nil {
		return cached
	}
	if disk := discordLoadCapsFromDisk(env, token); disk != nil {
		discordCapsCache[token] = disk
		return disk
	}
	capsDefault := discordPermissiveCaps()
	discordCapsCache[token] = capsDefault
	if !discordCapsBgStarted[token] {
		discordCapsBgStarted[token] = true
		go func() {
			caps := discordFetchCapabilities(env, token)
			if connTruthy(caps.GetOr("detected", nil)) {
				discordSaveCapsToDisk(env, token, caps)
			}
		}()
	}
	return capsDefault
}

// Capabilities from GET /applications/@me, permissive when detection fails, never touching the in process cache
func discordFetchCapabilities(env *Env, token string) *pyjson.Object {
	caps := discordPermissiveCaps()
	app, err := DiscordRequest(context.Background(), "GET", "/applications/@me", token, nil, nil, 5*time.Second)
	var flags int64
	if err == nil {
		o, isDict := app.(*pyjson.Object)
		if !isDict {
			err = fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(app))
		} else {
			flags, err = discordInt(o.GetOr("flags", int64(0)), 0)
		}
	}
	if err != nil {
		envLog(env).Info(fmt.Sprintf("Discord capability detection failed (%s); exposing all actions.", err))
		return caps
	}
	caps.Set("has_members_intent", flags&DiscordFlagsGuildMembers != 0)
	caps.Set("has_message_content", flags&DiscordFlagsMessageContent != 0)
	caps.Set("detected", true)
	return caps
}

// Blocking detection cached per token for the warm up path, force fetches again
func discordDetectCapabilities(env *Env, token string, force bool) *pyjson.Object {
	discordCapsMu.Lock()
	cached := discordCapsCache[token]
	discordCapsMu.Unlock()
	if cached != nil && !force {
		return cached
	}
	caps := discordFetchCapabilities(env, token)
	discordCapsMu.Lock()
	discordCapsCache[token] = caps
	discordCapsMu.Unlock()
	return caps
}

// Clears the detection cache and the record of started background detections
func discordResetCapabilityCache() {
	discordCapsMu.Lock()
	defer discordCapsMu.Unlock()
	discordCapsCache = map[string]*pyjson.Object{}
	discordCapsBgStarted = map[string]bool{}
}

// Python int() of a decoded value with a falsy value counting as the fallback
func discordInt(v any, fallback int64) (int64, error) {
	switch t := v.(type) {
	case nil:
		return fallback, nil
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case int64:
		return t, nil
	case int:
		return int64(t), nil
	case *big.Int:
		if t.IsInt64() {
			return t.Int64(), nil
		}
		return 1 << 62, nil
	case float64:
		return int64(t), nil
	case string:
		if t == "" {
			return fallback, nil
		}
		n, err := strconv.ParseInt(strings.ReplaceAll(pystr.Strip(t), "_", ""), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.Repr(t))
		}
		return n, nil
	}
	return 0, fmt.Errorf("int() argument must be a string, a bytes-like object or a real number, not '%s'", pyTypeName(v))
}

// KeyError a missing required key of a Discord answer raises
type discordKeyError struct{ key string }

func (e *discordKeyError) Error() string { return pyrepr.Repr(e.key) }

// Dict view of a decoded value, panicking with the AttributeError a get on a non dict raises
func discordDict(v any) *pyjson.Object {
	o, ok := v.(*pyjson.Object)
	if !ok {
		panic(fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(v)))
	}
	return o
}

// Required key of a Discord answer, panicking with the KeyError of a missing key
func discordReq(o *pyjson.Object, key string) any {
	v, ok := o.Get(key)
	if !ok {
		panic(&discordKeyError{key})
	}
	return v
}

// Items of a decoded list, panicking with the TypeError of iterating a non list
func discordList(v any) []any {
	items, ok := v.([]any)
	if !ok {
		panic(fmt.Errorf("'%s' object is not iterable", pyTypeName(v)))
	}
	return items
}

// Numeric sort key of a decoded value, ints and floats compared by value
func discordSortKey(v any) float64 {
	f, _ := portalFloat(v)
	return f
}

// JSON text of a listing with its item count
func discordListing(key string, items []any) string {
	return dumpsDefault(pyjson.ObjectOf(key, items, "count", len(items)))
}

// json.dumps with the default separators and ASCII escaping
func dumpsDefault(v any) string {
	text, err := pyjson.Dumps(v)
	if err != nil {
		panic(err)
	}
	return text
}

// Member row, the full shape adding the avatar and join fields member_info exposes
func discordMemberSummary(m *pyjson.Object, full bool) *pyjson.Object {
	user := discordDict(m.GetOr("user", pyjson.NewObject()))
	row := pyjson.ObjectOf("user_id", user.GetOr("id", nil), "username", user.GetOr("username", nil), "display_name", user.GetOr("global_name", nil),
		"nickname", m.GetOr("nick", nil), "avatar", user.GetOr("avatar", nil), "bot", user.GetOr("bot", false),
		"roles", m.GetOr("roles", []any{}), "joined_at", m.GetOr("joined_at", nil), "premium_since", m.GetOr("premium_since", nil))
	if full {
		return row
	}
	out := pyjson.NewObject()
	for _, k := range row.Keys {
		if k != "avatar" && k != "joined_at" && k != "premium_since" {
			out.Set(k, row.Vals[k])
		}
	}
	return out
}

// Message row with its author, attachments, reactions and pin state
func discordMessageSummary(msg *pyjson.Object) *pyjson.Object {
	author := discordDict(msg.GetOr("author", pyjson.NewObject()))
	attachments := []any{}
	for _, raw := range discordList(msg.GetOr("attachments", []any{})) {
		a := discordDict(raw)
		attachments = append(attachments, pyjson.ObjectOf("filename", a.GetOr("filename", nil), "url", a.GetOr("url", nil), "size", a.GetOr("size", nil)))
	}
	reactions := []any{}
	if connTruthy(msg.GetOr("reactions", nil)) {
		for _, raw := range discordList(msg.GetOr("reactions", []any{})) {
			r := discordDict(raw)
			emoji := discordDict(r.GetOr("emoji", pyjson.NewObject()))
			reactions = append(reactions, pyjson.ObjectOf("emoji", emoji.GetOr("name", nil), "count", r.GetOr("count", int64(0))))
		}
	}
	return pyjson.ObjectOf("id", discordReq(msg, "id"), "content", msg.GetOr("content", ""),
		"author", pyjson.ObjectOf("id", author.GetOr("id", nil), "username", author.GetOr("username", nil),
			"display_name", author.GetOr("global_name", nil), "bot", author.GetOr("bot", false)),
		"timestamp", msg.GetOr("timestamp", nil), "edited_timestamp", msg.GetOr("edited_timestamp", nil),
		"attachments", attachments, "reactions", reactions, "pinned", msg.GetOr("pinned", false))
}

// Page size text capped at Discord's 100 per page, the default when the limit is no integer
func discordLimitParam(limit any, def int64) string {
	n, err := discordInt(limit, def)
	if limit == nil || err != nil {
		n = def
	}
	return strconv.FormatInt(min(n, 100), 10)
}

// Action arguments with every handler default filled in
type discordArgs map[string]any

func (a discordArgs) str(key string) string { return pyrepr.Str(a[key]) }

// Servers the bot is in
func discordListGuilds(ctx context.Context, token string, _ discordArgs) (string, error) {
	guilds, err := DiscordRequest(ctx, "GET", "/users/@me/guilds", token, nil, nil, 15*time.Second)
	if err != nil {
		return "", err
	}
	items := []any{}
	for _, raw := range discordList(guilds) {
		g := discordDict(raw)
		items = append(items, pyjson.ObjectOf("id", discordReq(g, "id"), "name", discordReq(g, "name"), "icon", g.GetOr("icon", nil),
			"owner", g.GetOr("owner", false), "permissions", g.GetOr("permissions", nil)))
	}
	return discordListing("guilds", items), nil
}

// Server details with member and presence counts
func discordServerInfo(ctx context.Context, token string, a discordArgs) (string, error) {
	raw, err := DiscordRequest(ctx, "GET", "/guilds/"+a.str("guild_id"), token, [][2]string{{"with_counts", "true"}}, nil, 15*time.Second)
	if err != nil {
		return "", err
	}
	g := discordDict(raw)
	return dumpsDefault(pyjson.ObjectOf("id", discordReq(g, "id"), "name", discordReq(g, "name"), "description", g.GetOr("description", nil),
		"icon", g.GetOr("icon", nil), "owner_id", g.GetOr("owner_id", nil), "member_count", g.GetOr("approximate_member_count", nil),
		"online_count", g.GetOr("approximate_presence_count", nil), "features", g.GetOr("features", []any{}),
		"premium_tier", g.GetOr("premium_tier", nil), "premium_subscription_count", g.GetOr("premium_subscription_count", nil),
		"verification_level", g.GetOr("verification_level", nil))), nil
}

// Every channel grouped by category with uncategorized first, each group sorted by position
func discordListChannels(ctx context.Context, token string, a discordArgs) (string, error) {
	raw, err := DiscordRequest(ctx, "GET", "/guilds/"+a.str("guild_id")+"/channels", token, nil, nil, 15*time.Second)
	if err != nil {
		return "", err
	}
	channels := discordList(raw)
	var cats []*pyjson.Object
	for _, c := range channels {
		ch := discordDict(c)
		if pyjson.Equal(discordReq(ch, "type"), int64(4)) {
			cats = append(cats, ch)
		}
	}
	sort.SliceStable(cats, func(i, j int) bool {
		return discordSortKey(cats[i].GetOr("position", int64(0))) < discordSortKey(cats[j].GetOr("position", int64(0)))
	})
	groups := map[string][]*pyjson.Object{"": {}}
	isCategory := map[string]bool{}
	for _, c := range cats {
		id := pyrepr.Str(discordReq(c, "id"))
		groups[id] = []*pyjson.Object{}
		isCategory[id] = true
	}
	for _, c := range channels {
		ch := discordDict(c)
		typ := discordReq(ch, "type")
		if pyjson.Equal(typ, int64(4)) {
			continue
		}
		key := ""
		if parent, ok := ch.GetOr("parent_id", nil).(string); ok && isCategory[parent] {
			key = parent
		}
		groups[key] = append(groups[key], pyjson.ObjectOf("id", discordReq(ch, "id"), "name", ch.GetOr("name", ""), "type", discordChannelTypeName(typ),
			"position", ch.GetOr("position", int64(0)), "topic", ch.GetOr("topic", nil), "nsfw", ch.GetOr("nsfw", false)))
	}
	for _, group := range groups {
		sort.SliceStable(group, func(i, j int) bool {
			return discordSortKey(group[i].Vals["position"]) < discordSortKey(group[j].Vals["position"])
		})
	}
	asList := func(rows []*pyjson.Object) []any {
		out := make([]any, len(rows))
		for i, r := range rows {
			out[i] = r
		}
		return out
	}
	result := []any{}
	total := len(groups[""])
	if len(groups[""]) > 0 {
		result = append(result, pyjson.ObjectOf("category", nil, "channels", asList(groups[""])))
	}
	for _, c := range cats {
		id := pyrepr.Str(c.Vals["id"])
		total += len(groups[id])
		result = append(result, pyjson.ObjectOf("category", pyjson.ObjectOf("id", c.Vals["id"], "name", discordReq(c, "name")), "channels", asList(groups[id])))
	}
	return dumpsDefault(pyjson.ObjectOf("channel_groups", result, "total_channels", total)), nil
}

// Single channel details
func discordChannelInfo(ctx context.Context, token string, a discordArgs) (string, error) {
	raw, err := DiscordRequest(ctx, "GET", "/channels/"+a.str("channel_id"), token, nil, nil, 15*time.Second)
	if err != nil {
		return "", err
	}
	ch := discordDict(raw)
	return dumpsDefault(pyjson.ObjectOf("id", discordReq(ch, "id"), "name", ch.GetOr("name", nil), "type", discordChannelTypeName(discordReq(ch, "type")),
		"guild_id", ch.GetOr("guild_id", nil), "topic", ch.GetOr("topic", nil), "nsfw", ch.GetOr("nsfw", false),
		"position", ch.GetOr("position", nil), "parent_id", ch.GetOr("parent_id", nil),
		"rate_limit_per_user", ch.GetOr("rate_limit_per_user", int64(0)), "last_message_id", ch.GetOr("last_message_id", nil))), nil
}

// Roles sorted by position from the top, each color as a hex string
func discordListRoles(ctx context.Context, token string, a discordArgs) (string, error) {
	raw, err := DiscordRequest(ctx, "GET", "/guilds/"+a.str("guild_id")+"/roles", token, nil, nil, 15*time.Second)
	if err != nil {
		return "", err
	}
	var roles []*pyjson.Object
	for _, r := range discordList(raw) {
		roles = append(roles, discordDict(r))
	}
	sort.SliceStable(roles, func(i, j int) bool {
		return discordSortKey(roles[i].GetOr("position", int64(0))) > discordSortKey(roles[j].GetOr("position", int64(0)))
	})
	items := []any{}
	for _, r := range roles {
		var color any
		if c := r.GetOr("color", nil); connTruthy(c) {
			n, err := discordInt(c, 0)
			if err != nil {
				panic(err)
			}
			color = fmt.Sprintf("#%06x", n)
		}
		items = append(items, pyjson.ObjectOf("id", discordReq(r, "id"), "name", discordReq(r, "name"), "color", color,
			"position", r.GetOr("position", int64(0)), "mentionable", r.GetOr("mentionable", false), "managed", r.GetOr("managed", false),
			"member_count", r.GetOr("member_count", nil), "hoist", r.GetOr("hoist", false)))
	}
	return discordListing("roles", items), nil
}

// One guild member in the full shape
func discordMemberInfo(ctx context.Context, token string, a discordArgs) (string, error) {
	raw, err := DiscordRequest(ctx, "GET", "/guilds/"+a.str("guild_id")+"/members/"+a.str("user_id"), token, nil, nil, 15*time.Second)
	if err != nil {
		return "", err
	}
	return dumpsDefault(discordMemberSummary(discordDict(raw), true)), nil
}

// Name prefix member search, which needs the GUILD_MEMBERS intent
func discordSearchMembers(ctx context.Context, token string, a discordArgs) (string, error) {
	params := [][2]string{{"query", a.str("query")}, {"limit", discordLimitParam(a["limit"], 20)}}
	raw, err := DiscordRequest(ctx, "GET", "/guilds/"+a.str("guild_id")+"/members/search", token, params, nil, 15*time.Second)
	if err != nil {
		return "", err
	}
	items := []any{}
	for _, m := range discordList(raw) {
		items = append(items, discordMemberSummary(discordDict(m), false))
	}
	return discordListing("members", items), nil
}

// Recent channel messages, before and after paging by message snowflake
func discordFetchMessages(ctx context.Context, token string, a discordArgs) (string, error) {
	params := [][2]string{{"limit", discordLimitParam(a["limit"], 50)}}
	if connTruthy(a["before"]) {
		params = append(params, [2]string{"before", a.str("before")})
	}
	if connTruthy(a["after"]) {
		params = append(params, [2]string{"after", a.str("after")})
	}
	raw, err := DiscordRequest(ctx, "GET", "/channels/"+a.str("channel_id")+"/messages", token, params, nil, 15*time.Second)
	if err != nil {
		return "", err
	}
	items := []any{}
	for _, m := range discordList(raw) {
		items = append(items, discordMessageSummary(discordDict(m)))
	}
	return discordListing("messages", items), nil
}

// Pinned messages with their content cut to 200 characters
func discordListPins(ctx context.Context, token string, a discordArgs) (string, error) {
	raw, err := DiscordRequest(ctx, "GET", "/channels/"+a.str("channel_id")+"/pins", token, nil, nil, 15*time.Second)
	if err != nil {
		return "", err
	}
	items := []any{}
	for _, m := range discordList(raw) {
		msg := discordDict(m)
		content, ok := msg.GetOr("content", "").(string)
		if !ok {
			panic(fmt.Errorf("'%s' object is not subscriptable", pyTypeName(msg.Vals["content"])))
		}
		if r := []rune(content); len(r) > 200 {
			content = string(r[:200])
		}
		author := discordDict(msg.GetOr("author", pyjson.NewObject()))
		items = append(items, pyjson.ObjectOf("id", discordReq(msg, "id"), "content", content, "author", author.GetOr("username", nil),
			"timestamp", msg.GetOr("timestamp", nil)))
	}
	return discordListing("pinned_messages", items), nil
}

// Thread anchored to the message when one is named, else a standalone public thread
func discordCreateThread(ctx context.Context, token string, a discordArgs) (string, error) {
	body := pyjson.ObjectOf("name", a["name"], "auto_archive_duration", a["auto_archive_duration"])
	path := "/channels/" + a.str("channel_id") + "/threads"
	if connTruthy(a["message_id"]) {
		path = "/channels/" + a.str("channel_id") + "/messages/" + a.str("message_id") + "/threads"
	} else {
		body.Set("type", int64(11))
	}
	raw, err := DiscordRequest(ctx, "POST", path, token, nil, body, 15*time.Second)
	if err != nil {
		return "", err
	}
	thread := discordDict(raw)
	return dumpsDefault(pyjson.ObjectOf("success", true, "thread_id", discordReq(thread, "id"), "name", thread.GetOr("name", nil))), nil
}

// Body less write action whose path and message are templates over the action arguments
func discordMutation(method, path, message string) discordAction {
	format := func(template string, a discordArgs) string {
		for k := range a {
			template = strings.ReplaceAll(template, "{"+k+"}", a.str(k))
		}
		return template
	}
	return func(ctx context.Context, token string, a discordArgs) (string, error) {
		if _, err := DiscordRequest(ctx, method, format(path, a), token, nil, nil, 15*time.Second); err != nil {
			return "", err
		}
		return dumpsDefault(pyjson.ObjectOf("success", true, "message", format(message, a))), nil
	}
}

type discordAction func(ctx context.Context, token string, a discordArgs) (string, error)

type discordManifestRow struct {
	name string
	fn   discordAction
	sig  string
	desc string
}

// Every action in schema order with its handler, required parameter signature and description
var discordActionManifest = []discordManifestRow{
	{"list_guilds", discordListGuilds, "()", "list servers the bot is in"},
	{"server_info", discordServerInfo, "(guild_id)", "server details + member counts"},
	{"list_channels", discordListChannels, "(guild_id)", "all channels grouped by category"},
	{"channel_info", discordChannelInfo, "(channel_id)", "single channel details"},
	{"list_roles", discordListRoles, "(guild_id)", "roles sorted by position"},
	{"member_info", discordMemberInfo, "(guild_id, user_id)", "lookup a specific member"},
	{"search_members", discordSearchMembers, "(guild_id, query)", "find members by name prefix"},
	{"fetch_messages", discordFetchMessages, "(channel_id)", "recent messages; optional before/after snowflakes"},
	{"list_pins", discordListPins, "(channel_id)", "pinned messages in a channel"},
	{"pin_message", discordMutation("PUT", "/channels/{channel_id}/pins/{message_id}", "Message {message_id} pinned."), "(channel_id, message_id)", "pin a message"},
	{"unpin_message", discordMutation("DELETE", "/channels/{channel_id}/pins/{message_id}", "Message {message_id} unpinned."), "(channel_id, message_id)", "unpin a message"},
	{"delete_message", discordMutation("DELETE", "/channels/{channel_id}/messages/{message_id}", "Message {message_id} deleted."), "(channel_id, message_id)", "delete a message"},
	{"create_thread", discordCreateThread, "(channel_id, name)", "create a public thread; optional message_id anchor"},
	{"add_role", discordMutation("PUT", "/guilds/{guild_id}/members/{user_id}/roles/{role_id}", "Role {role_id} added to user {user_id}."), "(guild_id, user_id, role_id)", "assign a role"},
	{"remove_role", discordMutation("DELETE", "/guilds/{guild_id}/members/{user_id}/roles/{role_id}", "Role {role_id} removed from user {user_id}."), "(guild_id, user_id, role_id)", "remove a role"},
}

var discordCoreActionNames = map[string]bool{"fetch_messages": true, "search_members": true, "create_thread": true}

var discordIntentGatedMembers = map[string]bool{"member_info": true, "search_members": true}

// Action names of the core trio or of everything else, in manifest order
func discordActionNames(core bool) []string {
	var out []string
	for _, row := range discordActionManifest {
		if discordCoreActionNames[row.name] == core {
			out = append(out, row.name)
		}
	}
	return out
}

// Manifest row of a known action
func discordManifestOf(name string) (discordManifestRow, bool) {
	for _, row := range discordActionManifest {
		if row.name == name {
			return row, true
		}
	}
	return discordManifestRow{}, false
}

// Required parameters of an action from its manifest signature
func discordRequiredParams(name string) []string {
	row, _ := discordManifestOf(name)
	var out []string
	for _, p := range strings.Split(strings.Trim(row.sig, "()"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// The discord.server_actions allowlist, nil when unrestricted, unknown names dropped with a warning
func discordLoadAllowedActionsConfig(env *Env) ([]string, bool) {
	var raw any
	if section := cfgTree(env).Get("discord"); connTruthy(section) {
		raw = discordDict(pyjson.Ordered(section)).GetOr("server_actions", nil)
	}
	if raw == nil || raw == "" {
		return nil, false
	}
	var items []any
	switch t := raw.(type) {
	case string:
		for _, part := range strings.Split(t, ",") {
			items = append(items, part)
		}
	case []any:
		items = t
	default:
		envLog(env).Warn(fmt.Sprintf("discord.server_actions: unexpected type %s; ignoring.", pyTypeName(raw)))
		return nil, false
	}
	var names, invalid, known []string
	for _, n := range items {
		if s := pystr.Strip(pyrepr.Str(n)); s != "" {
			names = append(names, s)
		}
	}
	for _, n := range names {
		if _, ok := discordManifestOf(n); ok {
			known = append(known, n)
		} else {
			invalid = append(invalid, n)
		}
	}
	if len(invalid) > 0 {
		all := make([]string, len(discordActionManifest))
		for i, row := range discordActionManifest {
			all[i] = row.name
		}
		envLog(env).Warn(fmt.Sprintf("discord.server_actions: unknown action(s) ignored: %s. Known: %s", strings.Join(invalid, ", "), strings.Join(all, ", ")))
	}
	if known == nil {
		known = []string{}
	}
	return known, true
}

// Visible actions from the intents and the config allowlist, in manifest order
func discordAvailableActions(caps *pyjson.Object, allowlist []string, restricted bool) []string {
	membersOK := connTruthy(caps.GetOr("has_members_intent", true))
	var out []string
	for _, row := range discordActionManifest {
		if (membersOK || !discordIntentGatedMembers[row.name]) && (!restricted || inList(allowlist, row.name)) {
			out = append(out, row.name)
		}
	}
	return out
}

var discordToolDescriptions = map[string][2]string{
	"discord_admin": {"Manage a Discord server via the REST API.",
		"Call list_guilds first to discover guild_ids, then list_channels for channel_ids. Runtime errors will tell you if the bot lacks a specific per-guild permission (e.g. MANAGE_ROLES for add_role)."},
	"discord": {"Read and participate in a Discord server.",
		"Use the channel_id from the current conversation context. Use search_members to look up user IDs by name prefix."},
}

// Parameter schemas every discord action shares
func discordSchemaProperties() map[string]any {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	return map[string]any{
		"guild_id": str("Discord server (guild) ID."), "channel_id": str("Discord channel ID."), "user_id": str("Discord user ID."),
		"role_id": str("Discord role ID."), "message_id": str("Discord message ID."),
		"query": str("Member name prefix to search for (search_members)."), "name": str("New thread name (create_thread)."),
		"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "description": "Max results (default 50). Applies to fetch_messages, search_members."},
		"before": str("Snowflake ID for reverse pagination (fetch_messages)."), "after": str("Snowflake ID for forward pagination (fetch_messages)."),
		"auto_archive_duration": map[string]any{"type": "integer", "enum": []any{60, 1440, 4320, 10080}, "description": "Thread archive duration in minutes (create_thread, default 1440)."},
	}
}

const discordContentNote = "\n\nNOTE: Bot does NOT have the MESSAGE_CONTENT privileged intent. %s will return message metadata (author, " +
	"timestamps, attachments, reactions, pin state) but `content` will be empty for messages not sent as a direct mention to the bot or in DMs. " +
	"Enable the intent in the Discord Developer Portal to see all content."

// Tool schema of the filtered action list, nil when no action remains so the tool is dropped
func discordBuildSchema(actions []string, caps *pyjson.Object, toolName string) map[string]any {
	if len(actions) == 0 {
		return nil
	}
	if caps == nil {
		caps = pyjson.NewObject()
	}
	var lines, affected []string
	for _, row := range discordActionManifest {
		if inList(actions, row.name) {
			lines = append(lines, "  "+row.name+row.sig+"  — "+row.desc)
		}
	}
	for _, name := range []string{"fetch_messages", "list_pins"} {
		if inList(actions, name) {
			affected = append(affected, name)
		}
	}
	contentNote := ""
	if hasContent, set := caps.Get("has_message_content"); len(affected) > 0 && connTruthy(caps.GetOr("detected", nil)) && set && hasContent == false {
		sort.Strings(affected)
		contentNote = fmt.Sprintf(discordContentNote, strings.Join(affected, " and "))
	}
	desc, ok := discordToolDescriptions[toolName]
	if !ok {
		desc = discordToolDescriptions["discord"]
	}
	properties := discordSchemaProperties()
	properties["action"] = map[string]any{"type": "string", "enum": enumValues(actions)}
	return map[string]any{"name": toolName,
		"description": desc[0] + "\n\nAvailable actions:\n" + strings.Join(lines, "\n") + "\n\n" + desc[1] + contentNote,
		"parameters":  map[string]any{"type": "object", "properties": properties, "required": []any{"action"}}}
}

// Schema of the action subset filtered by the bot's intents and the config allowlist, nil when none remain
func discordGetDynamicSchema(env *Env, core bool, toolName string) map[string]any {
	token := discordBotToken(env)
	if token == "" {
		return nil
	}
	caps := discordDetectCapabilitiesNonblocking(env, token)
	allowlist, restricted := discordLoadAllowedActionsConfig(env)
	subset := discordActionNames(core)
	var actions []string
	for _, name := range discordAvailableActions(caps, allowlist, restricted) {
		if inList(subset, name) {
			actions = append(actions, name)
		}
	}
	return discordBuildSchema(actions, caps, toolName)
}

// Schema rewriter replacing a discord tool definition with its intent and config filtered schema, nil dropping it
func discordRewriter(core bool, toolName string) SchemaRewriter {
	return func(env *Env, _ map[string]any, _ map[string]bool) (out map[string]any) {
		defer func() {
			if recover() != nil {
				out = nil
			}
		}()
		dynamic := discordGetDynamicSchema(env, core, toolName)
		if dynamic == nil {
			return nil
		}
		return fnDef(dynamic)
	}
}

const (
	discordNoManageMessages = "Bot lacks MANAGE_MESSAGES permission in this channel"
	discordViewHistory      = "Bot cannot view this channel (missing VIEW_CHANNEL or READ_MESSAGE_HISTORY)."
	discordRoleHierarchy    = "Either the bot lacks MANAGE_ROLES, or the target role sits higher than the bot's highest role."
)

var discordAction403Hint = map[string]string{
	"pin_message":    discordNoManageMessages + ". Ask the server admin to grant the bot a role that has MANAGE_MESSAGES, or a per-channel overwrite.",
	"unpin_message":  discordNoManageMessages + ".",
	"delete_message": discordNoManageMessages + ", or cannot view the channel/message.",
	"create_thread":  "Bot lacks CREATE_PUBLIC_THREADS in this channel, or cannot view it.",
	"add_role":       discordRoleHierarchy + " Roles can only be assigned below the bot's own position in the role hierarchy.",
	"remove_role":    discordRoleHierarchy,
	"fetch_messages": discordViewHistory,
	"list_pins":      discordViewHistory,
	"channel_info":   "Bot cannot view this channel (missing VIEW_CHANNEL).",
	"search_members": "Likely missing the Server Members privileged intent — enable it in the Discord Developer Portal under your bot's settings.",
	"member_info":    "Bot cannot see this guild member (missing Server Members intent or insufficient permissions).",
}

// Guidance for a call time 403 on the action with the raw Discord body appended
func discordEnrich403(action, body string) string {
	base := "Discord API 403 (forbidden) on '" + action + "'."
	if hint, ok := discordAction403Hint[action]; ok {
		return base + " " + hint + " (Raw: " + body + ")"
	}
	return base + " (Raw: " + body + ")"
}

// Whether a Discord bot token is configured
func checkDiscordToolRequirements(env *Env) bool { return discordBotToken(env) != "" }

var discordHandlerDefaults = []struct {
	key string
	def any
}{{"guild_id", ""}, {"channel_id", ""}, {"user_id", ""}, {"role_id", ""}, {"message_id", ""}, {"query", ""},
	{"name", ""}, {"limit", int64(50)}, {"before", ""}, {"after", ""}, {"auto_archive_duration", int64(1440)}}

// Runs one action of the tool's action set with every gate, default and error mapping of the shared handler
func discordRunAction(ctx context.Context, env *Env, action string, core bool, toolLabel string, params map[string]any) (result string) {
	token := discordBotToken(env)
	if token == "" {
		return toolErrorJSON("DISCORD_BOT_TOKEN not configured.", nil)
	}
	valid := discordActionNames(core)
	row, known := discordManifestOf(action)
	if !known || !inList(valid, action) {
		return toolErrorJSON("Unknown action: "+action, pyjson.ObjectOf("available_actions", enumValues(valid)))
	}
	if allowlist, restricted := discordLoadAllowedActionsConfig(env); restricted && !inList(allowlist, action) {
		allowed := "<none>"
		if len(allowlist) > 0 {
			allowed = strings.Join(allowlist, ", ")
		}
		return toolErrorJSON(fmt.Sprintf("Action '%s' is disabled by config (discord.server_actions). Allowed: %s", action, allowed), nil)
	}
	args := discordArgs{}
	for _, d := range discordHandlerDefaults {
		if v, ok := params[d.key]; ok {
			args[d.key] = v
		} else {
			args[d.key] = d.def
		}
	}
	var missing []string
	for _, p := range discordRequiredParams(action) {
		if !connTruthy(args[p]) {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		return toolErrorJSON(fmt.Sprintf("Missing required parameters for '%s': %s", action, strings.Join(missing, ", ")), nil)
	}
	defer func() {
		if rec := recover(); rec != nil {
			envLog(env).Error(fmt.Sprintf("Unexpected error in %s action '%s'", toolLabel, action), "error", rec)
			result = toolErrorJSON(fmt.Sprintf("Unexpected error: %v", rec), nil)
		}
	}()
	out, err := row.fn(ctx, token, args)
	if err == nil {
		return out
	}
	if apiErr, ok := err.(*DiscordAPIError); ok {
		envLog(env).Warn(fmt.Sprintf("Discord API error in %s action '%s': %s", toolLabel, action, apiErr))
		if apiErr.Status == 403 {
			return toolErrorJSON(discordEnrich403(action, apiErr.Body), nil)
		}
		return toolErrorJSON(apiErr.Error(), nil)
	}
	envLog(env).Error(fmt.Sprintf("Unexpected error in %s action '%s'", toolLabel, action), "error", err)
	return toolErrorJSON(fmt.Sprintf("Unexpected error: %s", err), nil)
}

// Registers discord, the participation trio, and discord_admin, server management, with their filtered schemas
func registerDiscordTools() {
	for _, tool := range []struct {
		name string
		core bool
	}{{"discord", true}, {"discord_admin", false}} {
		tool := tool
		Register(Def{Name: tool.name, Toolset: tool.name, RequiresEnv: []string{"DISCORD_BOT_TOKEN"}, Check: checkDiscordToolRequirements,
			Schema: discordBuildSchema(discordActionNames(tool.core), pyjson.ObjectOf("detected", false), tool.name),
			Handler: func(ctx context.Context, call *Call) string {
				action := ""
				if v, ok := call.Args["action"]; ok {
					action = pyrepr.Str(v)
				}
				return discordRunAction(ctx, call.Env, action, tool.core, tool.name, call.Args)
			}})
		RegisterSchemaRewriter(tool.name, discordRewriter(tool.core, tool.name))
	}
}
