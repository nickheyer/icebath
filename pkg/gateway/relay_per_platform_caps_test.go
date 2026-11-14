package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// Plain descriptor of a fronted platform with its length cap and unit
func relayCapsDesc(platform string, maxLen int, lenUnit string) CapabilityDescriptor {
	return NewCapabilityDescriptor(RelayContractVersion, platform, strings.ToUpper(platform[:1])+platform[1:], maxLen, false, true, false, "plain", lenUnit)
}

func TestTransportDescriptorMapResetsOnRedial(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	srv := newRelayWSServer(t, func(*relayWSServer, *websocket.Conn, int) bool { return true })
	tr := NewWebSocketRelayTransport(srv.url, "telegram", "bot-9", RelayTransportOptions{Identities: [][2]string{{"telegram", "bot-9"}, {"discord", "app-1"}}})
	ctx := context.Background()
	tr.mu.Lock()
	tr.descriptorReady = newRelayFuture[CapabilityDescriptor]()
	tr.mu.Unlock()
	frame, _ := json.Marshal(map[string]any{"type": "descriptor", "descriptor": relayWSDescriptor})
	if err := tr.handleFrame(ctx, string(frame)); err != nil {
		t.Fatal(err)
	}
	if tr.DescriptorForPlatform("discord") == nil {
		t.Fatal("discord descriptor not recorded")
	}
	if err := tr.dialAndStart(ctx); err != nil {
		t.Fatal(err)
	}
	defer relayDisconnectNow(tr)
	tr.mu.Lock()
	primary := tr.descriptor
	tr.mu.Unlock()
	if tr.DescriptorForPlatform("discord") != nil || primary != nil {
		t.Fatal("a redial must reset the negotiated descriptors")
	}
}

func TestAdapterResolvesPerChatLimitsFromInboundPlatform(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	telegram, discord := relayCapsDesc("telegram", 4096, "utf16"), relayCapsDesc("discord", 2000, "chars")
	c := &relayCapture{desc: &telegram, result: relayConnectorResult, identities: [][2]string{{"telegram", "bot-9"}, {"discord", "app-1"}},
		byPlatform: map[string]CapabilityDescriptor{"telegram": telegram, "discord": discord}}
	a := NewRelayAdapter(NewPlatformConfig(), telegram, c)
	ctx := context.Background()
	if _, err := a.Connect(ctx, false); err != nil {
		t.Fatal(err)
	}
	for _, push := range []struct {
		platform Platform
		chat     string
	}{{PlatformDiscord, "dc-1"}, {PlatformTelegram, "tg-1"}} {
		e := relayInbound(push.platform, push.chat, "dm", func(s *SessionSource) { s.UserID = relayStrPtr("u-1") })
		if _, err := c.handler(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	dc, _ := a.MaxMessageLengthForChat("dc-1")
	tg, _ := a.MaxMessageLengthForChat("tg-1")
	tgLen, _ := a.MessageLenFnForChat("tg-1")
	dcLen, _ := a.MessageLenFnForChat("dc-1")
	if a.Traits.MaxMessageLength != 4096 || dc != 2000 || tg != 4096 || tgLen("\U0001f600") != 2 || dcLen("\U0001f600") != 1 {
		t.Fatalf("max %d dc %d tg %d", a.Traits.MaxMessageLength, dc, tg)
	}
}

// Adapter whose transport raises a dropped socket on every outbound
func relayExplodingAdapter(t *testing.T) *RelayAdapter {
	t.Helper()
	a, c := relayCaptureAdapter(t, relayLiveCardsDesc)
	c.result = func(map[string]any) (map[string]any, error) {
		return nil, &PyError{Kind: "ConnectionError", Msg: "socket dropped"}
	}
	return a
}

func TestTaskCardTransportFailureProgressReturnsFailedResult(t *testing.T) {
	a := relayExplodingAdapter(t)
	tasks := []map[string]string{{"id": "t1", "title": "terminal", "status": "in_progress"}}
	r, err := a.SendNativeTaskCardProgress(context.Background(), "C1", tasks, "", relayStrPtr("1700.1"), nil, "")
	if err != nil || r.Success || !strings.Contains(relayErrText(r), "transport error") {
		t.Fatalf("result %+v err %v", r, err)
	}
}

func TestTaskCardTransportFailureStopReturnsFailedResult(t *testing.T) {
	a := relayExplodingAdapter(t)
	r, err := a.StopNativeTaskCardProgress(context.Background(), "C1", relayStrPtr("1700.1"), nil)
	if err != nil || r.Success || !strings.Contains(relayErrText(r), "transport error") {
		t.Fatalf("result %+v err %v", r, err)
	}
}

// Error text of a send result, empty when none
func relayErrText(r SendResult) string {
	if s, ok := r.Error.(string); ok {
		return s
	}
	return ""
}
