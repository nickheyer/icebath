package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

type listingAdapter struct {
	*baseFakeAdapter
	channels any
}

func (a *listingAdapter) ListChannels(context.Context) (any, error) { return a.channels, nil }

type directoryDiscordChannel struct {
	id    uint64
	name  string
	flags int
}

func (c directoryDiscordChannel) DiscordChannelID() uint64         { return c.id }
func (c directoryDiscordChannel) DiscordChannelName() (any, bool)  { return c.name, true }
func (c directoryDiscordChannel) DiscordChannelFlags() (any, bool) { return c.flags, true }
func (g fakeDiscordGuild) GuildName() string                       { return g.name }
func (g fakeDiscordGuild) TextChannels() []DiscordDirectoryChannel { return g.text }
func (g fakeDiscordGuild) ForumChannels() []DiscordDirectoryChannel {
	return g.forums
}

type fakeDiscordGuild struct {
	name   string
	text   []DiscordDirectoryChannel
	forums []DiscordDirectoryChannel
}

type fakeDiscordClient struct {
	guilds  []DiscordDirectoryGuild
	mu      sync.Mutex
	callers []uint64
}

func (c *fakeDiscordClient) Guilds() []DiscordDirectoryGuild {
	c.mu.Lock()
	c.callers = append(c.callers, pythread.GoroutineID())
	c.mu.Unlock()
	return c.guilds
}

type discordAdapter struct {
	*baseFakeAdapter
	client *fakeDiscordClient
}

func (a *discordAdapter) DiscordDirectoryClient() DiscordDirectoryClient { return a.client }

type fakeSlackClient struct {
	mu        sync.Mutex
	pages     []map[string]any
	infos     []map[string]any
	convCalls int
	infoCalls []string
}

func (c *fakeSlackClient) UsersConversations(context.Context, string, bool, int, string) (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	page := c.pages[c.convCalls]
	c.convCalls++
	return page, nil
}

func (c *fakeSlackClient) ConversationsInfo(_ context.Context, channel string) (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	resp := c.infos[len(c.infoCalls)]
	c.infoCalls = append(c.infoCalls, channel)
	return resp, nil
}

func (c *fakeSlackClient) UsersInfo(context.Context, string) (map[string]any, error) {
	return map[string]any{"ok": false}, nil
}

type slackAdapter struct {
	*baseFakeAdapter
	teams *pydict.Dict[string, SlackDirectoryClient]
}

func (a *slackAdapter) SlackTeamClients() *pydict.Dict[string, SlackDirectoryClient] { return a.teams }

// Adapter table holding the given adapters in order
func adapterTable(pairs ...any) *pydict.Dict[Platform, PlatformAdapter] {
	d := pydict.New[Platform, PlatformAdapter]()
	for i := 0; i < len(pairs); i += 2 {
		d.Set(pairs[i].(Platform), pairs[i+1].(PlatformAdapter))
	}
	return d
}

// Writes sessions.json under the test home holding the given origins
func writeSessionOrigins(t *testing.T, homeDir string, origins ...map[string]any) {
	t.Helper()
	sessions := map[string]any{}
	for i, o := range origins {
		sessions["s"+string(rune('a'+i))] = map[string]any{"origin": o}
	}
	raw, _ := json.Marshal(sessions)
	if err := os.MkdirAll(filepath.Join(homeDir, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homeDir, "sessions", "sessions.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Slack adapter whose single workspace client is c
func slackTeam(c *fakeSlackClient) *slackAdapter {
	teams := pydict.New[string, SlackDirectoryClient]()
	teams.Set("T1", c)
	return &slackAdapter{newBaseFake(PlatformSlack), teams}
}

// Entry field values of a directory list
func entryField(t *testing.T, entries []any, key string) []any {
	t.Helper()
	var out []any
	for _, e := range entries {
		v, _ := e.(*pyjson.Object).Get(key)
		out = append(out, v)
	}
	return out
}

func TestBuildChannelDirectoryWritesFailedWritePreservesPreviousCache(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	dir := t.TempDir()
	isolateChannelDirectory(t, dir)
	writeChannelDirectory(t, dir, `{"telegram": [{"id": "123", "name": "Alice", "type": "dm"}]}`)
	previous := loadChannelDirectory(context.Background())
	saved := writeChannelDirectoryFn
	t.Cleanup(func() { writeChannelDirectoryFn = saved })
	writeChannelDirectoryFn = func(ctx context.Context, path string, data any) error {
		return config.AtomicJSONWrite(ctx, path, []any{data, make(chan int)})
	}
	if _, err := BuildChannelDirectory(context.Background(), adapterTable()); err != nil {
		t.Fatal(err)
	}
	want, _ := pyjson.Dumps(previous)
	got, _ := pyjson.Dumps(loadChannelDirectory(context.Background()))
	if got != want {
		t.Fatalf("directory changed: %s want %s", got, want)
	}
}

func TestBuildChannelDirectoryWritesUsesAdapterListChannelsWhenAvailable(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	isolateChannelDirectory(t, t.TempDir())
	adapter := &listingAdapter{newBaseFake(PlatformTelegram), []any{
		map[string]any{"id": "default", "name": "主对话", "type": "dm"},
		map[string]any{"id": "family_1", "name": "达拉崩吧", "type": "group"},
		map[string]any{"id": "", "name": "ignored", "type": "dm"},
		map[string]any{"id": "family_1", "name": "duplicate", "type": "group"},
	}}
	directory, err := BuildChannelDirectory(context.Background(), adapterTable(PlatformTelegram, adapter))
	if err != nil {
		t.Fatal(err)
	}
	platforms, _ := directory.Get("platforms")
	telegram, _ := platforms.(*pyjson.Object).Get("telegram")
	got, _ := pyjson.Dumps(telegram, pyjson.EnsureASCII(false))
	want := `[{"id": "default", "name": "主对话", "type": "dm"}, {"id": "family_1", "name": "达拉崩吧", "type": "group"}]`
	if got != want {
		t.Fatalf("telegram %s", got)
	}
}

func TestBuildChannelDirectoryOffloadDiscordBuilderRunsOffEventLoopThread(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	isolateChannelDirectory(t, t.TempDir())
	client := &fakeDiscordClient{}
	adapter := &discordAdapter{newBaseFake(PlatformDiscord), client}
	if _, err := BuildChannelDirectory(context.Background(), adapterTable(PlatformDiscord, adapter)); err != nil {
		t.Fatal(err)
	}
	loop := pythread.GoroutineID()
	if len(client.callers) == 0 {
		t.Fatal("discord builder never ran")
	}
	for _, id := range client.callers {
		if id == loop {
			t.Fatal("discord builder ran on the caller goroutine")
		}
	}
}

func TestBuildChannelDirectoryOffloadDirectoryWriteRunsOffEventLoopThread(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	isolateChannelDirectory(t, t.TempDir())
	saved := writeChannelDirectoryFn
	t.Cleanup(func() { writeChannelDirectoryFn = saved })
	var writers []uint64
	writeChannelDirectoryFn = func(context.Context, string, any) error {
		writers = append(writers, pythread.GoroutineID())
		return nil
	}
	adapter := &discordAdapter{newBaseFake(PlatformDiscord), &fakeDiscordClient{}}
	if _, err := BuildChannelDirectory(context.Background(), adapterTable(PlatformDiscord, adapter)); err != nil {
		t.Fatal(err)
	}
	loop := pythread.GoroutineID()
	if len(writers) == 0 {
		t.Fatal("directory was never written")
	}
	for _, id := range writers {
		if id == loop {
			t.Fatal("directory write ran on the caller goroutine")
		}
	}
}

func TestBuildSlackNoTeamClientsFallsBackToSessions(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("ICEBATH_HOME", homeDir)
	writeSessionOrigins(t, homeDir, map[string]any{"platform": "slack", "chat_id": "D123", "chat_name": "Alice"})
	adapter := &slackAdapter{newBaseFake(PlatformSlack), pydict.New[string, SlackDirectoryClient]()}
	entries, err := buildSlack(context.Background(), adapter)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entryField(t, entries, "id")[0] != "D123" {
		t.Fatalf("entries %v", entryField(t, entries, "id"))
	}
}

func TestBuildSlackListsChannelsFromUsersConversations(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	client := &fakeSlackClient{pages: []map[string]any{{
		"ok": true,
		"channels": []any{
			map[string]any{"id": "C0B0QV5434G", "name": "engineering", "is_private": false},
			map[string]any{"id": "G123ABCDEF", "name": "secret-chat", "is_private": true},
		},
		"response_metadata": map[string]any{},
	}}}
	entries, err := buildSlack(context.Background(), slackTeam(client))
	if err != nil {
		t.Fatal(err)
	}
	types := map[any]any{}
	ids, kinds := entryField(t, entries, "id"), entryField(t, entries, "type")
	for i := range ids {
		types[ids[i]] = kinds[i]
	}
	if !reflect.DeepEqual(types, map[any]any{"C0B0QV5434G": "channel", "G123ABCDEF": "private"}) {
		t.Fatalf("types %v", types)
	}
	if client.convCalls != 1 {
		t.Fatalf("users.conversations awaited %d times", client.convCalls)
	}
}

func TestBuildSlackPaginatesViaResponseMetadataCursor(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	client := &fakeSlackClient{pages: []map[string]any{
		{"ok": true, "channels": []any{map[string]any{"id": "C001", "name": "first", "is_private": false}}, "response_metadata": map[string]any{"next_cursor": "cur1"}},
		{"ok": true, "channels": []any{map[string]any{"id": "C002", "name": "second", "is_private": false}}, "response_metadata": map[string]any{"next_cursor": ""}},
	}}
	entries, err := buildSlack(context.Background(), slackTeam(client))
	if err != nil {
		t.Fatal(err)
	}
	if ids := entryField(t, entries, "id"); !reflect.DeepEqual(ids, []any{"C001", "C002"}) {
		t.Fatalf("ids %v", ids)
	}
	if client.convCalls != 2 {
		t.Fatalf("users.conversations awaited %d times", client.convCalls)
	}
}

func TestBuildSlackThreadIDsUseBaseConversationAndDedupeInfoCalls(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("ICEBATH_HOME", homeDir)
	writeSessionOrigins(t, homeDir,
		map[string]any{"platform": "slack", "chat_id": "C001", "thread_id": "111"},
		map[string]any{"platform": "slack", "chat_id": "C001", "thread_id": "222"},
		map[string]any{"platform": "slack", "chat_id": "C002", "thread_id": "333"},
	)
	client := &fakeSlackClient{
		pages: []map[string]any{{"ok": true, "channels": []any{}, "response_metadata": map[string]any{}}},
		infos: []map[string]any{
			{"ok": true, "channel": map[string]any{"name": "engineering"}},
			{"ok": true, "channel": map[string]any{"name": "support"}},
		},
	}
	entries, err := buildSlack(context.Background(), slackTeam(client))
	if err != nil {
		t.Fatal(err)
	}
	names := map[any]bool{}
	for _, n := range entryField(t, entries, "name") {
		names[n] = true
	}
	if !reflect.DeepEqual(names, map[any]bool{"engineering": true, "support": true}) {
		t.Fatalf("names %v", names)
	}
	calls := append([]string(nil), client.infoCalls...)
	sort.Strings(calls)
	if !reflect.DeepEqual(calls, []string{"C001", "C002"}) {
		t.Fatalf("conversations.info calls %v", client.infoCalls)
	}
}

func TestChannelAliasesAliasPersistsThroughRebuild(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	dir := t.TempDir()
	isolateChannelDirectory(t, dir)
	channelAliasesPath = filepath.Join(dir, "channel_aliases.json")
	if err := os.WriteFile(channelAliasesPath, []byte(`{"whatsapp": {"120363@g.us": "general"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildChannelDirectory(context.Background(), adapterTable()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(channelDirectoryPath)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk struct {
		Platforms map[string][]map[string]any `json:"platforms"`
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	var names []any
	for _, e := range onDisk.Platforms["whatsapp"] {
		if e["id"] == "120363@g.us" {
			names = append(names, e["name"])
		}
	}
	if !reflect.DeepEqual(names, []any{"general"}) {
		t.Fatalf("names %v", names)
	}
}

func TestBuildDiscordFiltersHiddenChannels(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	guild := fakeDiscordGuild{
		name: "TestGuild",
		text: []DiscordDirectoryChannel{
			directoryDiscordChannel{1, "general", 0},
			directoryDiscordChannel{2, "secret", DiscordChannelObfuscatedFlag},
			directoryDiscordChannel{3, DiscordObfuscatedChannelName, 0},
		},
		forums: []DiscordDirectoryChannel{
			directoryDiscordChannel{4, "forum-open", 0},
			directoryDiscordChannel{5, "forum-secret", DiscordChannelObfuscatedFlag},
		},
	}
	adapter := &discordAdapter{newBaseFake(PlatformDiscord), &fakeDiscordClient{guilds: []DiscordDirectoryGuild{guild}}}
	channels, err := buildDiscord(context.Background(), adapter)
	if err != nil {
		t.Fatal(err)
	}
	got := map[any]any{}
	ids, kinds := entryField(t, channels, "id"), entryField(t, channels, "type")
	for i := range ids {
		got[ids[i]] = kinds[i]
	}
	if !reflect.DeepEqual(got, map[any]any{"1": "channel", "4": "forum"}) {
		t.Fatalf("channels %v", got)
	}
}

func TestDoesNotResurrectDisconnectedPlatformsFromSessionHistory(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("ICEBATH_HOME", homeDir)
	isolateChannelDirectory(t, t.TempDir())
	writeSessionOrigins(t, homeDir,
		map[string]any{"platform": "telegram", "chat_id": "1", "chat_name": "old"},
		map[string]any{"platform": "whatsapp", "chat_id": "2", "chat_name": "old"},
		map[string]any{"platform": "signal", "chat_id": "3", "chat_name": "old"},
		map[string]any{"platform": "matrix", "chat_id": "4", "chat_name": "old"},
	)
	directory, err := BuildChannelDirectory(context.Background(), adapterTable(PlatformTelegram, newBaseFake(PlatformTelegram)))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := directory.Get("platforms")
	plats := raw.(*pyjson.Object)
	if _, ok := plats.Get("telegram"); !ok {
		t.Fatal("telegram missing")
	}
	for _, stale := range []string{"whatsapp", "signal", "matrix"} {
		if _, ok := plats.Get(stale); ok {
			t.Fatalf("%s resurrected from session history", stale)
		}
	}
	if !reflect.DeepEqual(plats.Keys, []string{"telegram"}) {
		t.Fatalf("platforms %v", plats.Keys)
	}
}
