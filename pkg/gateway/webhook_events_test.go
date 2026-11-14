package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Captured events of a replaced durable producer, the hermes _capture_admissions fixture
type webhookAdmissions struct {
	mu     sync.Mutex
	events []*MessageEvent
}

func (w *webhookAdmissions) snapshot() []*MessageEvent {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]*MessageEvent(nil), w.events...)
}

// Stands in for the durable producer and the finalizer so every dispatched event is captured
func captureWebhookAdmissions(t *testing.T, adapter *WebhookAdapter) *webhookAdmissions {
	captured := &webhookAdmissions{}
	prevAdmit, prevFinalize, prevAuthority := webhookAdmitProducer, webhookFinalizeDelivery, webhookAuthorityForProfileID
	webhookAdmitProducer = func(ctx context.Context, adapter PlatformAdapter, event *MessageEvent) (*AdmissionReceipt, error) {
		captured.mu.Lock()
		captured.events = append(captured.events, event)
		n := len(captured.events)
		captured.mu.Unlock()
		return &AdmissionReceipt{AdmissionID: "adm-" + strconv.Itoa(n), Ref: SessionRef{ProfileID: "default"}}, nil
	}
	webhookFinalizeDelivery = func(*WebhookAdapter, context.Context, *MessageEvent, *SessionAuthority, AdmissionReceipt) {}
	webhookAuthorityForProfileID = func(any, string) (*SessionAuthority, error) { return &SessionAuthority{}, nil }
	t.Cleanup(func() {
		adapter.CancelBackgroundTasks(context.Background())
		webhookAdmitProducer, webhookFinalizeDelivery, webhookAuthorityForProfileID = prevAdmit, prevFinalize, prevAuthority
	})
	return captured
}

// Coalesced pull request route with the given coalesce block
func coalesceRoute(coalesce map[string]any) map[string]any {
	if coalesce == nil {
		coalesce = map[string]any{"key": "pull_request.number"}
	}
	return map[string]any{"secret": webhookInsecureNoAuth, "prompt": "PR {pull_request.number}: {action}", "coalesce": coalesce}
}

// Pull request payload, without the pull request block when the number is negative
func coalescePayload(prNumber int, action string) []byte {
	if prNumber < 0 {
		return []byte(`{"action": "` + action + `"}`)
	}
	return []byte(`{"action": "` + action + `", "pull_request": {"number": ` + strconv.Itoa(prNumber) + `}}`)
}

// Posts a payload to the pr route with a delivery id
func coalescePost(t *testing.T, adapter *WebhookAdapter, body []byte, deliveryID string) (int, string) {
	t.Helper()
	headers := map[string]string{}
	if deliveryID != "" {
		headers["X-GitHub-Delivery"] = deliveryID
	}
	resp, err := adapter.handleWebhook(context.Background(), webhookRequest(headers, body, "pr"))
	status, obj := webhookResponseJSON(t, resp, err)
	text, _ := obj.GetOr("status", nil).(string)
	return status, text
}

func makeCoalesceAdapter(t *testing.T, routes map[string]any) *WebhookAdapter {
	return makeWebhookAdapter(t, routes, map[string]any{"host": "127.0.0.1", "rate_limit": 100})
}

func TestInvalidCoalesceConfigRejectedAtConnect(t *testing.T) {
	withBase := func(extra map[string]any) map[string]any {
		route := coalesceRoute(nil)
		for k, v := range extra {
			route[k] = v
		}
		return route
	}
	cases := []struct {
		route map[string]any
		match string
	}{
		{withBase(map[string]any{"coalesce": map[string]any{"window_seconds": 5}}), "coalesce block"},
		{withBase(map[string]any{"coalesce": "pull_request.number"}), "coalesce block"},
		{coalesceRoute(map[string]any{"key": "pull_request.number", "window_seconds": 0}), "window_seconds"},
		{coalesceRoute(map[string]any{"key": "pull_request.number", "max_wait_seconds": true}), "max_wait_seconds"},
		{withBase(map[string]any{"deliver_only": true, "deliver": "telegram"}), "deliver_only"},
		{withBase(map[string]any{"cron_job": "sweeper"}), "cron_job"},
	}
	for _, c := range cases {
		adapter := makeCoalesceAdapter(t, map[string]any{"pr": c.route})
		_, err := adapter.Connect(context.Background(), false)
		var pyErr *PyError
		if err == nil || !asPyError(err, &pyErr) || pyErr.Kind != "ValueError" || !strings.Contains(err.Error(), c.match) {
			t.Fatalf("%s: %v", c.match, err)
		}
		dynamic := makeCoalesceAdapter(t, nil)
		encoded, _ := pyjson.Dumps(map[string]any{"dyn": c.route})
		if err := os.WriteFile(filepath.Join(os.Getenv("ICEBATH_HOME"), "webhook_subscriptions.json"), []byte(encoded), 0o600); err != nil {
			t.Fatal(err)
		}
		dynamic.ReloadDynamicRoutes(context.Background())
		if _, found := dynamic.WebhookRoute("dyn"); found {
			t.Fatalf("%s: dynamic route kept", c.match)
		}
	}
}

func TestRapidSameEntityEventsDispatchOnceWithLatest(t *testing.T) {
	adapter := makeCoalesceAdapter(t, map[string]any{"pr": coalesceRoute(map[string]any{"key": "pull_request.number", "window_seconds": 0.05})})
	admitted := captureWebhookAdmissions(t, adapter)
	for i, action := range []string{"opened", "synchronize", "synchronize"} {
		status, text := coalescePost(t, adapter, coalescePayload(7, action), "d"+strconv.Itoa(i))
		if status != 202 || text != "coalesced" {
			t.Fatalf("event %d: %d %s", i, status, text)
		}
	}
	if _, text := coalescePost(t, adapter, coalescePayload(7, "synchronize"), "d2"); text != "duplicate" {
		t.Fatalf("retry %s", text)
	}
	if _, text := coalescePost(t, adapter, coalescePayload(8, "synchronize"), "other"); text != "coalesced" {
		t.Fatalf("other %s", text)
	}
	if len(admitted.snapshot()) != 0 {
		t.Fatal("admitted before the window")
	}
	time.Sleep(150 * time.Millisecond)
	events := map[string]*MessageEvent{}
	for _, event := range admitted.snapshot() {
		events[*event.MessageID] = event
	}
	if len(events) != 2 || events["d2"] == nil || events["other"] == nil {
		t.Fatalf("events %v", events)
	}
	if !strings.Contains(events["d2"].Text, "PR 7: synchronize") || !strings.Contains(events["d2"].Text, "3 webhook events") {
		t.Fatalf("d2 text %q", events["d2"].Text)
	}
	if strings.Contains(events["other"].Text, "coalesced") {
		t.Fatalf("other text %q", events["other"].Text)
	}
	if len(adapter.Coalescer().Pending()) != 0 {
		t.Fatal("pending left")
	}
}

func TestMaxWaitCapsStarvationAndUnresolvedKeyDispatchesImmediately(t *testing.T) {
	adapter := makeCoalesceAdapter(t, map[string]any{"pr": coalesceRoute(map[string]any{"key": "pull_request.number",
		"window_seconds": 10, "max_wait_seconds": 0.1})})
	admitted := captureWebhookAdmissions(t, adapter)
	if _, text := coalescePost(t, adapter, coalescePayload(-1, "created"), "nokey"); text != "accepted" {
		t.Fatalf("unkeyed %s", text)
	}
	if len(admitted.snapshot()) != 1 {
		t.Fatal("unkeyed not admitted at once")
	}
	for i := 0; i < 3; i++ {
		coalescePost(t, adapter, coalescePayload(5, "synchronize"), "s"+strconv.Itoa(i))
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	got := admitted.snapshot()
	if len(got) != 2 || !strings.Contains(got[1].Text, "3 webhook events") {
		t.Fatalf("admitted %d", len(got))
	}
}

func TestDisconnectFlushesPendingGroups(t *testing.T) {
	adapter := makeCoalesceAdapter(t, map[string]any{"pr": coalesceRoute(map[string]any{"key": "pull_request.number", "window_seconds": 60})})
	admitted := captureWebhookAdmissions(t, adapter)
	coalescePost(t, adapter, coalescePayload(4, "synchronize"), "pend")
	if len(admitted.snapshot()) != 0 {
		t.Fatal("admitted before flush")
	}
	if err := adapter.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := admitted.snapshot()
	if len(got) != 1 || *got[0].MessageID != "pend" || len(adapter.Coalescer().Pending()) != 0 {
		t.Fatalf("admitted %d pending %d", len(got), len(adapter.Coalescer().Pending()))
	}
}

const mcpTestRoute = "mcp-ambush-0123456789ab"

var mcpTestSecret = "whsec_" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))

// Home with one MCP event subscription route, the hermes home fixture
func mcpEventHome(t *testing.T) (*WebhookAdapter, string) {
	adapter := makeWebhookAdapter(t, nil, map[string]any{"host": "127.0.0.1"})
	h := os.Getenv("ICEBATH_HOME")
	route := map[string]any{"secret": mcpTestSecret, "prompt": "Event {eventId}: {data.feed_id}", "deliver": "log", "profile": "default",
		"mcp_event": map[string]any{"server": "ambush", "name": "feed.emission.created", "arguments": map[string]any{}}}
	encoded, _ := pyjson.Dumps(map[string]any{mcpTestRoute: route})
	if err := os.WriteFile(filepath.Join(h, "webhook_subscriptions.json"), []byte(encoded), 0o600); err != nil {
		t.Fatal(err)
	}
	return adapter, h
}

// Standard Webhooks signed POST of a payload to the MCP route
func mcpEventPost(t *testing.T, adapter *WebhookAdapter, payload string, secret string) (int, *pyjson.Object) {
	t.Helper()
	body := []byte(payload)
	msgID, stamp := "msg_"+strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", ""), nowText(0)
	key, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msgID + "." + stamp + "."))
	mac.Write(body)
	headers := map[string]string{"webhook-id": msgID, "webhook-timestamp": stamp,
		"webhook-signature":     "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)),
		"X-MCP-Subscription-Id": "sub_1", "Content-Type": "application/json"}
	resp, err := adapter.handleWebhook(context.Background(), webhookRequest(headers, body, mcpTestRoute))
	return webhookResponseJSON(t, resp, err)
}

func TestVerificationChallengeIsEchoedWithoutARun(t *testing.T) {
	adapter, _ := mcpEventHome(t)
	admitted := captureWebhookAdmissions(t, adapter)
	status, body := mcpEventPost(t, adapter, `{"type": "verification", "challenge": "nonce-123"}`, mcpTestSecret)
	if status != 200 || body.Len() != 1 || body.GetOr("challenge", nil) != "nonce-123" {
		t.Fatalf("%d %v", status, pyjson.Plain(body))
	}
	forged := "whsec_" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	if status, _ := mcpEventPost(t, adapter, `{"type": "verification", "challenge": "nonce-123"}`, forged); status != 401 {
		t.Fatalf("forged %d", status)
	}
	if len(admitted.snapshot()) != 0 {
		t.Fatal("run started")
	}
}

func TestTerminatedDropsTheRoute(t *testing.T) {
	adapter, h := mcpEventHome(t)
	admitted := captureWebhookAdmissions(t, adapter)
	statePath := filepath.Join(h, "mcp_event_subscriptions.json")
	if err := os.WriteFile(statePath, []byte(`{"`+mcpTestRoute+`": {"cursor": "c-1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if status, _ := mcpEventPost(t, adapter, `{"type": "terminated", "error": {"code": -32024, "message": "Forbidden"}}`, mcpTestSecret); status != 200 {
		t.Fatalf("status %d", status)
	}
	for _, path := range []string{filepath.Join(h, "webhook_subscriptions.json"), statePath} {
		raw, _ := os.ReadFile(path)
		if strings.Contains(string(raw), mcpTestRoute) {
			t.Fatalf("%s still names the route: %s", path, raw)
		}
	}
	if len(admitted.snapshot()) != 0 {
		t.Fatal("run started")
	}
}

func TestGapRemembersTheFreshCursor(t *testing.T) {
	adapter, h := mcpEventHome(t)
	admitted := captureWebhookAdmissions(t, adapter)
	if status, _ := mcpEventPost(t, adapter, `{"type": "gap", "cursor": "c-9"}`, mcpTestSecret); status != 200 {
		t.Fatalf("status %d", status)
	}
	raw, _ := os.ReadFile(filepath.Join(h, "mcp_event_subscriptions.json"))
	state, err := pyjson.Loads(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	route, _ := state.(*pyjson.Object).GetOr(mcpTestRoute, pyjson.NewObject()).(*pyjson.Object)
	if route == nil || route.GetOr("cursor", nil) != "c-9" || len(admitted.snapshot()) != 0 {
		t.Fatalf("state %s", raw)
	}
}

func TestAnUnknownControlEnvelopeIsAcknowledgedWithoutARun(t *testing.T) {
	adapter, _ := mcpEventHome(t)
	admitted := captureWebhookAdmissions(t, adapter)
	status, body := mcpEventPost(t, adapter, `{"type": "paused", "reason": "maintenance"}`, mcpTestSecret)
	if status != 200 || body.GetOr("status", nil) != "ignored" {
		t.Fatalf("%d %v", status, pyjson.Plain(body))
	}
	if status, _ := mcpEventPost(t, adapter, `{"type": {"not": "a string"}}`, mcpTestSecret); status != 200 {
		t.Fatalf("odd %d", status)
	}
	if len(admitted.snapshot()) != 0 {
		t.Fatal("run started")
	}
}

// Filter script under the home scripts directory and a fake bash first on PATH
func webhookFilterScript(t *testing.T, body, fakeBash string) string {
	if runtime.GOOS != "linux" {
		t.Skip("POSIX shebang fixture runs on linux")
	}
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	scripts := filepath.Join(h, "scripts")
	if err := os.MkdirAll(scripts, 0o755); err != nil {
		t.Fatal(err)
	}
	filter := filepath.Join(scripts, "filter.sh")
	if err := os.WriteFile(filter, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "bash"), []byte("#!/bin/sh\n"+fakeBash), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return filter
}

func TestShellFilterRunsUnderFindBashInterpreter(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	filter := webhookFilterScript(t, "exit 99\n", "touch \""+marker+"\"\nprintf '{\"ok\": true}'\n")
	accepted, transformed, err := NewWebhookRouteProcessor(WebhookDefaultScriptTimeoutSeconds).RunRouteScript(context.Background(), filter, pyjson.ObjectOf("a", int64(1)))
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatal("fake bash did not run")
	}
	if !accepted || transformed == nil || transformed.Len() != 1 || transformed.GetOr("ok", nil) != true {
		t.Fatalf("%v %v", accepted, pyjson.Plain(transformed))
	}
}

func TestSilentNonzeroExitIsLoggedAsWarning(t *testing.T) {
	filter := webhookFilterScript(t, "", "exit 1\n")
	logs := captureLogs(t)
	accepted, _, err := NewWebhookRouteProcessor(WebhookDefaultScriptTimeoutSeconds).RunRouteScript(context.Background(), filter, pyjson.NewObject())
	if err != nil || accepted {
		t.Fatalf("%v %v", accepted, err)
	}
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "script ignored webhook path=filter.sh") {
			if !strings.Contains(line, "level=WARN") {
				t.Fatalf("line %q", line)
			}
			return
		}
	}
	t.Fatalf("no record in %q", logs.String())
}
