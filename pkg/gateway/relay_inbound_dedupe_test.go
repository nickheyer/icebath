package gateway

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Slack descriptor of the dedupe cases, draft capable with send, edit and typing advertised
func relaySlackDedupeDesc(d *CapabilityDescriptor) {
	d.Platform, d.Label, d.MaxMessageLength, d.SupportsDraftStreaming = "slack", "Slack", 39000, true
	d.MarkdownDialect, d.LenUnit, d.Emoji = "slack", "chars", "\U0001f4ac"
	d.SupportedOps = []string{"send", "edit", "typing"}
}

// Records every event the gateway handler receives, returning a counter
func relayTapHandled(a *RelayAdapter) func() int {
	var mu sync.Mutex
	n := 0
	a.SetMessageHandler(func(context.Context, *MessageEvent) (any, error) {
		mu.Lock()
		n++
		mu.Unlock()
		return nil, nil
	})
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

// Waits for the count to reach n, then for any further dispatch to show itself
func relaySettledCount(count func() int, n int) int {
	relayWaitUntil(func() bool { return count() >= n })
	time.Sleep(150 * time.Millisecond)
	return count()
}

func relayDedupeEvent(messageID *string, chatID string) *MessageEvent {
	e := relayInbound(PlatformSlack, chatID, "channel", func(s *SessionSource) {
		s.UserID, s.MessageID = relayStrPtr("U1"), messageID
	})
	e.MessageID = messageID
	return e
}

func relayDeliver(t *testing.T, a *RelayAdapter, e *MessageEvent) {
	t.Helper()
	if _, err := a.onInbound(context.Background(), e); err != nil {
		t.Fatal(err)
	}
}

func TestInboundReplayDedupeReplayedInboundDropped(t *testing.T) {
	a, _ := relayCaptureAdapter(t, relaySlackDedupeDesc)
	handled := relayTapHandled(a)
	e := relayDedupeEvent(relayStrPtr("1700.100"), "C1")
	relayDeliver(t, a, e)
	relayDeliver(t, a, e)
	if n := relaySettledCount(handled, 1); n != 1 {
		t.Fatalf("handled %d", n)
	}
}

func TestInboundReplayDedupeDistinctMessagesBothHandled(t *testing.T) {
	a, _ := relayCaptureAdapter(t, relaySlackDedupeDesc)
	handled := relayTapHandled(a)
	relayDeliver(t, a, relayDedupeEvent(relayStrPtr("1700.100"), "C1"))
	relayWaitUntil(func() bool { return handled() >= 1 })
	relayDeliver(t, a, relayDedupeEvent(relayStrPtr("1700.200"), "C1"))
	if n := relaySettledCount(handled, 2); n != 2 {
		t.Fatalf("handled %d", n)
	}
}

func TestInboundReplayDedupeMissingMessageIDFailsOpen(t *testing.T) {
	a, _ := relayCaptureAdapter(t, relaySlackDedupeDesc)
	handled := relayTapHandled(a)
	e := relayDedupeEvent(nil, "C1")
	relayDeliver(t, a, e)
	relayWaitUntil(func() bool { return handled() >= 1 })
	relayDeliver(t, a, e)
	if n := relaySettledCount(handled, 2); n != 2 {
		t.Fatalf("handled %d", n)
	}
}

func TestInboundReplayDedupeSeenSetBounded(t *testing.T) {
	a, _ := relayCaptureAdapter(t, relaySlackDedupeDesc)
	for i := range 600 {
		relayDeliver(t, a, relayDedupeEvent(relayStrPtr(fmt.Sprintf("ts.%d", i)), "C1"))
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.seenInbound.order) > relaySeenInboundMax || len(a.seenInbound.set) > relaySeenInboundMax {
		t.Fatalf("seen %d", len(a.seenInbound.order))
	}
}

// Wire frame of the dedupe cases decoded the way the transport decodes it, the source platform overridable
func relayDecodeDedupeWire(t *testing.T, platform string) *MessageEvent {
	t.Helper()
	raw := pyjson.ObjectOf("text", "hi", "message_type", "text", "message_id", "1700.100",
		"source", pyjson.ObjectOf("platform", platform, "chat_id", "C1", "chat_type", "channel", "user_id", "U1", "message_id", "1700.100"))
	e, err := relayEventFromWire(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestWireLevelReplayDedupeReplayedWireFrameDropped(t *testing.T) {
	a, _ := relayCaptureAdapter(t, relaySlackDedupeDesc)
	handled := relayTapHandled(a)
	relayDeliver(t, a, relayDecodeDedupeWire(t, "slack"))
	relayDeliver(t, a, relayDecodeDedupeWire(t, "slack"))
	if n := relaySettledCount(handled, 1); n != 1 {
		t.Fatalf("handled %d", n)
	}
}

func TestWireLevelReplayDedupeSameIDsOnDifferentPlatformsNotConflated(t *testing.T) {
	a, _ := relayCaptureAdapter(t, relaySlackDedupeDesc)
	handled := relayTapHandled(a)
	relayDeliver(t, a, relayDecodeDedupeWire(t, "slack"))
	relayWaitUntil(func() bool { return handled() >= 1 })
	relayDeliver(t, a, relayDecodeDedupeWire(t, "discord"))
	if n := relaySettledCount(handled, 2); n != 2 {
		t.Fatalf("handled %d", n)
	}
}

func relayDedupeKeyFor(platform Platform) (string, bool) {
	e := relayInbound(platform, "C1", "channel", func(s *SessionSource) { s.MessageID = relayStrPtr("m1") })
	e.MessageID = relayStrPtr("m1")
	return relayInboundDedupeKey(e)
}

func TestDedupeKeyPlatformNormalizationEnumAndStringSpellingsProduceOneKey(t *testing.T) {
	enum, _ := relayDedupeKeyFor(PlatformSlack)
	str, _ := relayDedupeKeyFor(Platform("slack"))
	if enum != str {
		t.Fatalf("%q != %q", enum, str)
	}
}

func TestDedupeKeyPlatformNormalizationDistinctStringPlatformsStayDistinct(t *testing.T) {
	slack, _ := relayDedupeKeyFor(Platform("slack"))
	discord, _ := relayDedupeKeyFor(Platform("discord"))
	if slack == discord {
		t.Fatalf("collapsed %q", slack)
	}
}

func TestDedupeKeyPlatformNormalizationMissingPlatformStillYieldsAKey(t *testing.T) {
	if _, ok := relayDedupeKeyFor(""); !ok {
		t.Fatal("no key")
	}
}
