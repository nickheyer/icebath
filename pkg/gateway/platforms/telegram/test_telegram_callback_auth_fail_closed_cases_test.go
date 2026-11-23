package telegram

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Closure message handler with no bound runner
func closureHandler(context.Context, *gateway.MessageEvent) (any, error) { return nil, nil }

func TestCallbackAuthFailsClosedWithoutAnAccessCheck(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	setSetting(t, settings.TelegramAllowFrom.Key, []any{"12345"})
	setSetting(t, settings.GatewayAllowAllUsers.Key, true)
	core := newTestCore()
	if core.isCallbackUserAuthorized("12345", nil, nil, nil) {
		t.Fatal("must fail closed")
	}
}

func TestCallbackAuthUsesTheAccessCheck(t *testing.T) {
	core := newTestCore()
	core.base.SetMessageHandler(closureHandler)
	seen := map[string]any{}
	core.base.SetAuthorizationCheck(func(userID string, chatType, chatID *string, _ gateway.AuthorizationOpts) (any, error) {
		seen["user_id"], seen["chat_type"], seen["chat_id"] = userID, *chatType, *chatID
		return userID == "999", nil
	})
	chatID, chatType := "777", "supergroup"
	if !core.isCallbackUserAuthorized("999", &chatID, &chatType, nil) {
		t.Fatal("the access check must allow")
	}
	if !reflect.DeepEqual(seen, map[string]any{"user_id": "999", "chat_type": "group", "chat_id": "777"}) {
		t.Fatalf("got %v", seen)
	}
}

func TestCallbackAuthDenialWinsOverTheAllowList(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	setSetting(t, settings.TelegramAllowFrom.Key, []any{"12345"})
	core := newTestCore()
	core.base.SetMessageHandler(closureHandler)
	core.base.SetAuthorizationCheck(func(string, *string, *string, gateway.AuthorizationOpts) (any, error) { return false, nil })
	if core.isCallbackUserAuthorized("12345", nil, nil, nil) {
		t.Fatal("the access check's denial must win")
	}
}
