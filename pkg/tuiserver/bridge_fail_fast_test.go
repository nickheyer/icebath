package tuiserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// One recorded bridge request
type bridgeCall struct {
	event, sid string
	payload    map[string]any
	timeout    float64
}

// Fake string request serving canned answers in order and recording each call
type fakeBridge struct {
	answers []string
	calls   []bridgeCall
}

// Installs the fake in place of the server request for the test
func installBridge(t *testing.T) *fakeBridge {
	t.Helper()
	fake := &fakeBridge{}
	saved := askRequest
	askRequest = func(ctx context.Context, method, sid string, params map[string]any, timeout *float64) string {
		answer := ""
		if len(fake.answers) > 0 {
			answer, fake.answers = fake.answers[0], fake.answers[1:]
		}
		call := bridgeCall{event: method, sid: sid, payload: params}
		if timeout != nil {
			call.timeout = *timeout
		}
		fake.calls = append(fake.calls, call)
		return answer
	}
	t.Cleanup(func() {
		askRequest = saved
		previewActionReprobeMu.Lock()
		clear(previewActionReprobe)
		previewActionReprobeMu.Unlock()
	})
	return fake
}

// Registers an empty live session record for the test
func liveRecord(t *testing.T, sid string) *tuigateway.Session {
	t.Helper()
	record := tuigateway.NewSession(nil)
	tuigateway.SetSessionRecord(sid, record)
	t.Cleanup(func() { tuigateway.SetSessionRecord(sid, nil) })
	return record
}

// success field of a JSON bridge result
func bridgeSuccess(t *testing.T, raw string) any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("bad bridge result %q: %v", raw, err)
	}
	return decoded["success"]
}

const bridgeOK = `{"success": true}`

func tourFirstActionIsProbedOnAShortDeadline(t *testing.T) {
	liveRecord(t, "s1")
	bridge := installBridge(t)
	bridge.answers = []string{bridgeOK}
	tourRequest(context.Background(), "s1", map[string]any{"action": "targets"})
	if bridge.calls[0].event != "tour" || bridge.calls[0].timeout != tourProbeTimeoutS || !(tourProbeTimeoutS < tourTimeoutS) {
		t.Fatalf("calls = %+v", bridge.calls)
	}
}

func tourAClientThatAnswersGetsTheFullDeadlineBack(t *testing.T) {
	liveRecord(t, "s1")
	bridge := installBridge(t)
	bridge.answers = []string{bridgeOK, bridgeOK}
	tourRequest(context.Background(), "s1", map[string]any{"action": "targets"})
	tourRequest(context.Background(), "s1", map[string]any{"action": "show", "selector": "#composer"})
	if bridge.calls[1].timeout != tourTimeoutS {
		t.Fatalf("calls = %+v", bridge.calls)
	}
}

func TestLaterActionsShortCircuitInsteadOfStallingAgain(t *testing.T) {
	liveRecord(t, "s1")
	bridge := installBridge(t)
	tourRequest(context.Background(), "s1", map[string]any{"action": "targets"})
	for _, action := range []string{"show", "start", "next", "stop"} {
		if bridgeSuccess(t, tourRequest(context.Background(), "s1", map[string]any{"action": action})) != false {
			t.Fatalf("%s did not fail fast", action)
		}
	}
	if len(bridge.calls) != 1 {
		t.Fatalf("calls = %d", len(bridge.calls))
	}
}

func tourAProvenClientIsNotCondemnedByOneSlowAction(t *testing.T) {
	liveRecord(t, "s1")
	bridge := installBridge(t)
	bridge.answers = []string{bridgeOK}
	tourRequest(context.Background(), "s1", map[string]any{"action": "targets"})
	tourRequest(context.Background(), "s1", map[string]any{"action": "show", "selector": "#composer"})
	bridge.answers = []string{bridgeOK}
	if bridgeSuccess(t, tourRequest(context.Background(), "s1", map[string]any{"action": "next"})) != true || len(bridge.calls) != 3 {
		t.Fatalf("calls = %+v", bridge.calls)
	}
}

func tourANewSessionReprobes(t *testing.T) {
	liveRecord(t, "dead")
	liveRecord(t, "fresh")
	bridge := installBridge(t)
	tourRequest(context.Background(), "dead", map[string]any{"action": "targets"})
	bridge.answers = []string{bridgeOK}
	if bridgeSuccess(t, tourRequest(context.Background(), "fresh", map[string]any{"action": "targets"})) != true {
		t.Fatal("fresh session did not reprobe")
	}
}

func tourASessionWithNoRecordStillBridges(t *testing.T) {
	bridge := installBridge(t)
	bridge.answers = []string{bridgeOK}
	if bridgeSuccess(t, tourRequest(context.Background(), "gone", map[string]any{"action": "targets"})) != true {
		t.Fatal("detached caller lost the bridge")
	}
}

func previewFirstActionIsProbedOnAShortDeadline(t *testing.T) {
	liveRecord(t, "s1")
	bridge := installBridge(t)
	bridge.answers = []string{bridgeOK}
	previewActionRequest(context.Background(), "s1", map[string]any{"action": "elements"})
	if bridge.calls[0].event != "preview.act" || bridge.calls[0].timeout != previewActionProbeTimeoutS || !(previewActionProbeTimeoutS < previewActionTimeoutS) {
		t.Fatalf("calls = %+v", bridge.calls)
	}
}

func previewAClientThatAnswersGetsTheFullDeadlineBack(t *testing.T) {
	liveRecord(t, "s1")
	bridge := installBridge(t)
	bridge.answers = []string{bridgeOK, bridgeOK}
	previewActionRequest(context.Background(), "s1", map[string]any{"action": "elements"})
	previewActionRequest(context.Background(), "s1", map[string]any{"action": "click", "ref": "btn-sign-in"})
	if bridge.calls[1].timeout != previewActionTimeoutS {
		t.Fatalf("calls = %+v", bridge.calls)
	}
}

func TestUnansweredProbeReturnsAnActionableError(t *testing.T) {
	liveRecord(t, "s1")
	installBridge(t)
	var result map[string]any
	_ = json.Unmarshal([]byte(previewActionRequest(context.Background(), "s1", map[string]any{"action": "elements"})), &result)
	if result["success"] != false || !strings.Contains(result["error"].(string), "Update the Icebath Desktop app") {
		t.Fatalf("result = %v", result)
	}
}

func TestRepeatActionsShortCircuitInsteadOfStallingAgain(t *testing.T) {
	liveRecord(t, "s1")
	bridge := installBridge(t)
	previewActionRequest(context.Background(), "s1", map[string]any{"action": "elements"})
	for _, action := range []string{"click", "type", "scroll", "press"} {
		if bridgeSuccess(t, previewActionRequest(context.Background(), "s1", map[string]any{"action": action, "ref": "btn"})) != false {
			t.Fatalf("%s did not fail fast", action)
		}
	}
	if len(bridge.calls) != 1 {
		t.Fatalf("calls = %d", len(bridge.calls))
	}
}

func TestCooldownExpiryReprobesOnce(t *testing.T) {
	liveRecord(t, "s1")
	bridge := installBridge(t)
	saved := previewActionReprobeCooldownS
	previewActionReprobeCooldownS = 0
	t.Cleanup(func() { previewActionReprobeCooldownS = saved })
	previewActionRequest(context.Background(), "s1", map[string]any{"action": "elements"})
	if len(bridge.calls) != 1 {
		t.Fatalf("calls = %d", len(bridge.calls))
	}
	bridge.answers = []string{bridgeOK}
	if bridgeSuccess(t, previewActionRequest(context.Background(), "s1", map[string]any{"action": "elements"})) != true {
		t.Fatal("cooldown expiry did not reprobe")
	}
	if bridge.calls[1].timeout != previewActionProbeTimeoutS {
		t.Fatalf("reprobe timeout = %v", bridge.calls[1].timeout)
	}
	previewActionRequest(context.Background(), "s1", map[string]any{"action": "click", "ref": "btn"})
	if bridge.calls[2].timeout != previewActionTimeoutS {
		t.Fatalf("answered timeout = %v", bridge.calls[2].timeout)
	}
}

func TestWithinCooldownASecondCallerFailsFast(t *testing.T) {
	session := liveRecord(t, "s1")
	bridge := installBridge(t)
	previewActionRequest(context.Background(), "s1", map[string]any{"action": "elements"})
	session.Set("preview_action_bridge_retry_at", 0.0)
	previewActionReprobeMu.Lock()
	previewActionReprobe["s1"] = true
	previewActionReprobeMu.Unlock()
	if len(bridge.calls) != 1 {
		t.Fatalf("calls = %d", len(bridge.calls))
	}
	if bridgeSuccess(t, previewActionRequest(context.Background(), "s1", map[string]any{"action": "scroll"})) != false || len(bridge.calls) != 1 {
		t.Fatalf("calls = %+v", bridge.calls)
	}
}

func previewANewSessionReprobes(t *testing.T) {
	liveRecord(t, "dead")
	liveRecord(t, "fresh")
	bridge := installBridge(t)
	previewActionRequest(context.Background(), "dead", map[string]any{"action": "elements"})
	bridge.answers = []string{bridgeOK}
	if bridgeSuccess(t, previewActionRequest(context.Background(), "fresh", map[string]any{"action": "elements"})) != true {
		t.Fatal("fresh session did not reprobe")
	}
}

func previewASessionWithNoRecordStillBridges(t *testing.T) {
	bridge := installBridge(t)
	bridge.answers = []string{bridgeOK}
	if bridgeSuccess(t, previewActionRequest(context.Background(), "gone", map[string]any{"action": "elements"})) != true {
		t.Fatal("detached caller lost the bridge")
	}
}

func TestAWindowDeclineAnswerPassesThrough(t *testing.T) {
	liveRecord(t, "s1")
	bridge := installBridge(t)
	decline := `{"success": false, "error": "No Icebath Desktop window is showing this chat."}`
	bridge.answers = []string{decline}
	if got := previewActionRequest(context.Background(), "s1", map[string]any{"action": "elements"}); got != decline {
		t.Fatalf("got %q", got)
	}
}

func previewAProvenClientIsNotCondemnedByOneSlowAction(t *testing.T) {
	session := liveRecord(t, "s1")
	bridge := installBridge(t)
	bridge.answers = []string{bridgeOK}
	previewActionRequest(context.Background(), "s1", map[string]any{"action": "elements"})
	if bridgeSuccess(t, previewActionRequest(context.Background(), "s1", map[string]any{"action": "click", "ref": "btn"})) != false {
		t.Fatal("slow action reported success")
	}
	if session.Get("preview_action_bridge") != "answered" {
		t.Fatalf("bridge = %v", session.Get("preview_action_bridge"))
	}
	previewActionRequest(context.Background(), "s1", map[string]any{"action": "scroll"})
	if bridge.calls[2].timeout != previewActionTimeoutS {
		t.Fatalf("timeout = %v", bridge.calls[2].timeout)
	}
}

func TestFirstActionIsProbedOnAShortDeadline(t *testing.T) {
	t.Run("tour", tourFirstActionIsProbedOnAShortDeadline)
	t.Run("preview_action", previewFirstActionIsProbedOnAShortDeadline)
}

func TestAClientThatAnswersGetsTheFullDeadlineBack(t *testing.T) {
	t.Run("tour", tourAClientThatAnswersGetsTheFullDeadlineBack)
	t.Run("preview_action", previewAClientThatAnswersGetsTheFullDeadlineBack)
}

func TestANewSessionReprobes(t *testing.T) {
	t.Run("tour", tourANewSessionReprobes)
	t.Run("preview_action", previewANewSessionReprobes)
}

func TestASessionWithNoRecordStillBridges(t *testing.T) {
	t.Run("tour", tourASessionWithNoRecordStillBridges)
	t.Run("preview_action", previewASessionWithNoRecordStillBridges)
}

func TestAProvenClientIsNotCondemnedByOneSlowAction(t *testing.T) {
	t.Run("tour", tourAProvenClientIsNotCondemnedByOneSlowAction)
	t.Run("preview_action", previewAProvenClientIsNotCondemnedByOneSlowAction)
}
