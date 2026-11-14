package gatewayrun

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Limit payload of a usage window refusal, the hermes LIMIT fixture
func usageLimitFixture() map[string]any {
	return map[string]any{
		"gauge_id": "day", "scope": "all", "limit_name": "daily", "resets_at_ms": int64(4_000_000_000_000),
		"message":     "You've hit your daily limit · resets 15:56",
		"suggestions": []any{map[string]any{"model": "example/model-free:free", "label": "Model Free", "note": "free", "provider": "nous"}},
		"credits":     map[string]any{"action": "add_credits"},
	}
}

// Picker capable adapter recording each picker it was asked to send
type pickerAdapter struct {
	*updateTestAdapter
	pickerOK bool
	titles   []string
	choices  [][]map[string]any
}

func (a *pickerAdapter) SendChoicePicker(_ context.Context, _, title string, choices []map[string]any, _ string,
	_ func(ctx context.Context, chatID, value string) (string, error), _ map[string]any) (gateway.SendResult, error) {
	a.titles = append(a.titles, title)
	a.choices = append(a.choices, choices)
	return gateway.SendResult{Success: a.pickerOK}, nil
}

func newPickerAdapter(pickerOK bool) *pickerAdapter {
	a := &pickerAdapter{updateTestAdapter: &updateTestAdapter{}, pickerOK: pickerOK}
	cfg := gateway.NewPlatformConfig()
	cfg.Enabled = true
	a.updateTestAdapter.BasePlatformAdapter = gateway.NewBasePlatformAdapter(cfg, gateway.PlatformTelegram, a)
	return a
}

// Telegram text adapter with its own typed command prefix
func newPrefixedTextAdapter(prefix string) *updateTestAdapter {
	a := newUpdateTestAdapter()
	a.Traits.TypedCommandPrefix = prefix
	return a
}

// Runner serving the adapter on telegram
func offerRunner(t *testing.T, adapter gateway.PlatformAdapter) *GatewayRunner {
	t.Helper()
	r, _, _ := updateRunner(t)
	r.adapters = adapterDict(gateway.PlatformTelegram, adapter)
	return r
}

// Telegram chat event of the refused turn
func offerEvent() *gateway.MessageEvent {
	event := gateway.NewMessageEvent("hi")
	event.Source = &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "c1", ChatType: "dm"}
	return event
}

func TestAButtonPlatformGetsOneChoicePerSuggestion(t *testing.T) {
	adapter := newPickerAdapter(true)
	r := offerRunner(t, adapter)
	r.offerUsageLimitSwitch(context.Background(), offerEvent(), usageLimitFixture(), nil)
	if len(adapter.titles) != 1 || adapter.titles[0] != "Keep going on another model:" {
		t.Fatalf("titles = %q", adapter.titles)
	}
	want := []map[string]any{{"value": "example/model-free:free", "label": "Switch to Model Free (free)", "is_current": false}}
	if !reflect.DeepEqual(adapter.choices[0], want) {
		t.Fatalf("choices = %v", adapter.choices[0])
	}
	if sent := adapter.sends(); len(sent) != 0 {
		t.Fatalf("sent = %q", sent)
	}
}

func TestAFailedPickerFallsBackToTheTextLine(t *testing.T) {
	adapter := newPickerAdapter(false)
	r := offerRunner(t, adapter)
	r.offerUsageLimitSwitch(context.Background(), offerEvent(), usageLimitFixture(), nil)
	sent := adapter.sends()
	if len(sent) != 1 || sent[0] != "Switch to Model Free (free) with /model, or add credits in the Portal." {
		t.Fatalf("sent = %q", sent)
	}
}

func TestATextPlatformGetsOneLineWithItsOwnPrefixAndNoTopup(t *testing.T) {
	adapter := newPrefixedTextAdapter("!")
	r := offerRunner(t, adapter)
	limit := usageLimitFixture()
	limit["credits"] = map[string]any{"action": "use_credits"}
	r.offerUsageLimitSwitch(context.Background(), offerEvent(), limit, nil)
	sent := adapter.sends()
	if len(sent) != 1 || !strings.HasPrefix(sent[0], "Switch to Model Free (free) with !model") {
		t.Fatalf("sent = %q", sent)
	}
	if !strings.Contains(sent[0], "Portal") || strings.Contains(sent[0], "/topup") {
		t.Fatalf("sent = %q", sent[0])
	}
}

func TestNoSuggestionStillSaysHowToChangeModel(t *testing.T) {
	adapter := newPickerAdapter(true)
	r := offerRunner(t, adapter)
	limit := usageLimitFixture()
	limit["suggestions"], limit["credits"] = []any{}, nil
	r.offerUsageLimitSwitch(context.Background(), offerEvent(), limit, nil)
	if len(adapter.titles) != 0 {
		t.Fatalf("picker sent: %q", adapter.titles)
	}
	if sent := adapter.sends(); len(sent) != 1 || sent[0] != "Pick another model with /model." {
		t.Fatalf("sent = %q", sent)
	}
}

func TestTheOfferNeverRaises(t *testing.T) {
	r, _, _ := updateRunner(t)
	r.adapters = adapterDict()
	r.offerUsageLimitSwitch(context.Background(), offerEvent(), usageLimitFixture(), nil)
	r.offerUsageLimitSwitch(context.Background(), &gateway.MessageEvent{}, usageLimitFixture(), nil)
}

func TestTheOfferWaitsForTheReplyToLand(t *testing.T) {
	adapter := newPickerAdapter(true)
	r := offerRunner(t, adapter)
	generation := 7
	r.scheduleUsageLimitOffer(context.Background(), offerEvent(), usageLimitFixture(), "chat:c1", &generation)
	if callback := popPostDeliveryCallback(adapter, "chat:c1", &generation); callback == nil {
		t.Fatal("offer was not registered as the post delivery callback of generation 7")
	}
	if len(adapter.titles) != 0 || len(adapter.sends()) != 0 {
		t.Fatal("offer ran before the reply landed")
	}
}
