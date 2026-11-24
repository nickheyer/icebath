package gateway

import (
	"errors"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Detail text of a runtime refusal, failing the case for any other error
func refusalText(t *testing.T, err error) (string, string) {
	t.Helper()
	var refused *state.RuntimeStoreError
	if !errors.As(err, &refused) {
		t.Fatalf("error %v is not a refusal", err)
	}
	detail := ""
	if refused.Detail != nil {
		detail = *refused.Detail
	}
	return refused.Reason, detail
}

func TestHandoffVerbsQueuePollAndFailTheSessionRow(t *testing.T) {
	ctx, db, a := newTestAuthority(t, PlatformLocal)
	actor := Principal{Subject: "human", ProfileID: "owned", Capabilities: map[string]bool{"session:read": true, "session:control": true}, TransportID: "cli"}
	c := &AuthorityConnection{authority: a, actor: actor}
	request := func(platform string) (any, error) {
		return requestHandoff(ctx, c, testRef, jsonutil.ObjectOf("session_id", "s", "platform", platform))
	}
	for platform, want := range map[string]string{
		"nosuch":   "Unknown platform 'nosuch'.",
		"discord":  "Platform 'discord' is not configured/enabled in the gateway. Set it up with icebath gateway setup, then restart the gateway with icebath gateway restart.",
		"telegram": "No home channel configured for telegram. Set one with /sethome on the destination chat first.",
	} {
		if platform == "telegram" {
			t.Setenv("TELEGRAM_BOT_TOKEN", "123456:token")
		}
		_, err := request(platform)
		if reason, detail := refusalText(t, err); reason != "invalid_params" || detail != want {
			t.Fatalf("%s refused %s %q", platform, reason, detail)
		}
	}
	t.Setenv("TELEGRAM_HOME_CHANNEL", "4242")
	got, err := request("Telegram")
	if err != nil {
		t.Fatal(err)
	}
	queued := got.(*jsonutil.Object)
	if queued.ValueOr("queued", nil) != true || queued.ValueOr("platform", nil) != "telegram" || queued.ValueOr("session_key", nil) != "s" {
		t.Fatalf("queued %v", queued)
	}
	if _, err := request("telegram"); err == nil {
		t.Fatal("a second request while one is in flight was accepted")
	} else if reason, detail := refusalText(t, err); reason != "session_busy" || detail != "Session is already in flight for handoff. Wait for it to settle, then retry." {
		t.Fatalf("in flight refused %s %q", reason, detail)
	}
	polled, err := handoffState(ctx, c, testRef, jsonutil.ObjectOf("session_id", "s"))
	if err != nil || polled.(*jsonutil.Object).ValueOr("state", nil) != "pending" || polled.(*jsonutil.Object).ValueOr("platform", nil) != "telegram" {
		t.Fatalf("state %v %v", polled, err)
	}
	failed, err := failHandoff(ctx, c, testRef, jsonutil.ObjectOf("session_id", "s", "error", "timed out waiting for gateway"))
	if err != nil || failed.(*jsonutil.Object).ValueOr("failed", nil) != true || failed.(*jsonutil.Object).ValueOr("state", nil) != "failed" {
		t.Fatalf("fail %v %v", failed, err)
	}
	if row := db.GetHandoffState(ctx, "s"); row.ValueOr("error", nil) != "timed out waiting for gateway" {
		t.Fatalf("row %v", row)
	}
	if _, err := requestHandoff(ctx, c, testRef, jsonutil.ObjectOf("session_id", "s", "platform", "telegram", "extra", 1)); err == nil {
		t.Fatal("an unknown param was accepted")
	}
}
