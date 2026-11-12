package discord

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func TestPrivilegedIntentsRequiredFatalNeedsMembersIntentRules(t *testing.T) {
	cases := []struct {
		users, roles []string
		want         bool
	}{
		{nil, nil, false},
		{[]string{"*"}, nil, false},
		{[]string{"769524422783664158"}, nil, false},
		{[]string{"alice"}, nil, true},
		{nil, []string{"111"}, true},
	}
	for _, c := range cases {
		if got := needsServerMembersIntent(c.users, c.roles); got != c.want {
			t.Fatalf("needs(%v, %v) = %v, want %v", c.users, c.roles, got, c.want)
		}
	}
}

// Adapter whose live client caches the given guilds with their full member lists
func usernameAdapter(t *testing.T, allowed []string, guilds ...*discordgo.Guild) *DiscordAdapter {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := newTestAdapter(t, nil)
	a.allowedUserIDs = newStrSet(allowed...)
	attachClient(t, a, "1", nil)
	for _, g := range guilds {
		if err := a.client.State.GuildAdd(g); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

func member(uid, name, nick, globalName string) *discordgo.Member {
	return &discordgo.Member{User: &discordgo.User{ID: uid, Username: name, GlobalName: globalName, Discriminator: "0"}, Nick: nick}
}

func TestResolveAllowedUsernamesIgnoresDisplayNames(t *testing.T) {
	for _, c := range [][2]string{{"alice", ""}, {"", "alice"}} {
		impostor := member("111", "mallory", c[0], c[1])
		owner := member("222", "alice", "", "Alice W")
		a := usernameAdapter(t, []string{"alice"},
			&discordgo.Guild{ID: "1", Name: "g1", Members: []*discordgo.Member{impostor}, MemberCount: 1},
			&discordgo.Guild{ID: "2", Name: "g2", Members: []*discordgo.Member{owner}, MemberCount: 1})
		t.Setenv("DISCORD_ALLOWED_USERS", "alice")
		out := captureStdout(t, func() { a.resolveAllowedUsernames(context.Background()) })
		if !maps.Equal(a.allowedUserIDs, newStrSet("222")) || strings.Contains(out, "display name") {
			t.Fatalf("allowed %v out %q", a.allowedUserIDs, out)
		}
	}
}

func TestResolveAllowedUsernamesPreservesWildcard(t *testing.T) {
	for _, initial := range [][]string{{"*"}, {"769524422783664158", "*"}} {
		a := usernameAdapter(t, initial)
		sorted := append([]string(nil), initial...)
		slices.Sort(sorted)
		t.Setenv("DISCORD_ALLOWED_USERS", strings.Join(sorted, ","))
		a.resolveAllowedUsernames(context.Background())
		if !a.allowedUserIDs.has("*") {
			t.Fatal("wildcard must survive username resolution")
		}
		if !slices.Contains(strings.Split(os.Getenv("DISCORD_ALLOWED_USERS"), ","), "*") {
			t.Fatal("env lost the wildcard")
		}
	}
}

// Text a function prints on stdout
func captureStdout(t *testing.T, fn func()) string {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	fn()
	os.Stdout = prev
	w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

func TestSafeSyncSlashCommandsOnlyMutatesDiffs(t *testing.T) {
	a, api := sendAdapter(t)
	prev := commandSyncMutationInterval
	commandSyncMutationInterval = 0
	t.Cleanup(func() { commandSyncMutationInterval = prev })
	a.client.State.Application = &discordgo.Application{ID: "999"}
	cmd := func(name, desc string) map[string]any {
		return map[string]any{"name": name, "description": desc, "type": int64(1), "options": []any{}, "nsfw": false, "dm_permission": true, "default_member_permissions": nil}
	}
	desiredSame, desiredUpdated, desiredCreated := cmd("status", "Show Hermes session status"), cmd("help", "Show available commands"), cmd("metricas", "Show Colmeio metrics dashboard")
	a.desiredCommands = []map[string]any{desiredSame, desiredUpdated, desiredCreated}
	api.mu.Lock()
	api.get = map[string]string{"/applications/999/commands": `[
		{"id":"11","application_id":"999","type":1,"name":"status","description":"Show Hermes session status","options":[],"nsfw":false,"dm_permission":true,"name_localizations":{},"description_localizations":{}},
		{"id":"12","application_id":"999","type":1,"name":"help","description":"Old help text","options":[],"nsfw":false,"dm_permission":true},
		{"id":"13","application_id":"999","type":1,"name":"old-command","description":"To be deleted","options":[],"nsfw":false,"dm_permission":true}]`}
	api.mu.Unlock()
	summary, err := a.safeSyncSlashCommands(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"total": 3, "unchanged": 1, "updated": 1, "recreated": 0, "created": 1, "deleted": 1}
	if !maps.Equal(summary, want) {
		t.Fatalf("summary %v", summary)
	}
	edits := api.matching(http.MethodPatch, "/applications/999/commands/12")
	creates := api.matching(http.MethodPost, "/applications/999/commands")
	deletes := api.matching(http.MethodDelete, "/applications/999/commands/13")
	if len(edits) != 1 || edits[0].Body["description"] != "Show available commands" || len(creates) != 1 || creates[0].Body["name"] != "metricas" || len(deletes) != 1 {
		t.Fatalf("edits %v creates %v deletes %d", edits, creates, len(deletes))
	}
}

func TestReconnectClosesPreviousClientToPreventZombieWebsocket(t *testing.T) {
	a := newConnectAdapter(t, nil)
	emptySlashRegistry(t)
	created := newFakeGateway(t).installBot(t)
	ctx := context.Background()
	if ok, _ := a.Connect(ctx, false); !ok {
		t.Fatalf("first connect failed: %v", a.FatalErrorCode())
	}
	if len(*created) != 1 || (*created)[0].isClosed() {
		t.Fatalf("first bot should be open after connect, created=%d", len(*created))
	}
	first := (*created)[0]
	if ok, _ := a.Connect(ctx, false); !ok {
		t.Fatal("second connect failed")
	}
	if len(*created) != 2 {
		t.Fatalf("created %d bots", len(*created))
	}
	second := (*created)[1]
	if !first.isClosed() {
		t.Fatal("First Discord client must be closed on re-entry of connect() to prevent zombie websocket (#18187)")
	}
	if second.isClosed() || a.link != second {
		t.Fatal("second bot should still be open and current")
	}
	a.Disconnect(ctx)
}

func TestConnectClearsPreviousFatalError(t *testing.T) {
	a := newConnectAdapter(t, nil)
	emptySlashRegistry(t)
	newFakeGateway(t).installBot(t)
	ctx := context.Background()
	a.SetFatalError(ctx, "discord_connect_error", "Discord startup failed: boom", true)
	if !a.HasFatalError() {
		t.Fatal("fatal error not recorded")
	}
	if ok, _ := a.Connect(ctx, false); !ok {
		t.Fatal("connect failed")
	}
	if a.HasFatalError() || a.FatalErrorCode() != nil || a.FatalErrorMessage() != nil || !a.IsConnected() {
		t.Fatal("connect() must clear a previously recorded fatal error via _mark_connected()")
	}
	a.Disconnect(ctx)
}

func TestConnectTimeoutCancelsBotTask(t *testing.T) {
	a := newConnectAdapter(t, nil)
	emptySlashRegistry(t)
	newFakeGateway(t).installBot(t)
	orig := waitForReadyOrBotExit
	waitForReadyOrBotExit = func(context.Context, <-chan struct{}, *botTask, time.Duration) error { return errDiscordReadyTimeout }
	t.Cleanup(func() { waitForReadyOrBotExit = orig })
	ok, _ := a.Connect(context.Background(), false)
	if ok {
		t.Fatal("connect should fail on timeout")
	}
	if a.botTask != nil {
		t.Fatal("_bot_task must be cancelled and cleared on connect() timeout")
	}
	if code := a.FatalErrorCode(); code == nil || *code != "discord_connect_timeout" {
		t.Fatalf("fatal code %v", code)
	}
	a.Disconnect(context.Background())
}

func TestDisconnectCancelsRunningBotTask(t *testing.T) {
	a := newConnectAdapter(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	zombie := newBotTask(cancel)
	go func() {
		<-ctx.Done()
		zombie.finish(ctx.Err())
	}()
	a.botTask = zombie
	a.SetRunning(true)
	a.Disconnect(context.Background())
	if a.botTask != nil {
		t.Fatal("disconnect() must clear _bot_task")
	}
	if !zombie.isDone() || !errors.Is(zombie.err, context.Canceled) {
		t.Fatal("disconnect() must cancel the zombie bot task")
	}
}

func TestPostConnectInitializationRetriesFingerprintAfterTimeout(t *testing.T) {
	a := newConnectAdapter(t, nil)
	ctx := context.Background()
	s, _ := discordgo.New("Bot x")
	s.State.User = &discordgo.User{ID: "999"}
	a.link, a.client = &gatewayLink{Session: s, rateCap: &rateLimitCap{}}, s
	payload, _ := jsonValue(map[string]any{"name": "skill", "description": "Run a skill", "type": 1, "options": []any{}})
	a.desiredCommands = []map[string]any{payload.(map[string]any)}
	fingerprint := a.desiredCommandSyncFingerprint(ctx)
	statePath := commandSyncStatePath(ctx)
	os.MkdirAll(filepath.Dir(statePath), 0o755)
	raw, _ := json.Marshal(map[string]any{"999": map[string]any{"fingerprint": fingerprint, "last_attempt_at": 102.0, "last_success_at": 101.0,
		"summary": map[string]any{"total": 1}}})
	os.WriteFile(statePath, raw, 0o644)
	summary := map[string]int{"total": 1, "unchanged": 0, "updated": 0, "recreated": 0, "created": 1, "deleted": 0}
	calls := 0
	orig := safeSyncSlashCommandsFn
	safeSyncSlashCommandsFn = func(*DiscordAdapter, context.Context) (map[string]int, error) {
		calls++
		if calls == 1 {
			return nil, context.DeadlineExceeded
		}
		return summary, nil
	}
	t.Cleanup(func() { safeSyncSlashCommandsFn = orig })
	read := func() map[string]any {
		var st map[string]map[string]any
		b, _ := os.ReadFile(statePath)
		json.Unmarshal(b, &st)
		return st["999"]
	}
	a.runPostConnectInitialization(ctx)
	entry := read()
	if entry["fingerprint"] != fingerprint {
		t.Fatalf("fingerprint %v", entry["fingerprint"])
	}
	if _, has := entry["last_success_at"]; has {
		t.Fatal("timed out sync must not keep last_success_at")
	}
	if _, has := entry["summary"]; has {
		t.Fatal("timed out sync must not keep summary")
	}
	a.runPostConnectInitialization(ctx)
	if calls != 2 {
		t.Fatalf("sync awaited %d times", calls)
	}
	entry = read()
	if entry["last_success_at"].(float64) < entry["last_attempt_at"].(float64) {
		t.Fatal("success must not predate the attempt")
	}
	got, _ := json.Marshal(entry["summary"])
	want, _ := json.Marshal(summary)
	if string(got) != string(want) {
		t.Fatalf("summary %s want %s", got, want)
	}
}

func TestPrivilegedIntentsRequiredFatalConnectSetsNonRetryableFatal(t *testing.T) {
	a := newConnectAdapter(t, map[string]any{"slash_commands": false})
	fg := newFakeGateway(t)
	fg.closeWith = 4014
	fg.installBot(t)
	ok, _ := a.Connect(context.Background(), false)
	if ok {
		t.Fatal("connect should fail")
	}
	code := a.FatalErrorCode()
	if !a.HasFatalError() || a.FatalErrorRetryable() || code == nil || *code != "discord_intents_required" || a.FatalErrorMessage() == nil || *a.FatalErrorMessage() == "" {
		t.Fatalf("fatal state code=%v retryable=%v identifies=%d", *code, a.FatalErrorRetryable(), fg.identifies)
	}
	if a.botTask != nil {
		t.Fatal("bot task must be cleared")
	}
}

func TestSkillCatalogScanRunsOffTheEventLoop(t *testing.T) {
	a := newConnectAdapter(t, nil)
	emptySlashRegistry(t)
	newFakeGateway(t).installBot(t)
	scan := func(context.Context, map[string]bool) ([][]gateway.SkillCommandEntry, []gateway.SkillCommandEntry, int, error) {
		return nil, []gateway.SkillCommandEntry{{Name: "x", Description: "desc", CmdKey: "/x"}}, 0, nil
	}
	gateway.DiscordSkillCommandsByCategory = scan
	names := func() []string {
		var out []string
		for _, e := range a.skillEntries {
			out = append(out, e.name)
		}
		return out
	}
	ctx := context.Background()
	if ok, _ := a.Connect(ctx, false); !ok || !slices.Equal(names(), []string{"x"}) {
		t.Fatalf("connect registered skills %v", names())
	}
	a.skillEntries = nil
	if n, _ := a.RefreshSkillGroup(ctx); n != 1 || !slices.Equal(names(), []string{"x"}) {
		t.Fatalf("refresh registered skills %v", names())
	}
	a.Disconnect(ctx)
}
