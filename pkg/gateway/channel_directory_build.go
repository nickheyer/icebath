package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const slackDirectoryWarningInterval = 3600 * time.Second

var (
	skipSessionDiscovery = map[string]bool{"local": true, "api_server": true, "webhook": true}
	slackRawIDPrefixes   = []string{"C0", "D0", "G0"}

	slackDirectoryWarningMu   sync.Mutex
	slackDirectoryWarningLast = map[[2]string]time.Time{}

	writeChannelDirectoryFn = config.AtomicJSONWrite
)

// Adapter enumerating its own reachable channels, nil channels deferring to the platform builders
type ChannelListingAdapter interface {
	ListChannels(ctx context.Context) (any, error)
}

// Discord adapter exposing its gateway client, nil before login
type DiscordDirectoryAdapter interface {
	DiscordDirectoryClient() DiscordDirectoryClient
}

// Discord gateway client listing the guilds the bot belongs to
type DiscordDirectoryClient interface {
	Guilds() []DiscordDirectoryGuild
}

// Discord guild with its name, text channels and forum channels
type DiscordDirectoryGuild interface {
	GuildName() string
	TextChannels() []DiscordDirectoryChannel
	ForumChannels() []DiscordDirectoryChannel
}

// Discord guild channel with its snowflake id and display name
type DiscordDirectoryChannel interface {
	DiscordChannelID() uint64
	DiscordChannelNameReader
}

// Slack adapter exposing one Web API client per connected workspace
type SlackDirectoryAdapter interface {
	SlackTeamClients() *pydict.Dict[string, SlackDirectoryClient]
}

// Slack Web API calls the directory makes, each returning the decoded response body
type SlackDirectoryClient interface {
	UsersConversations(ctx context.Context, types string, excludeArchived bool, limit int, cursor string) (map[string]any, error)
	ConversationsInfo(ctx context.Context, channel string) (map[string]any, error)
	UsersInfo(ctx context.Context, user string) (map[string]any, error)
}

// Slack Web API failure carrying the decoded response body
type SlackAPIError struct {
	Message  string
	Response map[string]any
}

func (e *SlackAPIError) Error() string { return e.Message }

// Logs a Slack listing failure, missing_scope at debug and others as a warning once per hour per detail
func reportSlackFailure(teamID string, errorCode any, detail string) {
	if errorCode == any("missing_scope") {
		slog.Debug(fmt.Sprintf("Channel directory: Slack team %s lacks channels:read; using session history only", teamID))
		return
	}
	key := [2]string{teamID, detail}
	now := time.Now()
	slackDirectoryWarningMu.Lock()
	last, seen := slackDirectoryWarningLast[key]
	due := !seen || now.Sub(last) >= slackDirectoryWarningInterval
	if due {
		slackDirectoryWarningLast[key] = now
	}
	slackDirectoryWarningMu.Unlock()
	if due {
		slog.Warn(fmt.Sprintf("Channel directory: failed to list Slack channels for team %s: %s", teamID, detail))
		return
	}
	slog.Debug(fmt.Sprintf("Channel directory: suppressed repeated Slack channel list failure for team %s: %s", teamID, detail))
}

// Builds the directory from connected adapters and session history, persists it and returns it
func BuildChannelDirectory(ctx context.Context, adapters *pydict.Dict[Platform, PlatformAdapter]) (*pyjson.Object, error) {
	platforms := pyjson.NewObject()
	connected := map[string]bool{}
	for platform, adapter := range adapters.All() {
		connected[string(platform)] = true
		if err := buildPlatformChannels(ctx, platforms, platform, adapter); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			slog.Warn(fmt.Sprintf("Channel directory: failed to build %s: %s", platform, err))
		}
	}
	discover := func(name string) error {
		if _, built := platforms.Get(name); skipSessionDiscovery[name] || built || !connected[name] {
			return nil
		}
		entries, err := runOffLoop(ctx, func() ([]any, error) { return directoryList(BuildFromSessions(ctx, name)), nil })
		if err != nil {
			return err
		}
		platforms.Set(name, entries)
		return nil
	}
	for _, plat := range PlatformMembers() {
		if err := discover(string(plat)); err != nil {
			return nil, err
		}
	}
	for _, entry := range Registry.LoadedEntries(ctx) {
		if err := discover(entry.Name); err != nil {
			return nil, err
		}
	}
	applyChannelAliases(ctx, platforms)
	directory := pyjson.ObjectOf("updated_at", pydatetime.ISOFormatNaive(time.Now()), "platforms", platforms)
	path := channelDirectoryFile(ctx)
	if _, err := runOffLoop(ctx, func() (struct{}, error) { return struct{}{}, writeChannelDirectoryFn(ctx, path, directory) }); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		slog.Warn(fmt.Sprintf("Channel directory: failed to write: %s", err))
	}
	return directory, nil
}

// Fills one platform from its list_channels hook or its Discord or Slack builder
func buildPlatformChannels(ctx context.Context, platforms *pyjson.Object, platform Platform, adapter PlatformAdapter) error {
	if lister, ok := adapter.(ChannelListingAdapter); ok {
		raw, err := lister.ListChannels(ctx)
		if err != nil {
			return err
		}
		if raw != nil {
			platforms.Set(string(platform), normalizeAdapterChannels(raw))
			return nil
		}
	}
	switch platform {
	case PlatformDiscord:
		channels, err := runOffLoop(ctx, func() ([]any, error) { return buildDiscord(ctx, adapter) })
		if err != nil {
			return err
		}
		platforms.Set("discord", channels)
	case PlatformSlack:
		channels, err := buildSlack(ctx, adapter)
		if err != nil {
			return err
		}
		platforms.Set("slack", channels)
	}
	return nil
}

// Session entries as a JSON list, empty rather than null when none were found
func directoryList(entries []*pyjson.Object) []any {
	out := make([]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, e)
	}
	return out
}

// Text and forum channels the Discord bot can see that are not obfuscated, then session DMs
func buildDiscord(ctx context.Context, adapter any) ([]any, error) {
	channels := []any{}
	da, ok := adapter.(DiscordDirectoryAdapter)
	if !ok {
		return channels, nil
	}
	client := da.DiscordDirectoryClient()
	if client == nil || reflect.ValueOf(client).Kind() == reflect.Pointer && reflect.ValueOf(client).IsNil() {
		return channels, nil
	}
	for _, guild := range client.Guilds() {
		for _, group := range []struct {
			chs  []DiscordDirectoryChannel
			kind string
		}{{guild.TextChannels(), "channel"}, {guild.ForumChannels(), "forum"}} {
			for _, ch := range group.chs {
				if IsDiscordChannelObfuscated(ch) {
					continue
				}
				name, has := ch.DiscordChannelName()
				if !has {
					return nil, fmt.Errorf("AttributeError: channel object has no attribute 'name'")
				}
				channels = append(channels, pyjson.ObjectOf("id", fmt.Sprint(ch.DiscordChannelID()), "name", name, "guild", guild.GuildName(), "type", group.kind))
			}
		}
	}
	return append(channels, directoryList(BuildFromSessions(ctx, "discord"))...), nil
}

// Value of key in a decoded Slack mapping, an AttributeError for a value that is not a mapping
func slackGet(v any, key string) (any, bool, error) {
	switch m := v.(type) {
	case map[string]any:
		got, ok := m[key]
		return got, ok, nil
	case *pyjson.Object:
		if m == nil {
			return nil, false, fmt.Errorf("'NoneType' object has no attribute 'get'")
		}
		got, ok := m.Get(key)
		return got, ok, nil
	}
	return nil, false, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(v))
}

// Value of key in a decoded Slack mapping, def when absent
func slackGetOr(v any, key string, def any) (any, error) {
	got, ok, err := slackGet(v, key)
	if err != nil || !ok {
		return def, err
	}
	return got, nil
}

// Slack Web API error code of a SlackApiError, empty when it carries none
func slackAPIErrorCode(err error) any {
	var apiErr *SlackAPIError
	if errors.As(err, &apiErr) {
		if value := apiErr.Response["error"]; pyrepr.Truthy(value) {
			return pyrepr.Str(value)
		}
	}
	return nil
}

// Validated and deduped entries of an adapter list_channels result
func normalizeAdapterChannels(raw any) []any {
	channels := []any{}
	list, ok := raw.([]any)
	if !ok {
		return channels
	}
	seen := map[string]bool{}
	for _, item := range list {
		if _, isDict := item.(map[string]any); !isDict {
			if obj, isObj := item.(*pyjson.Object); !isObj || obj == nil {
				continue
			}
		}
		field := func(key string) any { v, _ := slackGetOr(item, key, nil); return v }
		channelID := pystr.Strip(pyrepr.Str(pyrepr.Or(field("id"), "")))
		name := pystr.Strip(pyrepr.Str(pyrepr.Or(field("name"), channelID)))
		if channelID == "" || name == "" || seen[channelID] {
			continue
		}
		entry := pyjson.ObjectOf("id", channelID, "name", name, "type", pyrepr.Str(pyrepr.Or(field("type"), "dm")))
		for _, key := range []string{"thread_id", "guild"} {
			if v := field(key); pyrepr.Truthy(v) {
				entry.Set(key, pyrepr.Str(v))
			}
		}
		channels = append(channels, entry)
		seen[channelID] = true
	}
	return channels
}

// Conversation id of a thread qualified Slack routing key
func slackBaseID(entryID any) (string, error) {
	s, ok := entryID.(string)
	if !ok {
		return "", fmt.Errorf("'%s' object has no attribute 'split'", pyrepr.TypeName(entryID))
	}
	base, _, _ := strings.Cut(s, ":")
	return base, nil
}

// Whether an entry is still named by a raw Slack conversation id
func slackHasRawName(entry *pyjson.Object) (bool, error) {
	name := entry.GetOr("name", "")
	s, ok := name.(string)
	if !ok {
		return false, fmt.Errorf("'%s' object has no attribute 'startswith'", pyrepr.TypeName(name))
	}
	for _, p := range slackRawIDPrefixes {
		if strings.HasPrefix(s, p) {
			return true, nil
		}
	}
	return false, nil
}

// Set key of a Slack value under Python hashing, numbers and booleans comparing by value
func slackSetKey(v any) (any, error) {
	switch x := v.(type) {
	case bool:
		if x {
			return float64(1), nil
		}
		return float64(0), nil
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case nil:
		return nil, nil
	}
	if !reflect.TypeOf(v).Comparable() {
		return nil, fmt.Errorf("unhashable type: '%s'", pyrepr.TypeName(v))
	}
	return v, nil
}

// Public and private channels the bot is a member of in one workspace, at most twenty pages
func slackTeamChannels(ctx context.Context, teamID string, client SlackDirectoryClient, seen map[any]bool) ([]*pyjson.Object, error) {
	var channels []*pyjson.Object
	err := func() error {
		cursor := ""
		for range 20 {
			response, err := client.UsersConversations(ctx, "public_channel,private_channel", true, 200, cursor)
			if err != nil {
				return err
			}
			if ok, _ := slackGetOr(response, "ok", nil); !pyrepr.Truthy(ok) {
				code, _ := slackGetOr(response, "error", "unknown")
				reportSlackFailure(teamID, code, "users.conversations not ok: "+pyrepr.Str(code))
				return nil
			}
			raw, has := response["channels"]
			if !has {
				raw = []any{}
			}
			list, isList := raw.([]any)
			if !isList {
				return fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(raw))
			}
			for _, ch := range list {
				cid, err := slackGetOr(ch, "id", nil)
				if err != nil {
					return err
				}
				name, _ := slackGetOr(ch, "name", nil)
				if !pyrepr.Truthy(cid) || !pyrepr.Truthy(name) {
					continue
				}
				key, err := slackSetKey(cid)
				if err != nil {
					return err
				}
				if seen[key] {
					continue
				}
				seen[key] = true
				kind := "channel"
				if private, _ := slackGetOr(ch, "is_private", nil); pyrepr.Truthy(private) {
					kind = "private"
				}
				channels = append(channels, pyjson.ObjectOf("id", cid, "name", name, "type", kind))
			}
			meta, _ := slackGetOr(response, "response_metadata", nil)
			if !pyrepr.Truthy(meta) {
				meta = map[string]any{}
			}
			next, err := slackGetOr(meta, "next_cursor", nil)
			if err != nil {
				return err
			}
			if !pyrepr.Truthy(next) {
				return nil
			}
			cursor = pyrepr.Str(next)
		}
		return nil
	}()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		reportSlackFailure(teamID, slackAPIErrorCode(err), err.Error())
	}
	return channels, nil
}

// Names entries still holding raw ids through conversations.info and users.info, once per base conversation
func slackResolveRawNames(ctx context.Context, client SlackDirectoryClient, channels []*pyjson.Object) error {
	var order []string
	unresolved := map[string][]*pyjson.Object{}
	for _, entry := range channels {
		raw, err := slackHasRawName(entry)
		if err != nil {
			return err
		}
		if !raw {
			continue
		}
		id, _ := entry.Get("id")
		base, err := slackBaseID(id)
		if err != nil {
			return err
		}
		if _, ok := unresolved[base]; !ok {
			order = append(order, base)
		}
		unresolved[base] = append(unresolved[base], entry)
	}
	var wg sync.WaitGroup
	for _, base := range order {
		wg.Go(func() {
			if err := slackResolveBase(ctx, client, base, unresolved[base]); err != nil {
				slog.Debug(fmt.Sprintf("Channel directory: failed to resolve %s: %s", base, err))
			}
		})
	}
	wg.Wait()
	return ctx.Err()
}

// Resolves one base conversation and renames its entries, a DM taking the user display name
func slackResolveBase(ctx context.Context, client SlackDirectoryClient, baseID string, entries []*pyjson.Object) error {
	resp, err := client.ConversationsInfo(ctx, baseID)
	if err != nil {
		return err
	}
	if ok, _ := slackGetOr(resp, "ok", nil); !pyrepr.Truthy(ok) {
		return nil
	}
	info, _ := slackGetOr(resp, "channel", map[string]any{})
	var resolvedName, resolvedType any
	isIM, err := slackGetOr(info, "is_im", nil)
	if err != nil {
		return err
	}
	if !pyrepr.Truthy(isIM) {
		name, _ := slackGetOr(info, "name", nil)
		normalized, _ := slackGetOr(info, "name_normalized", nil)
		resolvedName = pyrepr.Or(name, normalized)
	} else if user, _ := slackGetOr(info, "user", ""); pyrepr.Truthy(user) {
		userResp, err := client.UsersInfo(ctx, pyrepr.Str(user))
		if err != nil {
			return err
		}
		if ok, _ := slackGetOr(userResp, "ok", nil); pyrepr.Truthy(ok) {
			u, has := userResp["user"]
			if !has {
				return fmt.Errorf("KeyError: 'user'")
			}
			profile, err := slackGetOr(u, "profile", map[string]any{})
			if err != nil {
				return err
			}
			display, err := slackGetOr(profile, "display_name", nil)
			if err != nil {
				return err
			}
			realName, _ := slackGetOr(u, "real_name", nil)
			plain, _ := slackGetOr(u, "name", nil)
			resolvedName = pyrepr.Or(display, pyrepr.Or(realName, plain))
			resolvedType = "dm"
		}
	}
	if !pyrepr.Truthy(resolvedName) {
		return nil
	}
	for _, entry := range entries {
		entry.Set("name", resolvedName)
		if pyrepr.Truthy(resolvedType) {
			entry.Set("type", resolvedType)
		}
	}
	return nil
}

// Slack channels the bot joined in every workspace merged with session history, raw ids resolved to names
func buildSlack(ctx context.Context, adapter any) ([]any, error) {
	var teams *pydict.Dict[string, SlackDirectoryClient]
	if sa, ok := adapter.(SlackDirectoryAdapter); ok {
		teams = sa.SlackTeamClients()
	}
	if teams == nil || teams.Len() == 0 {
		return runOffLoop(ctx, func() ([]any, error) { return directoryList(BuildFromSessions(ctx, "slack")), nil })
	}
	var channels []*pyjson.Object
	seen := map[any]bool{}
	for teamID, client := range teams.All() {
		got, err := slackTeamChannels(ctx, teamID, client, seen)
		if err != nil {
			return nil, err
		}
		channels = append(channels, got...)
	}
	lookup := map[any]any{}
	for _, ch := range channels {
		id, _ := ch.Get("id")
		key, err := slackSetKey(id)
		if err != nil {
			return nil, err
		}
		lookup[key], _ = ch.Get("name")
	}
	sessions, err := runOffLoop(ctx, func() ([]*pyjson.Object, error) { return BuildFromSessions(ctx, "slack"), nil })
	if err != nil {
		return nil, err
	}
	for _, entry := range sessions {
		eid, isStr := entry.GetOr("id", nil).(string)
		if !isStr || seen[eid] {
			continue
		}
		raw, err := slackHasRawName(entry)
		if err != nil {
			return nil, err
		}
		if raw {
			base, _ := slackBaseID(eid)
			if name, known := lookup[base]; known {
				entry.Set("name", name)
			}
		}
		channels = append(channels, entry)
		seen[eid] = true
	}
	if err := slackResolveRawNames(ctx, teams.Values()[0], channels); err != nil {
		return nil, err
	}
	out := make([]any, 0, len(channels))
	for _, ch := range channels {
		out = append(out, ch)
	}
	return out, nil
}
