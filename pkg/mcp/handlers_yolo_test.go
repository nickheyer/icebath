package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Records the trust metadata discovery captures for srv with its write and paid_render tools
func registerYoloServer(t *testing.T, cfg map[string]any) {
	t.Helper()
	resetHandlerLedgers(t)
	if err := recordToolTrustMetadata(context.Background(), "srv", cfg, []*Tool{{Name: "write"}, {Name: "paid_render"}}, nil); err != nil {
		t.Fatal(err)
	}
}

// Enables yolo for one session key until the test ends
func yoloSession(t *testing.T, sessionKey string) {
	t.Helper()
	tools.EnableSessionYolo(sessionKey)
	t.Cleanup(func() { tools.DisableSessionYolo(sessionKey) })
}

var approvalsOff = config.Tree{"approvals": map[string]any{"mode": "off"}}

func TestTrustYoloAndModeOffSkipPromptForCallingSessionOnly(t *testing.T) {
	home := isolateHome(t)
	registerYoloServer(t, map[string]any{"trust": "untrusted"})
	yoloSession(t, "tg:yolo")
	yolo := openScopeChat(t, home, "tg:yolo", "deny")
	if got := yolo.gate(t, "write"); got != "" {
		t.Fatalf("yolo session gated %s", got)
	}
	other := openScopeChat(t, home, "tg:other", "deny")
	if got := other.gate(t, "write"); got == "" {
		t.Fatal("other chat ran without a prompt")
	}
	if len(yolo.seen()) != 0 || len(other.seen()) != 1 {
		t.Fatalf("yolo cards %d other cards %d", len(yolo.seen()), len(other.seen()))
	}
	again := openScopeChat(t, home, "tg:other-off", "deny")
	again.env.Config = approvalsOff
	if got := again.gate(t, "write"); got != "" || len(again.seen()) != 0 {
		t.Fatalf("approvals off gated %s cards %d", got, len(again.seen()))
	}
}

func TestTrustYoloSpendLimitStillAsksAndRefusesUnattended(t *testing.T) {
	home := isolateHome(t)
	registerYoloServer(t, map[string]any{"trust": "untrusted", "tool_costs": map[string]any{"paid_render": 40}, "spend_limit": 50})
	yoloSession(t, "tg:yolo")
	chat := openScopeChat(t, home, "tg:yolo", "deny")
	if got := chat.gate(t, "paid_render"); got != "" {
		t.Fatalf("first paid call gated %s", got)
	}
	refused := chat.gate(t, "paid_render")
	cards := chat.seen()
	if len(cards) != 1 {
		t.Fatalf("cards %v", cards)
	}
	if card := cards[0]["command"].(string); !strings.Contains(card, "spend limit") || strings.Contains(card, "UNTRUSTED") {
		t.Fatalf("card %q", card)
	}
	if msg, _ := decodeResult(t, refused)["error"].(string); !strings.Contains(msg, "spend limit is 50 credits") {
		t.Fatalf("refusal %s", refused)
	}
	cron := sessionEnv(map[string]any{"cron": true, "key": "cron:job"})
	cron.Home, cron.Config = home, approvalsOff
	gate := func(tool string) string {
		var booked []*Charge
		return trustGateCheck(envContext(cron), cron, "srv", tool, &booked, nil, "")
	}
	if got := gate("write"); got != "" {
		t.Fatalf("cron untrusted write gated %s", got)
	}
	if got := gate("paid_render"); got != "" {
		t.Fatalf("cron first paid call gated %s", got)
	}
	refused = gate("paid_render")
	if msg, _ := decodeResult(t, refused)["error"].(string); !strings.Contains(msg, "spend limit is 50 credits") {
		t.Fatalf("cron refusal %s", refused)
	}
}
