package telegram

import (
	"context"
	"testing"

	"github.com/go-telegram/bot/models"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Core whose config extra carries the overrides of the hermes _make_adapter helper
func authCore(extra map[string]any) *adapterCore {
	core := newTestCore()
	core.base.Config.Extra = extra
	core.base.SetMessageHandler(closureHandler)
	return core
}

// Message of the hermes _make_message helper
func authMessage(fromUserID, chatID int64, chatType string) *models.Message {
	return &models.Message{ID: 42, Text: "hello", Chat: models.Chat{ID: chatID, Type: models.ChatType(chatType), Title: "Test"},
		From: &models.User{ID: fromUserID, FirstName: "Test", LastName: "User"}}
}

// Runner double answering the auth chain and the unauthorized DM behavior
type authRunner struct {
	behavior string
	allow    func(source *gateway.SessionSource) bool
	seen     []*gateway.SessionSource
}

func (r *authRunner) IsUserAuthorized(_ context.Context, source *gateway.SessionSource, _ bool) (bool, error) {
	r.seen = append(r.seen, source)
	return r.allow(source), nil
}

func (r *authRunner) GetUnauthorizedDMBehavior(_ context.Context, platform gateway.Platform, _ *string) string {
	if platform != gateway.PlatformTelegram {
		panic("wrong platform")
	}
	return r.behavior
}

func (r *authRunner) handle(context.Context, *gateway.MessageEvent) (any, error) { return nil, nil }

// Runner allowing only user 111
func userRunner(behavior string) *authRunner {
	return &authRunner{behavior: behavior, allow: func(s *gateway.SessionSource) bool { return s.UserID != nil && *s.UserID == "111" }}
}

func bindRunner(core *adapterCore, runner *authRunner) {
	core.base.SetBoundMessageHandler(runner, runner.handle)
}

func TestIsUserAuthorizedFromMessageAllowFrom(t *testing.T) {
	core := authCore(map[string]any{"allow_from": []any{"111", "222"}})
	ctx := context.Background()
	if !core.isUserAuthorizedFromMessage(ctx, authMessage(111, -100, "dm")) {
		t.Fatal("111 must pass")
	}
	if core.isUserAuthorizedFromMessage(ctx, authMessage(333, -100, "dm")) {
		t.Fatal("333 must be rejected")
	}
}

func TestAllowlistDmWithExplicitPairBehaviorReachesGateway(t *testing.T) {
	t.Setenv("TELEGRAM_ALLOWED_USERS", "111")
	core := authCore(map[string]any{})
	bindRunner(core, userRunner("pair"))
	if !core.isUserAuthorizedFromMessage(context.Background(), authMessage(999, 999, "private")) {
		t.Fatal("pairing DM must reach the gateway")
	}
}

func TestAllowlistDmWithoutPairBehaviorStillEarlyRejects(t *testing.T) {
	t.Setenv("TELEGRAM_ALLOWED_USERS", "111")
	core := authCore(map[string]any{})
	bindRunner(core, userRunner("ignore"))
	if core.isUserAuthorizedFromMessage(context.Background(), authMessage(999, 999, "private")) {
		t.Fatal("ignored DM must be rejected early")
	}
}

func TestAllowFromDmWithPairOverrideReachesGateway(t *testing.T) {
	core := authCore(map[string]any{"allow_from": []any{"111"}, "unauthorized_dm_behavior": "pair"})
	if !core.isUserAuthorizedFromMessage(context.Background(), authMessage(999, 999, "dm")) {
		t.Fatal("pair override must reach the gateway")
	}
}

func TestAllowlistGroupWithPairBehaviorStillEarlyRejects(t *testing.T) {
	t.Setenv("TELEGRAM_ALLOWED_USERS", "111")
	core := authCore(map[string]any{"group_allow_from": []any{"111"}})
	bindRunner(core, userRunner("pair"))
	if core.isUserAuthorizedFromMessage(context.Background(), authMessage(999, -100, "group")) {
		t.Fatal("groups never pair")
	}
}

func TestRunnerAuthGetsGroupUserAllowlistContext(t *testing.T) {
	t.Setenv("TELEGRAM_GROUP_ALLOWED_USERS", "111")
	core := authCore(map[string]any{})
	runner := &authRunner{allow: func(s *gateway.SessionSource) bool {
		return s.ChatType == "group" && s.ChatID == "-100" && s.UserID != nil && *s.UserID == "111"
	}}
	bindRunner(core, runner)
	if !core.isUserAuthorizedFromMessage(context.Background(), authMessage(111, -100, "group")) {
		t.Fatal("group member must pass")
	}
	if len(runner.seen) == 0 || runner.seen[0].ChatType != "group" || runner.seen[0].ChatID != "-100" {
		t.Fatalf("runner saw %v", runner.seen)
	}
}

func TestGroupAllowlistAuthorizedUnderMultiplexClosureHandler(t *testing.T) {
	t.Setenv("TELEGRAM_GROUP_ALLOWED_CHATS", "-100123")
	core := authCore(map[string]any{"group_allowed_chats": []any{"-100123"}})
	if core.base.MessageHandlerOwner() != nil {
		t.Fatal("closure handler has no owner")
	}
	core.base.SetAuthorizationCheck(func(_ string, _, chatID *string, _ gateway.AuthorizationOpts) (any, error) {
		return chatID != nil && pyrepr.Str(*chatID) == "-100123", nil
	})
	ctx := context.Background()
	if !core.isUserAuthorizedFromMessage(ctx, authMessage(555, -100123, "group")) {
		t.Fatal("allowed chat must pass")
	}
	if core.isUserAuthorizedFromMessage(ctx, authMessage(555, -100999, "group")) {
		t.Fatal("other chat must be rejected")
	}
}

func TestMultiplexClosureHandlerWithoutCallbackFallsBackToEnv(t *testing.T) {
	t.Setenv("TELEGRAM_GROUP_ALLOWED_CHATS", "-100123")
	t.Setenv("TELEGRAM_ALLOWED_USERS", "111")
	core := authCore(map[string]any{"group_allowed_chats": []any{"-100123"}})
	ctx := context.Background()
	if !core.isUserAuthorizedFromMessage(ctx, authMessage(111, -100123, "group")) {
		t.Fatal("env allowlisted user must pass")
	}
	if core.isUserAuthorizedFromMessage(ctx, authMessage(555, -100123, "group")) {
		t.Fatal("env unlisted user must be rejected")
	}
}
