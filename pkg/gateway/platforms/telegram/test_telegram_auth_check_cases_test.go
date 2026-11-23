package telegram

import (
	"context"
	"testing"

	"github.com/go-telegram/bot/models"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Core with a closure message handler, the adapter state before the background service wires it
func authCore() *adapterCore {
	core := newTestCore()
	core.base.SetMessageHandler(closureHandler)
	return core
}

// Message from a user in a chat of the given Telegram type
func authMessage(fromUserID, chatID int64, chatType string) *models.Message {
	return &models.Message{ID: 42, Text: "hello", Chat: models.Chat{ID: chatID, Type: models.ChatType(chatType), Title: "Test"},
		From: &models.User{ID: fromUserID, FirstName: "Test", LastName: "User"}}
}

// One call of the access check with the sender and chat it was asked about
type accessQuestion struct {
	userID, chatType, chatID string
}

// Access check of the background service allowing only user 111, recording every question
func allowOnly111(core *adapterCore) *[]accessQuestion {
	seen := &[]accessQuestion{}
	core.base.SetAuthorizationCheck(func(userID string, chatType, chatID *string, _ gateway.AuthorizationOpts) (any, error) {
		*seen = append(*seen, accessQuestion{userID, *chatType, *chatID})
		return userID == "111", nil
	})
	return seen
}

// Background service double answering how a profile treats a private chat from someone not allowed
type dmBehaviorRunner struct{ behavior string }

func (r *dmBehaviorRunner) GetUnauthorizedDMBehavior(_ context.Context, platform gateway.Platform, _ *string) string {
	if platform != gateway.PlatformTelegram {
		panic("wrong platform")
	}
	return r.behavior
}

func TestIntakeFollowsTheAccessCheck(t *testing.T) {
	core := authCore()
	allowOnly111(core)
	ctx := context.Background()
	if !core.isUserAuthorizedFromMessage(ctx, authMessage(111, 111, "private")) {
		t.Fatal("111 must pass")
	}
	if core.isUserAuthorizedFromMessage(ctx, authMessage(333, 333, "private")) {
		t.Fatal("333 must be rejected")
	}
}

func TestUnauthorizedDmReachesGatewayUnlessTheProfileIgnoresStrangers(t *testing.T) {
	for behavior, want := range map[string]bool{"pair": true, "decline": true, "ignore": false} {
		core := authCore()
		allowOnly111(core)
		core.base.GatewayRunner = &dmBehaviorRunner{behavior: behavior}
		if got := core.isUserAuthorizedFromMessage(context.Background(), authMessage(999, 999, "private")); got != want {
			t.Fatalf("%s: stranger DM passed = %v, want %v", behavior, got, want)
		}
	}
}

func TestUnauthorizedGroupMessageIsRejectedEvenWhenStrangersArePaired(t *testing.T) {
	core := authCore()
	allowOnly111(core)
	core.base.GatewayRunner = &dmBehaviorRunner{behavior: "pair"}
	if core.isUserAuthorizedFromMessage(context.Background(), authMessage(999, -100, "group")) {
		t.Fatal("groups never pair")
	}
}

func TestAccessCheckGetsTheGroupContext(t *testing.T) {
	core := authCore()
	seen := allowOnly111(core)
	if !core.isUserAuthorizedFromMessage(context.Background(), authMessage(111, -100, "supergroup")) {
		t.Fatal("group member must pass")
	}
	if len(*seen) != 1 || (*seen)[0] != (accessQuestion{"111", "group", "-100"}) {
		t.Fatalf("access check saw %v", *seen)
	}
}

func TestAccessCheckDecidesByChat(t *testing.T) {
	core := authCore()
	core.base.SetAuthorizationCheck(func(_ string, _, chatID *string, _ gateway.AuthorizationOpts) (any, error) {
		return chatID != nil && *chatID == "-100123", nil
	})
	ctx := context.Background()
	if !core.isUserAuthorizedFromMessage(ctx, authMessage(555, -100123, "group")) {
		t.Fatal("allowed chat must pass")
	}
	if core.isUserAuthorizedFromMessage(ctx, authMessage(555, -100999, "group")) {
		t.Fatal("other chat must be rejected")
	}
}

func TestIntakeWithoutAnAccessCheckLeavesTheDecisionToAdmission(t *testing.T) {
	core := authCore()
	if !core.isUserAuthorizedFromMessage(context.Background(), authMessage(555, -100123, "group")) {
		t.Fatal("an adapter no background service wired must hand the message on")
	}
}
