package gateway

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Webhook adapter with the hermes test defaults, extra keys overriding them
func makeWebhookAdapter(t *testing.T, routes map[string]any, overrides map[string]any) *WebhookAdapter {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if routes == nil {
		routes = map[string]any{}
	}
	extra := map[string]any{"host": "0.0.0.0", "port": 0, "routes": routes, "rate_limit": 30, "max_body_bytes": 1_048_576}
	for k, v := range overrides {
		extra[k] = v
	}
	config := NewPlatformConfig()
	config.Enabled = true
	config.Extra = extra
	adapter, err := NewWebhookAdapter(config)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

// Request with the given headers and body routed to a webhook route name
func webhookRequest(headers map[string]string, body []byte, route string) *WebRequest {
	r := httptest.NewRequest("POST", "/webhooks/"+route, bytes.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	request := NewWebRequest(r, 0)
	request.MatchInfo["route_name"] = route
	return request
}

// Hex HMAC SHA256 of the body, the generic and Linear signature
func webhookTestHex(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// GitHub X-Hub-Signature-256 of the body
func webhookGitHubSignature(body []byte, secret string) string {
	return "sha256=" + webhookTestHex(body, secret)
}

// Svix v1 signature header of the body
func webhookSvixSignature(t *testing.T, body []byte, secret, msgID, timestamp string) string {
	key := []byte(secret)
	if strings.HasPrefix(secret, "whsec_") {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
		if err != nil {
			t.Fatal(err)
		}
		key = decoded
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msgID + "." + timestamp + "."))
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func nowText(offset int64) string { return strconv.FormatInt(time.Now().Unix()+offset, 10) }

func TestValidateNoSignatureWithSecretRejects(t *testing.T) {
	adapter := makeWebhookAdapter(t, nil, nil)
	if adapter.ValidateSignature(webhookRequest(nil, nil, ""), []byte("{}"), "my-secret") {
		t.Fatal("accepted")
	}
}

func TestNonASCIISignatureHeadersRejectWithoutRaising(t *testing.T) {
	adapter := makeWebhookAdapter(t, nil, nil)
	for _, header := range []string{"X-Hub-Signature-256", "X-Gitlab-Token", "X-Webhook-Signature", "linear-signature"} {
		req := webhookRequest(map[string]string{header: "ské-not-a-valid-signature"}, nil, "")
		if adapter.ValidateSignature(req, []byte(`{"action": "opened"}`), "webhook-secret-42") {
			t.Fatalf("%s accepted", header)
		}
	}
}

func TestLinearSignatureValidAccepts(t *testing.T) {
	adapter := makeWebhookAdapter(t, nil, nil)
	body := []byte(`{"type": "Issue", "data": {"id": "abc"}}`)
	req := webhookRequest(map[string]string{"linear-signature": webhookTestHex(body, "linear-webhook-key")}, nil, "")
	if !adapter.ValidateSignature(req, body, "linear-webhook-key") {
		t.Fatal("rejected")
	}
}

func TestLinearSignatureMismatchRejects(t *testing.T) {
	adapter := makeWebhookAdapter(t, nil, nil)
	body := []byte(`{"type": "Issue"}`)
	req := webhookRequest(map[string]string{"linear-signature": webhookTestHex(body, "attacker-controlled-key")}, nil, "")
	if adapter.ValidateSignature(req, body, "real-secret") {
		t.Fatal("accepted")
	}
}

func TestNonASCIISvixSignatureRejected(t *testing.T) {
	adapter := makeWebhookAdapter(t, nil, nil)
	req := webhookRequest(map[string]string{"svix-id": "msg_2xabc", "svix-timestamp": nowText(0),
		"svix-signature": "v1,ské-not-a-valid-base64-sig"}, nil, "")
	if adapter.ValidateSignature(req, []byte(`{"x":1}`), "shh-secret") {
		t.Fatal("accepted")
	}
}

func TestNonASCIISecretStillValidatesAMatchingToken(t *testing.T) {
	adapter := makeWebhookAdapter(t, nil, nil)
	secret := "gl-tökén-välue"
	if !adapter.ValidateSignature(webhookRequest(map[string]string{"X-Gitlab-Token": secret}, nil, ""), []byte("{}"), secret) {
		t.Fatal("rejected")
	}
}

func TestValidateGenericV2WrongTimestampRejects(t *testing.T) {
	adapter := makeWebhookAdapter(t, nil, nil)
	body := []byte(`{"event": "push"}`)
	real := nowText(0)
	sig := webhookTestHex(append([]byte(real+"."), body...), "generic-secret")
	req := webhookRequest(map[string]string{"X-Webhook-Signature-V2": sig, "X-Webhook-Timestamp": nowText(1)}, nil, "")
	if adapter.ValidateSignature(req, body, "generic-secret") {
		t.Fatal("accepted")
	}
}

func TestValidateGenericV2StrippedTimestampDoesNotDowngradeToV1(t *testing.T) {
	adapter := makeWebhookAdapter(t, nil, nil)
	body := []byte(`{"event": "push"}`)
	v2 := webhookTestHex(append([]byte(nowText(0)+"."), body...), "generic-secret")
	req := webhookRequest(map[string]string{"X-Webhook-Signature-V2": v2, "X-Webhook-Signature": webhookTestHex(body, "generic-secret")}, nil, "")
	if adapter.ValidateSignature(req, body, "generic-secret") {
		t.Fatal("accepted")
	}
}

func TestValidateGenericV1SignatureAccepts(t *testing.T) {
	adapter := makeWebhookAdapter(t, nil, nil)
	body := []byte(`{"event": "push"}`)
	req := webhookRequest(map[string]string{"X-Webhook-Signature": webhookTestHex(body, "generic-secret")}, nil, "")
	if !adapter.ValidateSignature(req, body, "generic-secret") {
		t.Fatal("rejected")
	}
}

func TestValidateSvixSignatureRawSecretValid(t *testing.T) {
	adapter := makeWebhookAdapter(t, nil, nil)
	body := []byte(`{"event_type":"message.received"}`)
	stamp := nowText(0)
	sig := webhookSvixSignature(t, body, "raw-agentmail-secret", "msg_123", stamp)
	req := webhookRequest(map[string]string{"svix-id": "msg_123", "svix-timestamp": stamp, "svix-signature": sig}, nil, "")
	if !adapter.ValidateSignature(req, body, "raw-agentmail-secret") {
		t.Fatal("rejected")
	}
}

func TestStandardWebhooksHeadersValidateLikeSvix(t *testing.T) {
	cases := []struct {
		secret, signWith string
		body, received   string
		stale, expected  bool
	}{
		{"whsec_" + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")), "", `{"a":1}`, `{"a":1}`, false, true},
		{"raw-signing-secret", "", `{"a":1}`, `{"a":1}`, false, true},
		{"real-secret", "attacker-secret", `{"a":1}`, `{"a":1}`, false, false},
		{"real-secret", "", `{"a":1}`, `{"a":2}`, false, false},
		{"real-secret", "", `{"a":1}`, `{"a":1}`, true, false},
		{"whsec_", "", `{"a":1}`, `{"a":1}`, false, false},
		{"whsec_ICAg", "", `{"a":1}`, `{"a":1}`, false, false},
		{"whsec_AA==", "", `{"a":1}`, `{"a":1}`, false, false},
	}
	for i, c := range cases {
		adapter := makeWebhookAdapter(t, nil, nil)
		offset := int64(0)
		if c.stale {
			offset = -600
		}
		stamp := nowText(offset)
		signWith := c.signWith
		if signWith == "" {
			signWith = c.secret
		}
		sig := webhookSvixSignature(t, []byte(c.body), signWith, "msg_std", stamp)
		req := webhookRequest(map[string]string{"webhook-id": "msg_std", "webhook-timestamp": stamp, "webhook-signature": sig}, nil, "")
		if got := adapter.ValidateSignature(req, []byte(c.received), c.secret); got != c.expected {
			t.Fatalf("case %d: got %v", i, got)
		}
	}
}

func TestGitlabSecretTokenSurvivesUnsignedStandardWebhooksMetadata(t *testing.T) {
	adapter := makeWebhookAdapter(t, nil, nil)
	req := webhookRequest(map[string]string{"X-Gitlab-Token": "legacy-token", "webhook-id": "gl_1", "webhook-timestamp": nowText(0)}, nil, "")
	if !adapter.ValidateSignature(req, []byte("{}"), "legacy-token") {
		t.Fatal("legacy token rejected")
	}
	partial := webhookRequest(map[string]string{"webhook-id": "gl_2", "webhook-timestamp": nowText(0), "webhook-signature": "v1,AAAA"}, nil, "")
	if adapter.ValidateSignature(partial, []byte("{}"), "legacy-token") {
		t.Fatal("partial accepted")
	}
}

func TestRenderPromptDotNotation(t *testing.T) {
	adapter := makeWebhookAdapter(t, nil, nil)
	payload := pyjson.ObjectOf("pull_request", pyjson.ObjectOf("title", "Fix bug", "number", int64(42)))
	got, err := adapter.RenderPrompt("PR #{pull_request.number}: {pull_request.title}", payload, "pull_request", "github")
	if err != nil || got != "PR #42: Fix bug" {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestRenderDeliveryExtraTemplates(t *testing.T) {
	adapter := makeWebhookAdapter(t, nil, nil)
	extra := pyjson.ObjectOf("repo", "{repository.full_name}", "pr_number", "{number}", "static", int64(42))
	payload := pyjson.ObjectOf("repository", pyjson.ObjectOf("full_name", "org/repo"), "number", int64(7))
	got, err := adapter.renderDeliveryExtra(extra, payload)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetOr("repo", nil) != "org/repo" || got.GetOr("pr_number", nil) != "7" || !pyrepr.Equal(got.GetOr("static", nil), 42) {
		t.Fatalf("got %v", pyjson.Plain(got))
	}
}

// Status and JSON body of a handler response
func webhookResponseJSON(t *testing.T, resp *WebResponse, err error) (int, *pyjson.Object) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	decoded, decodeErr := pyjson.Loads(resp.Text())
	if decodeErr != nil {
		t.Fatalf("body %q: %v", resp.Text(), decodeErr)
	}
	obj, _ := decoded.(*pyjson.Object)
	return resp.Status, obj
}

func TestUnknownRouteReturns404(t *testing.T) {
	adapter := makeWebhookAdapter(t, map[string]any{"real": map[string]any{"secret": webhookInsecureNoAuth, "prompt": "x"}}, nil)
	resp, err := adapter.handleWebhook(context.Background(), webhookRequest(nil, []byte(`{"a": 1}`), "nonexistent"))
	if status, _ := webhookResponseJSON(t, resp, err); status != 404 {
		t.Fatalf("status %d", status)
	}
}

func TestRouteWithoutSecretRejectsUnsignedRequest(t *testing.T) {
	adapter := makeWebhookAdapter(t, map[string]any{"test": map[string]any{"prompt": "hi"},
		"blank": map[string]any{"prompt": "hi", "secret": "   "}}, map[string]any{"secret": ""})
	resp, err := adapter.handleWebhook(context.Background(), webhookRequest(nil, []byte(`{"data": "value"}`), "test"))
	if status, _ := webhookResponseJSON(t, resp, err); status != 403 {
		t.Fatalf("status %d", status)
	}
	body := []byte(`{"data": "value"}`)
	resp, err = adapter.handleWebhook(context.Background(), webhookRequest(map[string]string{"Content-Type": "application/json",
		"X-Hub-Signature-256": webhookGitHubSignature(body, "   ")}, body, "blank"))
	if status, _ := webhookResponseJSON(t, resp, err); status != 403 {
		t.Fatalf("status %d", status)
	}
}

func TestOversizedPayloadRejected(t *testing.T) {
	adapter := makeWebhookAdapter(t, map[string]any{"big": map[string]any{"secret": webhookInsecureNoAuth, "prompt": "test"}},
		map[string]any{"max_body_bytes": 100})
	body := []byte(`{"data": "` + strings.Repeat("x", 200) + `"}`)
	req := webhookRequest(nil, body, "big")
	length := int64(999999)
	req.ContentLength = &length
	resp, err := adapter.handleWebhook(context.Background(), req)
	if status, _ := webhookResponseJSON(t, resp, err); status != 413 {
		t.Fatalf("status %d", status)
	}
}

// Platform adapter recording every send, the AsyncMock target of the hermes delivery cases
type webhookTargetAdapter struct {
	*BasePlatformAdapter
	mu     sync.Mutex
	calls  []webhookTargetCall
	result *SendResult
}

type webhookTargetCall struct {
	chatID, content string
	metadata        map[string]any
}

func (a *webhookTargetAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *webhookTargetAdapter) Disconnect(context.Context) error            { return nil }
func (a *webhookTargetAdapter) GetChatInfo(context.Context, string) (map[string]any, error) {
	return map[string]any{}, nil
}
func (a *webhookTargetAdapter) Send(_ context.Context, chatID, content, _ string, metadata map[string]any) (SendResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, webhookTargetCall{chatID, content, metadata})
	if a.result != nil {
		return *a.result, nil
	}
	return SendResult{Success: true}, nil
}

func newWebhookTarget(platform Platform) *webhookTargetAdapter {
	a := &webhookTargetAdapter{}
	cfg := NewPlatformConfig()
	cfg.Enabled = true
	a.BasePlatformAdapter = NewBasePlatformAdapter(cfg, platform, a)
	return a
}

// Runner exposing one adapter map for every profile and a config without home channels, the MagicMock runner
type webhookMockRunner struct {
	adapters *pydict.Dict[Platform, PlatformAdapter]
	config   *GatewayConfig
}

func (r *webhookMockRunner) AdaptersForProfile(*string) *pydict.Dict[Platform, PlatformAdapter] {
	return r.adapters
}
func (r *webhookMockRunner) GatewayConfig() *GatewayConfig { return r.config }

// Adapter with a telegram target and one retained telegram delivery
func webhookAdapterWithMockTarget(t *testing.T) (*WebhookAdapter, *webhookTargetAdapter, string) {
	adapter := makeWebhookAdapter(t, nil, nil)
	target := newWebhookTarget(PlatformTelegram)
	adapters := pydict.New[Platform, PlatformAdapter]()
	adapters.Set(PlatformTelegram, target)
	adapter.GatewayRunner = &webhookMockRunner{adapters: adapters, config: &GatewayConfig{}}
	chatID := "webhook:helper-events:d-1"
	adapter.deliveryInfo[chatID] = pyjson.ObjectOf("deliver", "telegram", "deliver_extra", pyjson.ObjectOf("chat_id", "-100123"))
	adapter.deliveryInfoCreated[chatID] = webhookNow()
	return adapter, target, chatID
}

func TestBareMarkerIsNotDelivered(t *testing.T) {
	adapter, target, chatID := webhookAdapterWithMockTarget(t)
	result, err := adapter.Send(context.Background(), chatID, "[SILENT]", "", nil)
	if err != nil || !result.Success || len(target.calls) != 0 {
		t.Fatalf("result %+v %v calls %d", result, err, len(target.calls))
	}
}

func TestMarkerFollowedByProseIsNotDelivered(t *testing.T) {
	adapter, target, chatID := webhookAdapterWithMockTarget(t)
	result, err := adapter.Send(context.Background(), chatID, "[SILENT]\n\nThe new inbound was the same email quoted back a second "+
		"time, on a ticket we already answered. Nothing new to reply to, so I "+
		"closed it; it reopens by itself if they write back.", "", nil)
	if err != nil || !result.Success || len(target.calls) != 0 {
		t.Fatalf("result %+v %v calls %d", result, err, len(target.calls))
	}
}

func TestDeliveryInfoSurvivesMultipleSends(t *testing.T) {
	adapter := makeWebhookAdapter(t, nil, nil)
	chatID := "webhook:test:d-xyz"
	adapter.deliveryInfo[chatID] = pyjson.ObjectOf("deliver", "log", "deliver_extra", pyjson.NewObject())
	adapter.deliveryInfoCreated[chatID] = webhookNow()
	for _, text := range []string{"Status: switching to fallback", "Final agent response"} {
		result, err := adapter.Send(context.Background(), chatID, text, "", nil)
		if err != nil || !result.Success {
			t.Fatalf("send %q: %+v %v", text, result, err)
		}
		if adapter.DeliveryInfo(chatID) == nil {
			t.Fatal("delivery info dropped")
		}
	}
}

func TestRawMixedWithOtherVariables(t *testing.T) {
	adapter := makeWebhookAdapter(t, nil, nil)
	got, err := adapter.RenderPrompt("Action={action} Raw={__raw__}", pyjson.ObjectOf("action", "closed", "number", int64(7)), "push", "test")
	if err != nil || !strings.HasPrefix(got, "Action=closed Raw=") || !strings.Contains(got, `"action": "closed"`) || !strings.Contains(got, `"number": 7`) {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestThreadIDPassedAsMetadata(t *testing.T) {
	adapter, target, _ := webhookAdapterWithMockTarget(t)
	delivery := pyjson.ObjectOf("deliver_extra", pyjson.ObjectOf("chat_id", "12345", "thread_id", "999"))
	if _, err := adapter.deliverCrossPlatform(context.Background(), "telegram", "hello", delivery); err != nil {
		t.Fatal(err)
	}
	if len(target.calls) != 1 || target.calls[0].chatID != "12345" || target.calls[0].content != "hello" ||
		target.calls[0].metadata["thread_id"] != "999" || len(target.calls[0].metadata) != 1 {
		t.Fatalf("calls %+v", target.calls)
	}
}

func TestConnectAllowsInsecureNoAuthOnLoopback(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "localhost"} {
		adapter := makeWebhookAdapter(t, map[string]any{"r1": map[string]any{"secret": webhookInsecureNoAuth, "prompt": "x"}},
			map[string]any{"host": host, "port": 0})
		ok, err := adapter.Connect(context.Background(), false)
		adapter.Disconnect(context.Background())
		if err != nil || !ok {
			t.Fatalf("%s: %v %v", host, ok, err)
		}
	}
}

func TestConnectAllowsRealSecretOnPublicBind(t *testing.T) {
	adapter := makeWebhookAdapter(t, map[string]any{"r1": map[string]any{"secret": "real-secret-abc123", "prompt": "x"}},
		map[string]any{"host": "0.0.0.0", "port": 0})
	ok, err := adapter.Connect(context.Background(), false)
	adapter.Disconnect(context.Background())
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
}

func TestDefaultBindServesBothFamilies(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cfg := NewPlatformConfig()
	cfg.Enabled = true
	cfg.Extra = map[string]any{"port": 0, "routes": map[string]any{"r1": map[string]any{"secret": "real-secret-abc123", "prompt": "x"}}}
	adapter, err := NewWebhookAdapter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if adapter.ListenHost() != nil {
		t.Fatal("host not nil")
	}
	defer adapter.Disconnect(context.Background())
	if ok, err := adapter.Connect(context.Background(), false); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	var v4, v6 bool
	for _, addr := range adapter.Runner().Addrs() {
		ip := addr.(*net.TCPAddr).IP
		if ip.To4() != nil {
			v4 = true
		} else {
			v6 = true
		}
	}
	if !v4 || !v6 {
		t.Fatalf("v4 %v v6 %v addrs %v", v4, v6, adapter.Runner().Addrs())
	}
}

func TestExplicitHostStillRejectsLiveListener(t *testing.T) {
	blocker, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	port := blocker.Addr().(*net.TCPAddr).Port
	adapter := makeWebhookAdapter(t, map[string]any{"r1": map[string]any{"secret": "real-secret-abc123", "prompt": "x"}},
		map[string]any{"host": "127.0.0.1", "port": port})
	defer adapter.Disconnect(context.Background())
	ok, err := adapter.Connect(context.Background(), false)
	if err != nil || ok {
		t.Fatalf("connect %v %v", ok, err)
	}
	if adapter.Runner() != nil || adapter.IsConnected() {
		t.Fatal("runner kept or connected")
	}
}

func TestRouteProfileValidationFailsClosed(t *testing.T) {
	worker, other := "worker", "other"
	if !webhookRouteAllowsProfile(pyjson.NewObject(), nil) {
		t.Fatal("default route refused")
	}
	if !webhookRouteAllowsProfile(pyjson.ObjectOf("profile", "worker"), &worker) {
		t.Fatal("worker route refused")
	}
	if webhookRouteAllowsProfile(pyjson.ObjectOf("profile", "worker"), &other) {
		t.Fatal("worker route served other")
	}
	for _, malformed := range []any{nil, "", "   ", int64(123), []any{"worker"}} {
		if webhookRouteAllowsProfile(pyjson.ObjectOf("profile", malformed), &worker) {
			t.Fatalf("malformed %v accepted", malformed)
		}
	}
}

func TestConnectRejectsAnUnusableRouteSecret(t *testing.T) {
	for _, secret := range []any{"   ", "\t", "\n", int64(123)} {
		adapter := makeWebhookAdapter(t, map[string]any{"hook": map[string]any{"secret": secret}}, nil)
		_, err := adapter.Connect(context.Background(), false)
		if err == nil || !strings.Contains(err.Error(), "missing, blank, or not a string") {
			t.Fatalf("%v: %v", secret, err)
		}
		var pyErr *PyError
		if !asPyError(err, &pyErr) || pyErr.Kind != "ValueError" {
			t.Fatalf("%v: %T", secret, err)
		}
		if adapter.dynamicRouteAllowed("hook", pyjson.ObjectOf("secret", secret)) {
			t.Fatalf("%v dynamic allowed", secret)
		}
	}
}

// Whether err is a gateway PyError, assigning it
func asPyError(err error, target **PyError) bool {
	pyErr, ok := err.(*PyError)
	*target = pyErr
	return ok
}

func TestIsUsableSecretClassifiesConfiguredSecrets(t *testing.T) {
	for _, c := range []struct {
		value  any
		usable bool
	}{{"s3cret", true}, {"x", true}, {" x ", true}, {"", false}, {"   ", false}, {"\t\n", false},
		{nil, false}, {[]byte("bytes"), false}, {int64(123), false}} {
		if got := WebhookIsUsableSecret(c.value); got != c.usable {
			t.Fatalf("%v: %v", c.value, got)
		}
	}
}
