package telegram

import (
	"context"
	"testing"
)

func TestDisabledIsNoop(t *testing.T) {
	a, fake := newScriptedAdapter(t, map[string]any{"status_indicator": false})
	a.setStatusIndicator(context.Background(), true)
	if n := len(fake.callsOf("setMyShortDescription")); n != 0 {
		t.Fatalf("calls %d", n)
	}
}

func TestOnlineSetsDefaultText(t *testing.T) {
	a, fake := newScriptedAdapter(t, map[string]any{"status_indicator": true})
	a.setStatusIndicator(context.Background(), true)
	calls := fake.callsOf("setMyShortDescription")
	if len(calls) != 1 || calls[0].params["short_description"] != "Online" {
		t.Fatalf("calls %+v", calls)
	}
}
