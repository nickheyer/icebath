package discord

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Guild click by the user carrying the given roles
func componentInteraction(userID string, roles ...string) *discordgo.Interaction {
	return &discordgo.Interaction{ChannelID: "555", GuildID: "999", Member: &discordgo.Member{User: &discordgo.User{ID: userID}, Roles: roles}}
}

// Adapter with every component gate env cleared
func componentAdapter(t *testing.T) *DiscordAdapter {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, v := range []string{"DISCORD_ALLOW_ALL_USERS", "GATEWAY_ALLOW_ALL_USERS", "GATEWAY_ALLOWED_USERS", "DISCORD_ALLOWED_USERS"} {
		t.Setenv(v, "")
	}
	return newTestAdapter(t, nil)
}

func TestComponentCheckExplicitAllowAllPasses(t *testing.T) {
	for _, env := range [][2]string{{"DISCORD_ALLOW_ALL_USERS", "true"}, {"GATEWAY_ALLOW_ALL_USERS", "yes"}} {
		a := componentAdapter(t)
		t.Setenv(env[0], env[1])
		if !a.componentCheckAuth(context.Background(), componentInteraction("11111"), strSet{}, strSet{}, nil) {
			t.Fatalf("%s=%s rejected", env[0], env[1])
		}
	}
}

func TestSnapshotUserIsConfirmedWithTheLiveCheck(t *testing.T) {
	yes, no := true, false
	for _, snap := range []string{"11111", "*"} {
		for _, c := range []struct {
			verdict  *bool
			expected bool
		}{{nil, true}, {&yes, true}, {&no, false}} {
			a := componentAdapter(t)
			var seen []string
			live := func(i *discordgo.Interaction) *bool {
				seen = append(seen, interactionUser(i).ID)
				return c.verdict
			}
			if got := a.componentCheckAuth(context.Background(), componentInteraction("11111"), newStrSet(snap), strSet{}, live); got != c.expected || len(seen) != 1 || seen[0] != "11111" {
				t.Fatalf("snapshot %s verdict %v got %v seen %v", snap, c.verdict, got, seen)
			}
		}
	}
}

// View under test from the live store of a sent prompt message
func liveView[T any](t *testing.T, a *DiscordAdapter, messageID any) T {
	t.Helper()
	v := a.views.lookup(fmt.Sprint(messageID))
	if v == nil {
		t.Fatalf("no live view on message %v", messageID)
	}
	view, ok := v.hooks.(T)
	if !ok {
		t.Fatalf("view %T", v.hooks)
	}
	return view
}

func TestApprovalButtonsFollowARevokeMadeAfterConnect(t *testing.T) {
	a, _ := sendAdapter(t)
	a.allowedUserIDs = newStrSet("11111")
	live := map[string]bool{"11111": true}
	var mu sync.Mutex
	a.SetAuthorizationCheck(func(userID string, chatType, chatID *string, opts gateway.AuthorizationOpts) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		return live[userID], nil
	})
	result, err := a.SendExecApproval(context.Background(), "555", "make deploy", "discord:555", nil, nil, true, true, false, nil)
	if err != nil || !result.Success {
		t.Fatalf("send = %+v %v", result, err)
	}
	view := liveView[*execApprovalView](t, a, result.MessageID)
	if !view.checkAuth(context.Background(), componentInteraction("11111")) {
		t.Fatal("allowed user rejected")
	}
	mu.Lock()
	live["11111"] = false
	mu.Unlock()
	if view.checkAuth(context.Background(), componentInteraction("11111")) {
		t.Fatal("revoked user still accepted")
	}
}

// Every view kind built with the given allowlists
func roleViews(t *testing.T, a *DiscordAdapter, users, roles strSet) map[string]viewHooks {
	t.Helper()
	ctx := context.Background()
	ev, err := a.newExecApprovalView(ctx, "sess-1", users, roles, false, nil, true, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	sc, err := a.newSlashConfirmView(ctx, "sess-1", "c1", users, roles)
	if err != nil {
		t.Fatal(err)
	}
	up, err := a.newUpdatePromptView(ctx, "sess-1", users, roles)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := a.newClarifyChoiceView(ctx, []string{"one", "two"}, "clarify-1", users, roles)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]viewHooks{"exec": ev, "slash": sc, "update": up, "clarify": cl}
}

func TestExecApprovalViewAcceptsRoleAllowlist(t *testing.T) {
	a := componentAdapter(t)
	v := roleViews(t, a, newStrSet("11111"), newStrSet("42"))["exec"]
	if !v.checkAuth(context.Background(), componentInteraction("99999", "42")) || v.checkAuth(context.Background(), componentInteraction("99999", "7")) {
		t.Fatal("role allowlist not honoured")
	}
}

func TestSlashConfirmViewAcceptsRoleAllowlist(t *testing.T) {
	a := componentAdapter(t)
	v := roleViews(t, a, strSet{}, newStrSet("42"))["slash"]
	if !v.checkAuth(context.Background(), componentInteraction("99999", "42")) || v.checkAuth(context.Background(), componentInteraction("99999", "7")) {
		t.Fatal("role allowlist not honoured")
	}
}

func TestUpdatePromptViewAcceptsRoleAllowlist(t *testing.T) {
	a := componentAdapter(t)
	v := roleViews(t, a, strSet{}, newStrSet("42"))["update"]
	if !v.checkAuth(context.Background(), componentInteraction("99999", "42")) || v.checkAuth(context.Background(), componentInteraction("99999", "7")) {
		t.Fatal("role allowlist not honoured")
	}
}

func TestClarifyChoiceViewAcceptsRoleAllowlist(t *testing.T) {
	a := componentAdapter(t)
	v := roleViews(t, a, strSet{}, newStrSet("42"))["clarify"]
	if !v.checkAuth(context.Background(), componentInteraction("99999", "42")) || v.checkAuth(context.Background(), componentInteraction("99999", "7")) {
		t.Fatal("role allowlist not honoured")
	}
}

func TestViewsEmptyAllowlistsRejectByDefault(t *testing.T) {
	a := componentAdapter(t)
	for name, v := range roleViews(t, a, strSet{}, nil) {
		t.Run(name, func(t *testing.T) {
			if v.checkAuth(context.Background(), componentInteraction("99999")) {
				t.Fatal("empty allowlists admitted a stranger")
			}
		})
	}
}

func TestModelPickerViewEmptyAllowlistsRejectByDefault(t *testing.T) {
	a := componentAdapter(t)
	s := attachClient(t, a, "1", []string{"999"})
	f := newFakeDiscordAPI(t, s)
	noop := func(context.Context, string, string, string) (string, error) { return "", nil }
	view, err := a.newModelPickerView(context.Background(), nil, "m", "p", "s", noop, strSet{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if view.allowedRoleIDs == nil || len(view.allowedRoleIDs) != 0 {
		t.Fatalf("allowed roles = %v", view.allowedRoleIDs)
	}
	if view.checkAuth(context.Background(), componentInteraction("99999")) {
		t.Fatal("stranger admitted")
	}
	stranger := clickOn(&discordgo.Message{ID: "1", ChannelID: "555"}, "model_cancel", discordgo.ButtonComponent, "99999")
	if err := view.onCancel(context.Background(), stranger); err != nil {
		t.Fatal(err)
	}
	if view.isResolved() {
		t.Fatal("a stranger closed the picker")
	}
	if got := ephemeralReply(t, f, stranger); got != unauthorized(context.Background()) {
		t.Fatalf("reply = %q", got)
	}
}

func TestViewEmptyAllowlistsAllowWithExplicitAllowAll(t *testing.T) {
	a := componentAdapter(t)
	t.Setenv("DISCORD_ALLOW_ALL_USERS", "true")
	if !roleViews(t, a, strSet{}, nil)["exec"].checkAuth(context.Background(), componentInteraction("99999")) {
		t.Fatal("explicit allow all rejected")
	}
}

func TestComponentCheckPairingApprovedUserPasses(t *testing.T) {
	a := componentAdapter(t)
	ctx := context.Background()
	store, err := gateway.NewPairingStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	code, ok, err := store.GenerateCode(ctx, "discord", "11111", "pal")
	if err != nil || !ok {
		t.Fatalf("code = %v %v %v", code, ok, err)
	}
	if _, err := store.ApproveCode(ctx, "discord", code); err != nil {
		t.Fatal(err)
	}
	if !a.componentCheckAuth(ctx, componentInteraction("11111"), strSet{}, strSet{}, nil) {
		t.Fatal("pairing approved user rejected")
	}
}

func TestAdminGateResolverOnParsesAdmins(t *testing.T) {
	requireAdmin, admins := ResolveExecApprovalAdminGate(map[string]any{"require_admin_for_exec_approval": true, "allow_admin_from": "111, 222"})
	if !requireAdmin || len(admins) != 2 {
		t.Fatalf("gate = %v %v", requireAdmin, admins)
	}
	for _, id := range []string{"111", "222"} {
		if _, ok := admins[id]; !ok {
			t.Fatalf("admins = %v", admins)
		}
	}
	_, list := ResolveExecApprovalAdminGate(map[string]any{"require_admin_for_exec_approval": "true", "allow_admin_from": []any{111, 222}})
	if len(list) != 2 {
		t.Fatalf("list admins = %v", list)
	}
	for _, id := range []string{"111", "222"} {
		if _, ok := list[id]; !ok {
			t.Fatalf("list admins = %v", list)
		}
	}
}

func TestExecViewGateOnNonAdminRejected(t *testing.T) {
	a := componentAdapter(t)
	v, err := a.newExecApprovalView(context.Background(), "s", newStrSet("11111", "22222"), nil, true, map[string]struct{}{"11111": {}}, true, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if v.checkAuth(context.Background(), componentInteraction("22222")) {
		t.Fatal("non admin approved")
	}
}

func TestExecViewGateOnNoAdminsFailsClosed(t *testing.T) {
	a := componentAdapter(t)
	v, err := a.newExecApprovalView(context.Background(), "s", newStrSet("11111"), nil, true, map[string]struct{}{}, true, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)
	if v.checkAuth(context.Background(), componentInteraction("11111")) {
		t.Fatal("gate with no admins admitted a click")
	}
	if !strings.Contains(buf.String(), "require_admin_for_exec_approval") {
		t.Fatalf("log = %q", buf.String())
	}
}

func TestOtherViewsNotAdminGated(t *testing.T) {
	a := componentAdapter(t)
	sc, err := a.newSlashConfirmView(context.Background(), "s", "c", newStrSet("11111"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !sc.checkAuth(context.Background(), componentInteraction("11111")) {
		t.Fatal("slash confirm took an admin gate")
	}
}
