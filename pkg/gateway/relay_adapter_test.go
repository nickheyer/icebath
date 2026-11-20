package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestAdvertisesDescriptorMaxLength(t *testing.T) {
	a, _ := relayCaptureAdapter(t, func(d *CapabilityDescriptor) { d.MaxMessageLength = 2000 })
	if a.Traits.MaxMessageLength != 2000 {
		t.Fatalf("max %d", a.Traits.MaxMessageLength)
	}
}

func TestLenFnUtf16CountsCodeUnits(t *testing.T) {
	a, _ := relayCaptureAdapter(t, func(d *CapabilityDescriptor) { d.LenUnit = "utf16" })
	if n := a.MessageLenFn()("\U0001f600"); n != 2 {
		t.Fatalf("len %d", n)
	}
}

func TestSendReattachesDmUserIDFromInboundScope(t *testing.T) {
	a, c := relayCaptureAdapter(t, relayOnPlatform("discord"))
	a.captureScope(relayInbound(PlatformRelay, "dm-1", "dm", func(s *SessionSource) { s.UserID = relayStrPtr("user-42") }))
	if _, err := a.Send(context.Background(), "dm-1", "the reply", "", nil); err != nil {
		t.Fatal(err)
	}
	sent, _ := c.last(t)
	md := sent["metadata"].(map[string]any)
	if _, has := md["scope_id"]; md["user_id"] != "user-42" || has {
		t.Fatalf("metadata %v", md)
	}
}

func TestScopedReplyReattachesBothScopeIDAndUserID(t *testing.T) {
	a, c := relayCaptureAdapter(t, relayOnPlatform("discord"))
	a.captureScope(relayInbound(PlatformRelay, "chan-1", "channel", func(s *SessionSource) {
		s.ScopeID, s.UserID = relayStrPtr("scope-9"), relayStrPtr("user-42")
	}))
	if _, err := a.Send(context.Background(), "chan-1", "hi", "", nil); err != nil {
		t.Fatal(err)
	}
	sent, _ := c.last(t)
	md := sent["metadata"].(map[string]any)
	if md["scope_id"] != "scope-9" || md["user_id"] != "user-42" {
		t.Fatalf("metadata %v", md)
	}
}

func TestStopTypingForwardsExplicitClearWithRoutingContext(t *testing.T) {
	a, c := relayCaptureAdapter(t, relayOnPlatform("slack"))
	a.captureScope(relayInbound(PlatformSlack, "channel-1", "channel", func(s *SessionSource) { s.ScopeID = relayStrPtr("workspace-1") }))
	if err := a.StopTyping(context.Background(), "channel-1", map[string]any{"thread_id": "thread-1"}); err != nil {
		t.Fatal(err)
	}
	sent, platform := c.last(t)
	want := map[string]any{"op": "typing", "chat_id": "channel-1", "content": "",
		"metadata": map[string]any{"thread_id": "thread-1", "scope_id": "workspace-1"}}
	if !relayMetaEqual(sent, want) || platform != "slack" {
		t.Fatalf("sent %v platform %q", sent, platform)
	}
}

func TestSendTypingTagsEgressPlatform(t *testing.T) {
	a, c := relayCaptureAdapter(t, relayOnPlatform("discord"))
	a.captureScope(relayInbound(PlatformDiscord, "chan-2", "channel", func(s *SessionSource) { s.ScopeID = relayStrPtr("scope-1") }))
	if err := a.SendTyping(context.Background(), "chan-2", nil); err != nil {
		t.Fatal(err)
	}
	if _, platform := c.last(t); platform != "discord" {
		t.Fatalf("platform %q", platform)
	}
}

func TestGetChatInfoLocalFallbackWhenNotAdvertised(t *testing.T) {
	a, c := relayCaptureAdapter(t, relayWithOps("", "send", "edit", "typing"))
	info, err := a.GetChatInfo(context.Background(), "chan-1")
	if err != nil || !relayMetaEqual(info, map[string]any{"name": "chan-1", "type": "dm"}) || len(c.chatInfo) != 0 {
		t.Fatalf("info %v calls %v err %v", info, c.chatInfo, err)
	}
}

// Transport that hangs in go idle until its caller is cancelled
type relayHangOnIdle struct {
	relayCapture
	started  chan struct{}
	once     sync.Once
	timeouts []float64
}

func (h *relayHangOnIdle) GoIdle(ctx context.Context, timeoutS float64) bool {
	h.mu.Lock()
	h.timeouts = append(h.timeouts, timeoutS)
	h.mu.Unlock()
	h.once.Do(func() { close(h.started) })
	select {
	case <-ctx.Done():
	case <-time.After(time.Hour):
	}
	return false
}

func TestDisconnectTearsDownTransportWhenGoIdleIsCancelled(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	transport := &relayHangOnIdle{started: make(chan struct{})}
	a := NewRelayAdapter(NewPlatformConfig(), relayTestDesc(relayOnPlatform("discord")), transport)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Disconnect(ctx) }()
	select {
	case <-transport.started:
	case <-time.After(time.Second):
		t.Fatal("go idle never started")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("disconnect returned %v", err)
	}
	if transport.disconnects != 1 || len(transport.timeouts) == 0 || transport.timeouts[0] >= 5.0 {
		t.Fatalf("disconnects %d timeouts %v", transport.disconnects, transport.timeouts)
	}
}
