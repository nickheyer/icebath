package gateway

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Wires a mock runner whose telegram adapter records sends, the hermes _wire_mock_target
func wireWebhookMockTarget(adapter *WebhookAdapter) *webhookTargetAdapter {
	target := newWebhookTarget(PlatformTelegram)
	adapters := pydict.New[Platform, PlatformAdapter]()
	adapters.Set(PlatformTelegram, target)
	adapter.GatewayRunner = &webhookMockRunner{adapters: adapters, config: &GatewayConfig{}}
	return target
}

// Loopback webhook adapter with the given routes, the deliver only and cron trigger test helper
func makeLoopbackWebhookAdapter(t *testing.T, routes map[string]any) *WebhookAdapter {
	return makeWebhookAdapter(t, routes, map[string]any{"host": "127.0.0.1"})
}

// Waits until every background task of the adapter finished
func drainWebhookBackgroundTasks(t *testing.T, adapter *WebhookAdapter) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		adapter.mu.Lock()
		pending := 0
		for task := range adapter.backgroundTasks {
			if !task.finished() {
				pending++
			}
		}
		adapter.mu.Unlock()
		if pending == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("background tasks still running")
}

func TestPostDeliversDirectlyWithoutAgent(t *testing.T) {
	adapter := makeLoopbackWebhookAdapter(t, map[string]any{"match-alert": map[string]any{"secret": webhookInsecureNoAuth,
		"deliver": "telegram", "deliver_only": true, "deliver_extra": map[string]any{"chat_id": "12345"},
		"prompt": "{payload.user} matched with {payload.other}!"}})
	target := wireWebhookMockTarget(adapter)
	admitted := captureWebhookAdmissions(t, adapter)
	resp, err := adapter.handleWebhook(context.Background(), webhookRequest(map[string]string{"Content-Type": "application/json",
		"X-GitHub-Delivery": "delivery-1"}, []byte(`{"payload": {"user": "alice", "other": "bob"}}`), "match-alert"))
	status, body := webhookResponseJSON(t, resp, err)
	if status != 200 || body.GetOr("status", nil) != "delivered" || body.GetOr("route", nil) != "match-alert" || body.GetOr("target", nil) != "telegram" {
		t.Fatalf("%d %v", status, pyjson.Plain(body))
	}
	if len(admitted.snapshot()) != 0 {
		t.Fatal("agent run admitted")
	}
	if len(target.calls) != 1 || target.calls[0].chatID != "12345" || target.calls[0].content != "alice matched with bob!" {
		t.Fatalf("calls %+v", target.calls)
	}
}

func TestDeliveryFailureReturns502(t *testing.T) {
	adapter := makeLoopbackWebhookAdapter(t, map[string]any{"r": map[string]any{"secret": webhookInsecureNoAuth,
		"deliver": "telegram", "deliver_only": true, "deliver_extra": map[string]any{"chat_id": "c-1"}, "prompt": "hi"}})
	target := wireWebhookMockTarget(adapter)
	target.result = &SendResult{Success: false, Error: "rate limited by tg"}
	resp, err := adapter.handleWebhook(context.Background(), webhookRequest(map[string]string{"X-GitHub-Delivery": "d-fail-1"}, []byte(`{}`), "r"))
	status, _ := webhookResponseJSON(t, resp, err)
	if status != 502 || strings.Contains(resp.Text(), "rate limited") {
		t.Fatalf("%d %s", status, resp.Text())
	}
}

func TestHmacStillEnforced(t *testing.T) {
	adapter := makeLoopbackWebhookAdapter(t, map[string]any{"r": map[string]any{"secret": "real-secret-123",
		"deliver": "telegram", "deliver_only": true, "deliver_extra": map[string]any{"chat_id": "c-1"}, "prompt": "hi"}})
	target := wireWebhookMockTarget(adapter)
	resp, err := adapter.handleWebhook(context.Background(), webhookRequest(map[string]string{"X-GitHub-Delivery": "d-noauth-1"}, []byte(`{}`), "r"))
	if status, _ := webhookResponseJSON(t, resp, err); status != 401 || len(target.calls) != 0 {
		t.Fatalf("%d calls %d", status, len(target.calls))
	}
}

// Replaces the cron event runner for one test, recording each fired job
func fakeCronExecute(t *testing.T, result map[string]any) *[][2]string {
	var mu sync.Mutex
	fired := &[][2]string{}
	prev := WebhookExecuteJobForEvent
	WebhookExecuteJobForEvent = func(ctx context.Context, h, jobRef string, extraPrompt *string) (map[string]any, error) {
		mu.Lock()
		defer mu.Unlock()
		*fired = append(*fired, [2]string{jobRef, *extraPrompt})
		return result, nil
	}
	t.Cleanup(func() { WebhookExecuteJobForEvent = prev })
	return fired
}

func TestPostFiresJobWithEventContext(t *testing.T) {
	adapter := makeLoopbackWebhookAdapter(t, map[string]any{"pr-feedback": map[string]any{"secret": webhookInsecureNoAuth,
		"cron_job": "review-sweeper", "prompt": "PR #{number} received feedback: {review.body}"}})
	admitted := captureWebhookAdmissions(t, adapter)
	fired := fakeCronExecute(t, map[string]any{"claimed": true, "success": true, "error": nil})
	resp, err := adapter.handleWebhook(context.Background(), webhookRequest(map[string]string{"Content-Type": "application/json",
		"X-GitHub-Delivery": "delivery-cron-1", "X-GitHub-Event": "pull_request_review"},
		[]byte(`{"number": 7, "review": {"body": "needs tests"}}`), "pr-feedback"))
	status, body := webhookResponseJSON(t, resp, err)
	if status != 202 || body.GetOr("status", nil) != "accepted" || body.GetOr("cron_job", nil) != "review-sweeper" {
		t.Fatalf("%d %v", status, pyjson.Plain(body))
	}
	drainWebhookBackgroundTasks(t, adapter)
	if len(*fired) != 1 || (*fired)[0][0] != "review-sweeper" || !strings.Contains((*fired)[0][1], "PR #7 received feedback: needs tests") ||
		!strings.Contains((*fired)[0][1], "pull_request_review") {
		t.Fatalf("fired %v", *fired)
	}
	if len(admitted.snapshot()) != 0 {
		t.Fatal("agent run admitted")
	}
}

func TestJobFailureDoesNotBreakHTTPResponse(t *testing.T) {
	adapter := makeLoopbackWebhookAdapter(t, map[string]any{"flaky": map[string]any{"secret": webhookInsecureNoAuth, "cron_job": "gone-job"}})
	fakeCronExecute(t, map[string]any{"claimed": false, "success": false, "error": "Cron job 'gone-job' not found."})
	resp, err := adapter.handleWebhook(context.Background(), webhookRequest(map[string]string{"Content-Type": "application/json",
		"X-GitHub-Delivery": "delivery-cron-2"}, []byte(`{}`), "flaky"))
	if status, _ := webhookResponseJSON(t, resp, err); status != 202 {
		t.Fatalf("status %d", status)
	}
	drainWebhookBackgroundTasks(t, adapter)
}

func TestCronJobPlusDeliverOnlyRejectedAtConnect(t *testing.T) {
	adapter := makeLoopbackWebhookAdapter(t, map[string]any{"bad": map[string]any{"secret": "s3cret", "cron_job": "some-job",
		"deliver_only": true, "deliver": "telegram"}})
	_, err := adapter.Connect(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err %v", err)
	}
}
