package gateway

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Session source of a chat id with the user id hermes derives from a webhook chat id
func webhookToolsetSource(chatID string) *SessionSource {
	source := &SessionSource{ChatID: chatID}
	if strings.HasPrefix(chatID, "webhook:") {
		user := chatID[:strings.LastIndex(chatID, ":")]
		source.UserID = &user
	}
	return source
}

func TestRouteWithToolsetsReturnsList(t *testing.T) {
	adapter := makeWebhookAdapter(t, map[string]any{"mon": map[string]any{"secret": "x", "toolsets": []any{"terminal", "file"}}}, nil)
	got, ok := adapter.ToolsetsForSource(webhookToolsetSource("webhook:mon:d1"))
	if !ok || !reflect.DeepEqual(got, []string{"terminal", "file"}) {
		t.Fatalf("%v %v", got, ok)
	}
}

func TestUnknownRouteReturnsNone(t *testing.T) {
	adapter := makeWebhookAdapter(t, nil, nil)
	if got, ok := adapter.ToolsetsForSource(webhookToolsetSource("webhook:ghost:d1")); ok || got != nil {
		t.Fatalf("%v", got)
	}
}

func TestNonWebhookChatIDReturnsNone(t *testing.T) {
	adapter := makeWebhookAdapter(t, map[string]any{"mon": map[string]any{"secret": "x", "toolsets": []any{"terminal"}}}, nil)
	if got, ok := adapter.ToolsetsForSource(webhookToolsetSource("telegram:123")); ok || got != nil {
		t.Fatalf("%v", got)
	}
}

func TestEmptyOrNonListToolsetsReturnsNone(t *testing.T) {
	adapter := makeWebhookAdapter(t, map[string]any{"empty": map[string]any{"secret": "x", "toolsets": []any{}},
		"str": map[string]any{"secret": "x", "toolsets": "terminal"}, "blank": map[string]any{"secret": "x", "toolsets": []any{"  ", ""}}}, nil)
	for _, chat := range []string{"webhook:empty:d", "webhook:str:d", "webhook:blank:d"} {
		if got, ok := adapter.ToolsetsForSource(webhookToolsetSource(chat)); ok || got != nil {
			t.Fatalf("%s: %v", chat, got)
		}
	}
}

// Adapter with the dynamic route test global secret unless the extra names one, subscriptions written first
func makeDynamicRouteAdapter(t *testing.T, routes map[string]any, extra map[string]any, subscriptions string) *WebhookAdapter {
	if extra == nil {
		extra = map[string]any{}
	}
	if _, ok := extra["secret"]; !ok {
		extra["secret"] = "test-global-secret"
	}
	adapter := makeWebhookAdapter(t, routes, extra)
	if err := os.WriteFile(filepath.Join(os.Getenv("ICEBATH_HOME"), webhookDynamicRoutesFilename), []byte(subscriptions), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter.ReloadDynamicRoutes(context.Background())
	return adapter
}

func TestLoadsDynamicRoutes(t *testing.T) {
	adapter := makeDynamicRouteAdapter(t, map[string]any{"static": map[string]any{"secret": "s"}}, nil,
		`{"my-hook": {"secret": "dynamic-secret", "prompt": "test", "events": []}}`)
	for _, name := range []string{"my-hook", "static"} {
		if _, ok := adapter.WebhookRoute(name); !ok {
			t.Fatalf("%s missing", name)
		}
	}
}

func TestEmptySecretRejected(t *testing.T) {
	adapter := makeDynamicRouteAdapter(t, nil, nil, `{"evil": {"secret": "", "prompt": "rm -rf"}}`)
	if _, ok := adapter.WebhookRoute("evil"); ok || adapter.dynamicRoutes.Has("evil") {
		t.Fatal("evil route loaded")
	}
}

func TestMissingSecretNoGlobalRejected(t *testing.T) {
	adapter := makeDynamicRouteAdapter(t, nil, map[string]any{"secret": ""}, `{"orphan": {"prompt": "test"}}`)
	if _, ok := adapter.WebhookRoute("orphan"); ok || adapter.dynamicRoutes.Has("orphan") {
		t.Fatal("orphan route loaded")
	}
}

func TestMissingSecretInheritsGlobal(t *testing.T) {
	adapter := makeDynamicRouteAdapter(t, nil, nil, `{"valid": {"prompt": "ok"}}`)
	if _, ok := adapter.WebhookRoute("valid"); !ok {
		t.Fatal("valid route missing")
	}
}
