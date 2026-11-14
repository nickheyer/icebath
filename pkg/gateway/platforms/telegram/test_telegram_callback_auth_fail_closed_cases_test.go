package telegram

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Closure message handler with no bound runner
func closureHandler(context.Context, *gateway.MessageEvent) (any, error) { return nil, nil }

func TestCallbackAuthFailClosedNoAllowlistNoAllowAllDenies(t *testing.T) {
	t.Setenv("TELEGRAM_ALLOWED_USERS", "")
	t.Setenv("GATEWAY_ALLOW_ALL_USERS", "")
	core := newTestCore()
	if core.isCallbackUserAuthorized(context.Background(), "12345", nil, nil, nil, nil) {
		t.Fatal("must fail closed")
	}
}

func TestCallbackAuthFailClosedAllowlistWithMatchingUserPermits(t *testing.T) {
	t.Setenv("TELEGRAM_ALLOWED_USERS", "12345,67890")
	core := newTestCore()
	if !core.isCallbackUserAuthorized(context.Background(), "12345", nil, nil, nil, nil) {
		t.Fatal("allowlisted user must pass")
	}
}

func TestCallbackAuthPrefersInjectedCheckInjectedCheckUsedWhenHandlerIsAClosure(t *testing.T) {
	t.Setenv("TELEGRAM_ALLOWED_USERS", "")
	t.Setenv("GATEWAY_ALLOW_ALL_USERS", "")
	core := newTestCore()
	core.base.SetMessageHandler(closureHandler)
	seen := map[string]any{}
	core.base.SetAuthorizationCheck(func(userID string, chatType, chatID *string, _ gateway.AuthorizationOpts) (any, error) {
		seen["user_id"], seen["chat_type"], seen["chat_id"] = userID, *chatType, *chatID
		return userID == "999", nil
	})
	chatID, chatType := "777", "supergroup"
	if !core.isCallbackUserAuthorized(context.Background(), "999", &chatID, &chatType, nil, nil) {
		t.Fatal("injected check must allow")
	}
	if !reflect.DeepEqual(seen, map[string]any{"user_id": "999", "chat_type": "group", "chat_id": "777"}) {
		t.Fatalf("got %v", seen)
	}
}

func TestCallbackAuthPrefersInjectedCheckInjectedCheckDenyWinsOverEnvAllowlist(t *testing.T) {
	t.Setenv("TELEGRAM_ALLOWED_USERS", "12345")
	core := newTestCore()
	core.base.SetMessageHandler(closureHandler)
	core.base.SetAuthorizationCheck(func(string, *string, *string, gateway.AuthorizationOpts) (any, error) { return false, nil })
	if core.isCallbackUserAuthorized(context.Background(), "12345", nil, nil, nil, nil) {
		t.Fatal("injected deny must win")
	}
}
