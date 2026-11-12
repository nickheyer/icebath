package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Provider rows the hermes picker cases seed
func pickerProviders() []map[string]any {
	return []map[string]any{
		{"slug": "openrouter", "name": "OpenRouter", "models": []any{"anthropic/claude-sonnet-4", "openai/gpt-5"}, "total_models": int64(2), "is_current": true},
		{"slug": "anthropic", "name": "Anthropic", "models": []any{"claude-sonnet-4-20250514"}, "total_models": int64(1), "is_current": false},
	}
}

func pickerInteractionBody(msgTS, user, uid string) map[string]any {
	return map[string]any{"message": map[string]any{"ts": msgTS, "blocks": []any{}}, "channel": map[string]any{"id": "C1"},
		"user": map[string]any{"name": user, "id": uid}, "team_id": "T1"}
}

// Model switch callback double recording its calls
type pickerCallback struct {
	calls  [][3]string
	result string
	err    error
}

func (c *pickerCallback) fn(_ context.Context, chatID, modelID, providerSlug string) (string, error) {
	c.calls = append(c.calls, [3]string{chatID, modelID, providerSlug})
	return c.result, c.err
}

func pickerSend(t *testing.T, a *SlackAdapter, providers []map[string]any, metadata map[string]any) gateway.SendResult {
	t.Helper()
	cb := &pickerCallback{result: "Switched!"}
	result, err := a.SendModelPicker(context.Background(), "C1", providers, "anthropic/claude-sonnet-4", "openrouter", "test-session", cb.fn, metadata)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSlackModelPickerSendSendsProviderDropdown(t *testing.T) {
	a, f := approvalAdapter(t)
	f.respond("chat.postMessage", map[string]any{"ok": true, "ts": "1234.5678"})
	result := pickerSend(t, a, pickerProviders(), nil)
	if !result.Success || result.MessageID != "1234.5678" {
		t.Fatalf("result %+v", result)
	}
	blocks := blocksOf(t, f.callsTo("chat.postMessage")[0].Args)
	if len(blocks) != 2 || blocks[0].(map[string]any)["type"] != "section" ||
		!strings.Contains(blocks[0].(map[string]any)["text"].(map[string]any)["text"].(string), "anthropic/claude-sonnet-4") {
		t.Fatalf("blocks %v", blocks)
	}
	elements := blocks[1].(map[string]any)["elements"].([]any)
	sel, cancel := elements[0].(map[string]any), elements[1].(map[string]any)
	options := sel["options"].([]any)
	if len(elements) != 2 || sel["type"] != "static_select" || sel["action_id"] != "hermes_model_provider" || len(options) != 2 ||
		options[0].(map[string]any)["value"] != "0" || options[1].(map[string]any)["value"] != "1" ||
		!strings.Contains(options[0].(map[string]any)["text"].(map[string]any)["text"].(string), "OpenRouter") ||
		cancel["type"] != "button" || cancel["action_id"] != "hermes_model_cancel" {
		t.Fatalf("elements %v", elements)
	}
	state, ok := a.modelPickerState.Get("1234.5678")
	if !ok || state.stage != "provider" || state.sessionKey != "test-session" {
		t.Fatalf("state %+v", state)
	}
}

func TestSlackModelPickerSendSendsInThread(t *testing.T) {
	a, f := approvalAdapter(t)
	f.respond("chat.postMessage", map[string]any{"ok": true, "ts": "1.2"})
	pickerSend(t, a, pickerProviders(), map[string]any{"thread_id": "9999.0000"})
	if got := f.callsTo("chat.postMessage")[0].Args["thread_ts"]; got != "9999.0000" {
		t.Fatalf("thread_ts %v", got)
	}
}

func TestSlackModelPickerSendNotConnected(t *testing.T) {
	a, _ := approvalAdapter(t)
	a.app = nil
	if pickerSend(t, a, nil, nil).Success {
		t.Fatal("a disconnected picker send must fail")
	}
}

func TestSlackModelPickerSendNoProviders(t *testing.T) {
	a, _ := approvalAdapter(t)
	if pickerSend(t, a, nil, nil).Success {
		t.Fatal("a picker without providers must fail")
	}
}

func TestSlackModelPickerSendLongCustomSlugNeverReachesOptionValue(t *testing.T) {
	a, f := approvalAdapter(t)
	f.respond("chat.postMessage", map[string]any{"ok": true, "ts": "1234.5678"})
	providers := append(pickerProviders(), map[string]any{"slug": "custom-" + strings.Repeat("x", 100), "name": "CustomCo", "models": []any{"m1"}, "total_models": int64(1)})
	if !pickerSend(t, a, providers, nil).Success {
		t.Fatal("send failed")
	}
	options := blocksOf(t, f.callsTo("chat.postMessage")[0].Args)[1].(map[string]any)["elements"].([]any)[0].(map[string]any)["options"].([]any)
	if len(options) != 3 {
		t.Fatalf("options %v", options)
	}
	for i, o := range options {
		v := o.(map[string]any)["value"].(string)
		if len(v) > 75 || v != fmt.Sprint(i) {
			t.Fatalf("option value %q", v)
		}
	}
	if !strings.Contains(options[2].(map[string]any)["text"].(map[string]any)["text"].(string), "CustomCo") {
		t.Fatalf("option %v", options[2])
	}
}

func TestSlackModelPickerSendProviderStageHintWhenOverOptionCap(t *testing.T) {
	a, f := approvalAdapter(t)
	f.respond("chat.postMessage", map[string]any{"ok": true, "ts": "1234.5678"})
	var providers []map[string]any
	for i := 0; i < 120; i++ {
		providers = append(providers, map[string]any{"slug": fmt.Sprintf("p%d", i), "name": fmt.Sprintf("P%d", i), "models": []any{fmt.Sprintf("m%d", i)}, "total_models": int64(1)})
	}
	if !pickerSend(t, a, providers, nil).Success {
		t.Fatal("send failed")
	}
	blocks := blocksOf(t, f.callsTo("chat.postMessage")[0].Args)
	if n := len(blocks[1].(map[string]any)["elements"].([]any)[0].(map[string]any)["options"].([]any)); n != 100 {
		t.Fatalf("options %d", n)
	}
	if text := blocks[0].(map[string]any)["text"].(map[string]any)["text"].(string); !strings.Contains(text, "20 more available") {
		t.Fatalf("section %q", text)
	}
}

// Seeds picker state under the team scoped marker, or the bare ts when asked
func seedPickerState(a *SlackAdapter, cb *pickerCallback, stage, selected string, bareTS bool, providers []map[string]any) any {
	var key any = [2]string{"T1", "1234.5678"}
	team := "T1"
	if bareTS {
		key, team = "1234.5678", ""
	}
	if providers == nil {
		providers = pickerProviders()
	}
	if cb == nil {
		cb = &pickerCallback{result: "ok"}
	}
	a.modelPickerState.Set(key, &modelPickerState{providers: providers, sessionKey: "s1", chatID: "C1", teamID: team, currentModel: "old",
		currentProvider: "openrouter", onModelSelected: cb.fn, stage: stage, selectedProviderSlug: selected})
	return key
}

func pickerAction(actionID, value string) map[string]any {
	return map[string]any{"action_id": actionID, "selected_option": map[string]any{"value": value}}
}

func pickerAdapter(t *testing.T) (*SlackAdapter, *fakeSlackAPI) {
	a, f := approvalAdapter(t)
	attachAuthRunner(a, nil)
	return a, f
}

func lastUpdateText(t *testing.T, f *fakeSlackAPI) string {
	t.Helper()
	updates := f.callsTo("chat.update")
	if len(updates) == 0 {
		t.Fatal("no chat.update")
	}
	return updates[len(updates)-1].Args["text"].(string)
}

func TestSlackModelPickerActionProviderSelectShowsModels(t *testing.T) {
	a, f := pickerAdapter(t)
	key := seedPickerState(a, nil, "provider", "", false, nil)
	ack := &ackRecorder{}
	a.handleModelPickerAction(context.Background(), ack.ack, pickerInteractionBody("1234.5678", "alice", "U_ALICE"), pickerAction("hermes_model_provider", "0"))
	updates := f.callsTo("chat.update")
	if ack.calls != 1 || len(updates) != 1 {
		t.Fatalf("ack %d updates %d", ack.calls, len(updates))
	}
	elements := blocksOf(t, updates[0].Args)[1].(map[string]any)["elements"].([]any)
	sel := elements[0].(map[string]any)
	var values []string
	for _, o := range sel["options"].([]any) {
		values = append(values, o.(map[string]any)["value"].(string))
	}
	if sel["action_id"] != "hermes_model_model" || strings.Join(values, ",") != "0,1" ||
		elements[1].(map[string]any)["action_id"] != "hermes_model_back" || elements[2].(map[string]any)["action_id"] != "hermes_model_cancel" {
		t.Fatalf("elements %v", elements)
	}
	state, _ := a.modelPickerState.Get(key)
	if state.stage != "model" || state.selectedProviderSlug != "openrouter" {
		t.Fatalf("state %+v", state)
	}
}

func TestSlackModelPickerActionBareTsStateResolvesOnTeamScopedClick(t *testing.T) {
	a, f := pickerAdapter(t)
	seedPickerState(a, nil, "provider", "", true, nil)
	ack := &ackRecorder{}
	a.handleModelPickerAction(context.Background(), ack.ack, pickerInteractionBody("1234.5678", "alice", "U_ALICE"), pickerAction("hermes_model_provider", "0"))
	if ack.calls != 1 || len(f.callsTo("chat.update")) != 1 {
		t.Fatalf("ack %d updates %d", ack.calls, len(f.callsTo("chat.update")))
	}
	state, _ := a.modelPickerState.Get("1234.5678")
	if state.stage != "model" || state.selectedProviderSlug != "openrouter" {
		t.Fatalf("state %+v", state)
	}
}

func TestSlackModelPickerActionProviderSelectWithNoModelsDismissesPicker(t *testing.T) {
	a, f := pickerAdapter(t)
	key := seedPickerState(a, nil, "provider", "", false, []map[string]any{{"slug": "empty", "name": "EmptyCo", "models": []any{}, "total_models": int64(0), "is_current": false}})
	ack := &ackRecorder{}
	a.handleModelPickerAction(context.Background(), ack.ack, pickerInteractionBody("1234.5678", "alice", "U_ALICE"), pickerAction("hermes_model_provider", "0"))
	if ack.calls != 1 || len(f.callsTo("chat.update")) != 1 || !strings.Contains(lastUpdateText(t, f), "No models available") || a.modelPickerState.Has(key) {
		t.Fatalf("ack %d updates %v", ack.calls, f.callsTo("chat.update"))
	}
}

func TestSlackModelPickerActionModelSelectCallsCallback(t *testing.T) {
	a, f := pickerAdapter(t)
	cb := &pickerCallback{result: "✅ Switched to claude-sonnet-4"}
	key := seedPickerState(a, cb, "model", "openrouter", false, nil)
	ack := &ackRecorder{}
	a.handleModelPickerAction(context.Background(), ack.ack, pickerInteractionBody("1234.5678", "alice", "U_ALICE"), pickerAction("hermes_model_model", "0"))
	if ack.calls != 1 || len(cb.calls) != 1 || cb.calls[0] != [3]string{"C1", "anthropic/claude-sonnet-4", "openrouter"} || a.modelPickerState.Has(key) {
		t.Fatalf("ack %d calls %v", ack.calls, cb.calls)
	}
	if n := len(f.callsTo("chat.update")); n != 2 {
		t.Fatalf("updates %d", n)
	}
	if last := lastUpdateText(t, f); !strings.Contains(last, "⚙ Model Switched") || !strings.Contains(last, "Switched to claude-sonnet-4") {
		t.Fatalf("last update %q", last)
	}
}

func TestSlackModelPickerActionModelSelectCallbackErrorIsReported(t *testing.T) {
	a, f := pickerAdapter(t)
	seedPickerState(a, &pickerCallback{err: errors.New("boom")}, "model", "openrouter", false, nil)
	a.handleModelPickerAction(context.Background(), (&ackRecorder{}).ack, pickerInteractionBody("1234.5678", "alice", "U_ALICE"), pickerAction("hermes_model_model", "1"))
	if !strings.Contains(strings.ToLower(lastUpdateText(t, f)), "failed") {
		t.Fatalf("last update %q", lastUpdateText(t, f))
	}
}

func TestSlackModelPickerActionModelSelectGatewayErrorReturnUsesFailureHeader(t *testing.T) {
	a, f := pickerAdapter(t)
	seedPickerState(a, &pickerCallback{result: "Error: Model switch to anthropic/claude-sonnet-4 failed (boom); staying on old."}, "model", "openrouter", false, nil)
	a.handleModelPickerAction(context.Background(), (&ackRecorder{}).ack, pickerInteractionBody("1234.5678", "alice", "U_ALICE"), pickerAction("hermes_model_model", "0"))
	if last := lastUpdateText(t, f); !strings.HasPrefix(last, "⚙ Model Switch Failed") || !strings.Contains(last, "staying on old") {
		t.Fatalf("last update %q", last)
	}
}

func TestSlackModelPickerActionModelSelectInvalidIndexExpiresPicker(t *testing.T) {
	a, f := pickerAdapter(t)
	cb := &pickerCallback{}
	key := seedPickerState(a, cb, "model", "openrouter", false, nil)
	ack := &ackRecorder{}
	a.handleModelPickerAction(context.Background(), ack.ack, pickerInteractionBody("1234.5678", "alice", "U_ALICE"), pickerAction("hermes_model_model", "bogus"))
	if ack.calls != 1 || len(cb.calls) != 0 || len(f.callsTo("chat.update")) != 1 || !strings.Contains(strings.ToLower(lastUpdateText(t, f)), "expired") || a.modelPickerState.Has(key) {
		t.Fatalf("ack %d calls %v updates %v", ack.calls, cb.calls, f.callsTo("chat.update"))
	}
}

func TestSlackModelPickerActionModelSelectOutOfRangeIndexExpiresPicker(t *testing.T) {
	a, f := pickerAdapter(t)
	cb := &pickerCallback{}
	key := seedPickerState(a, cb, "model", "openrouter", false, nil)
	for _, token := range []string{"2", "-1"} {
		if !a.modelPickerState.Has(key) {
			seedPickerState(a, cb, "model", "openrouter", false, nil)
		}
		a.handleModelPickerAction(context.Background(), (&ackRecorder{}).ack, pickerInteractionBody("1234.5678", "alice", "U_ALICE"), pickerAction("hermes_model_model", token))
		if len(cb.calls) != 0 {
			t.Fatalf("callback ran for %s", token)
		}
	}
	if !strings.Contains(strings.ToLower(lastUpdateText(t, f)), "expired") {
		t.Fatalf("last update %q", lastUpdateText(t, f))
	}
}

func TestSlackModelPickerActionProviderSelectInvalidIndexExpiresPicker(t *testing.T) {
	a, f := pickerAdapter(t)
	key := seedPickerState(a, nil, "provider", "", false, nil)
	ack := &ackRecorder{}
	a.handleModelPickerAction(context.Background(), ack.ack, pickerInteractionBody("1234.5678", "alice", "U_ALICE"), pickerAction("hermes_model_provider", "7"))
	if ack.calls != 1 || len(f.callsTo("chat.update")) != 1 || !strings.Contains(strings.ToLower(lastUpdateText(t, f)), "expired") || a.modelPickerState.Has(key) {
		t.Fatalf("ack %d updates %v", ack.calls, f.callsTo("chat.update"))
	}
}

func TestSlackModelPickerActionCancelClearsState(t *testing.T) {
	a, f := pickerAdapter(t)
	key := seedPickerState(a, nil, "provider", "", false, nil)
	ack := &ackRecorder{}
	a.handleModelPickerAction(context.Background(), ack.ack, pickerInteractionBody("1234.5678", "alice", "U_ALICE"), map[string]any{"action_id": "hermes_model_cancel", "value": "cancel"})
	if ack.calls != 1 || a.modelPickerState.Has(key) || len(f.callsTo("chat.update")) != 1 || !strings.Contains(strings.ToLower(lastUpdateText(t, f)), "cancelled") {
		t.Fatalf("ack %d updates %v", ack.calls, f.callsTo("chat.update"))
	}
}

func TestSlackModelPickerActionBackReturnsToProvider(t *testing.T) {
	a, f := pickerAdapter(t)
	key := seedPickerState(a, nil, "model", "openrouter", false, nil)
	a.handleModelPickerAction(context.Background(), (&ackRecorder{}).ack, pickerInteractionBody("1234.5678", "alice", "U_ALICE"), map[string]any{"action_id": "hermes_model_back", "value": "openrouter"})
	updates := f.callsTo("chat.update")
	if len(updates) != 1 || blocksOf(t, updates[0].Args)[1].(map[string]any)["elements"].([]any)[0].(map[string]any)["action_id"] != "hermes_model_provider" {
		t.Fatalf("updates %v", updates)
	}
	state, _ := a.modelPickerState.Get(key)
	if state.stage != "provider" || state.selectedProviderSlug != "" {
		t.Fatalf("state %+v", state)
	}
}

func TestSlackModelPickerActionStateNotFoundShowsExpiry(t *testing.T) {
	a, f := pickerAdapter(t)
	ack := &ackRecorder{}
	a.handleModelPickerAction(context.Background(), ack.ack, pickerInteractionBody("nonexistent", "alice", "U_ALICE"), pickerAction("hermes_model_provider", "0"))
	if ack.calls != 1 || len(f.callsTo("chat.update")) != 1 || !strings.Contains(strings.ToLower(lastUpdateText(t, f)), "expired") {
		t.Fatalf("ack %d updates %v", ack.calls, f.callsTo("chat.update"))
	}
}

func TestSlackModelPickerActionUnauthorizedUserBlocked(t *testing.T) {
	a, f := approvalAdapter(t)
	attachAuthRunner(a, func(s *gateway.SessionSource) bool { return deref(s.UserID) == "U_OWNER" })
	key := seedPickerState(a, nil, "provider", "", false, nil)
	ack := &ackRecorder{}
	a.handleModelPickerAction(context.Background(), ack.ack, pickerInteractionBody("1234.5678", "intruder", "U_BAD"), pickerAction("hermes_model_provider", "openrouter"))
	state, _ := a.modelPickerState.Get(key)
	if ack.calls != 1 || len(f.callsTo("chat.update")) != 0 || state.stage != "provider" {
		t.Fatalf("ack %d updates %v state %+v", ack.calls, f.callsTo("chat.update"), state)
	}
}
