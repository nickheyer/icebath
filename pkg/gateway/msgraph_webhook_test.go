package gateway

import (
	"context"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Graph adapter with the hermes test defaults overridden by the given extra keys
func makeMSGraphAdapter(t *testing.T, overrides map[string]any) *MSGraphWebhookAdapter {
	t.Helper()
	extra := map[string]any{"host": "127.0.0.1", "client_state": "expected-client-state",
		"accepted_resources": []any{"communications/onlineMeetings"}}
	for k, v := range overrides {
		extra[k] = v
	}
	config := NewPlatformConfig()
	config.Enabled = true
	config.Extra = extra
	adapter, err := NewMSGraphWebhookAdapter(config)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

// Request with the given query, JSON body, declared length and peer, the hermes fake request
func msgraphFakeRequest(query map[string]string, body string, contentLength int64, remote string) *WebRequest {
	values := url.Values{}
	for k, v := range query {
		values.Set(k, v)
	}
	r := httptest.NewRequest("POST", "/msgraph/webhook?"+values.Encode(), strings.NewReader(body))
	r.ContentLength = contentLength
	if remote == "" {
		remote = "127.0.0.1"
	}
	r.RemoteAddr = remote + ":40000"
	return NewWebRequest(r, 0)
}

func TestGatewayConfigAcceptsMsgraphWebhookPlatform(t *testing.T) {
	ctx := context.Background()
	config, err := GatewayConfigFromDict(ctx, map[string]any{"platforms": map[string]any{"msgraph_webhook": map[string]any{
		"enabled": true, "extra": map[string]any{"client_state": "expected"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := config.Platforms[PlatformMSGraphWebhook]; !ok {
		t.Fatal("msgraph_webhook missing from platforms")
	}
	if !slices.Contains(config.GetConnectedPlatforms(ctx), PlatformMSGraphWebhook) {
		t.Fatal("msgraph_webhook missing from connected platforms")
	}
}

func TestConnectAllowsLoopbackWithoutSourceAllowlist(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	adapter := makeMSGraphAdapter(t, map[string]any{"host": "127.0.0.1", "port": 0, "allowed_source_cidrs": []any{}})
	defer adapter.Disconnect(ctx)
	connected, err := adapter.Connect(ctx, false)
	if err != nil || !connected {
		t.Fatalf("connect = %v %v", connected, err)
	}
	if !adapter.IsConnected() {
		t.Fatal("adapter not connected")
	}
}

func TestValidationTokenEchoOnGet(t *testing.T) {
	adapter := makeMSGraphAdapter(t, nil)
	resp, err := adapter.handleValidation(context.Background(), msgraphFakeRequest(map[string]string{"validationToken": "abc123"}, "", -1, ""))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != 200 || resp.Text() != "abc123" || resp.ContentType() != "text/plain" {
		t.Fatalf("got %d %q %q", resp.Status, resp.Text(), resp.ContentType())
	}
}

func TestMissingClientStateIsAuthRejected(t *testing.T) {
	adapter := makeMSGraphAdapter(t, map[string]any{"client_state": nil})
	body := `{"value": [{"id": "notif-no-client-state", "subscriptionId": "sub-1", "changeType": "updated", "resource": "communications/onlineMeetings/meeting-1"}]}`
	resp, err := adapter.handleNotification(context.Background(), msgraphFakeRequest(nil, body, -1, ""))
	if err != nil || resp.Status != 403 {
		t.Fatalf("got %v %v", resp, err)
	}
}

func TestValidNotificationAcceptedAndScheduled(t *testing.T) {
	adapter := makeMSGraphAdapter(t, nil)
	type scheduledCall struct {
		notification *pyjson.Object
		event        *MessageEvent
	}
	var mu sync.Mutex
	var scheduled []scheduledCall
	adapter.SetNotificationScheduler(func(ctx context.Context, notification *pyjson.Object, event *MessageEvent) error {
		mu.Lock()
		scheduled = append(scheduled, scheduledCall{notification, event})
		mu.Unlock()
		return nil
	})
	body := `{"value": [{"id": "notif-1", "subscriptionId": "sub-1", "changeType": "updated", "resource": "communications/onlineMeetings/meeting-1", "clientState": "expected-client-state", "resourceData": {"id": "meeting-1"}}]}`
	resp, err := adapter.handleNotification(context.Background(), msgraphFakeRequest(nil, body, -1, ""))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != 202 || len(resp.Body) != 0 {
		t.Fatalf("got %d %q", resp.Status, resp.Body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(scheduled) != 1 {
		t.Fatalf("scheduled %d", len(scheduled))
	}
	notification, event := scheduled[0].notification, scheduled[0].event
	if id, _ := notification.Get("id"); id != "notif-1" {
		t.Fatalf("id = %v", id)
	}
	if event.Source.Platform != PlatformMSGraphWebhook || event.Source.ChatType != "webhook" || *event.MessageID != "id:notif-1" {
		t.Fatalf("event = %+v %v", event.Source, *event.MessageID)
	}
}

func TestOversizedNotificationRejectedByContentLength(t *testing.T) {
	adapter := makeMSGraphAdapter(t, map[string]any{"max_body_bytes": 100})
	body := `{"value": [{"id": "notif-oversized", "subscriptionId": "sub-1", "changeType": "updated", "resource": "communications/onlineMeetings/meeting-1", "clientState": "expected-client-state"}]}`
	resp, err := adapter.handleNotification(context.Background(), msgraphFakeRequest(nil, body, 101, ""))
	if err != nil || resp.Status != 413 {
		t.Fatalf("got %v %v", resp, err)
	}
}

func TestNonASCIIClientStateRejectedWithoutRaising(t *testing.T) {
	adapter := makeMSGraphAdapter(t, nil)
	body := `{"value": [{"id": "notif-nonascii", "subscriptionId": "sub-1", "changeType": "updated", "resource": "communications/onlineMeetings/meeting-x", "clientState": "ské-not-the-secret"}]}`
	resp, err := adapter.handleNotification(context.Background(), msgraphFakeRequest(nil, body, -1, ""))
	if err != nil || resp.Status != 403 {
		t.Fatalf("got %v %v", resp, err)
	}
}

func TestResourcePatternsAcceptLeadingSlash(t *testing.T) {
	adapter := makeMSGraphAdapter(t, map[string]any{"accepted_resources": []any{"/communications/onlineMeetings"}})
	adapter.SetNotificationScheduler(func(context.Context, *pyjson.Object, *MessageEvent) error { return nil })
	body := `{"value": [{"id": "notif-slash", "subscriptionId": "sub-1", "changeType": "updated", "resource": "communications/onlineMeetings/meeting-4", "clientState": "expected-client-state"}]}`
	resp, err := adapter.handleNotification(context.Background(), msgraphFakeRequest(nil, body, -1, ""))
	if err != nil || resp.Status != 202 {
		t.Fatalf("got %v %v", resp, err)
	}
}

func TestPublicBindWithoutAllowlistFailsClosed(t *testing.T) {
	adapter := makeMSGraphAdapter(t, map[string]any{"host": "0.0.0.0", "allowed_source_cidrs": []any{}})
	body := `{"value": [{"id": "notif-ip", "resource": "communications/onlineMeetings/m", "clientState": "expected-client-state"}]}`
	resp, err := adapter.handleNotification(context.Background(), msgraphFakeRequest(nil, body, -1, "203.0.113.99"))
	if err != nil || resp.Status != 403 {
		t.Fatalf("got %v %v", resp, err)
	}
}

func TestLoopbackBindWithoutAllowlistStillAcceptsLocalRequests(t *testing.T) {
	adapter := makeMSGraphAdapter(t, map[string]any{"host": "127.0.0.1", "allowed_source_cidrs": []any{}})
	adapter.SetNotificationScheduler(func(context.Context, *pyjson.Object, *MessageEvent) error { return nil })
	body := `{"value": [{"id": "notif-ip-local", "resource": "communications/onlineMeetings/m", "clientState": "expected-client-state"}]}`
	resp, err := adapter.handleNotification(context.Background(), msgraphFakeRequest(nil, body, -1, "127.0.0.1"))
	if err != nil || resp.Status != 202 {
		t.Fatalf("got %v %v", resp, err)
	}
}

func TestPostFromDisallowedIPRejected(t *testing.T) {
	adapter := makeMSGraphAdapter(t, map[string]any{"allowed_source_cidrs": []any{"10.0.0.0/8"}})
	body := `{"value": [{"id": "notif-ip-bad", "resource": "communications/onlineMeetings/m", "clientState": "expected-client-state"}]}`
	resp, err := adapter.handleNotification(context.Background(), msgraphFakeRequest(nil, body, -1, "203.0.113.99"))
	if err != nil || resp.Status != 403 {
		t.Fatalf("got %v %v", resp, err)
	}
}
