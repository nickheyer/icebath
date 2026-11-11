package gateway

import (
	"context"
	"maps"
	"sync"
	"testing"
)

// Descriptor of the Python make_desc defaults, telegram with utf16 lengths, with the overrides applied
func relayTestDesc(mod func(d *CapabilityDescriptor)) CapabilityDescriptor {
	d := NewCapabilityDescriptor(RelayContractVersion, "telegram", "Telegram", 4096, false, true, true, "markdown_v2", "utf16")
	d.Emoji = "✈️"
	if mod != nil {
		mod(&d)
	}
	return d
}

// Descriptor override for the platform
func relayOnPlatform(platform string) func(d *CapabilityDescriptor) {
	return func(d *CapabilityDescriptor) { d.Platform = platform }
}

// Descriptor override for the platform and advertised ops
func relayWithOps(platform string, ops ...string) func(d *CapabilityDescriptor) {
	return func(d *CapabilityDescriptor) {
		if platform != "" {
			d.Platform = platform
		}
		d.SupportedOps = ops
	}
}

// Recording transport that answers every outbound with a scripted or successful result
type relayCapture struct {
	mu          sync.Mutex
	sent        []map[string]any
	platforms   []string
	followUps   []map[string]any
	chatInfo    []string
	result      func(action map[string]any) (map[string]any, error)
	identities  [][2]string
	byPlatform  map[string]CapabilityDescriptor
	handler     RelayInboundHandler
	disconnects int
}

func (c *relayCapture) Connect(context.Context) (bool, error) { return true, nil }

func (c *relayCapture) Disconnect(context.Context, *float64) error {
	c.mu.Lock()
	c.disconnects++
	c.mu.Unlock()
	return nil
}

func (c *relayCapture) Handshake(context.Context) (CapabilityDescriptor, error) {
	return relayTestDesc(nil), nil
}

func (c *relayCapture) SetInboundHandler(h RelayInboundHandler) { c.handler = h }

func (c *relayCapture) SendOutbound(_ context.Context, action map[string]any, platform string) (map[string]any, error) {
	c.mu.Lock()
	c.sent = append(c.sent, action)
	c.platforms = append(c.platforms, platform)
	result := c.result
	c.mu.Unlock()
	if result != nil {
		return result(action)
	}
	return map[string]any{"success": true, "message_id": "m1"}, nil
}

func (c *relayCapture) GetChatInfo(_ context.Context, chatID string) (map[string]any, error) {
	c.mu.Lock()
	c.chatInfo = append(c.chatInfo, chatID)
	c.mu.Unlock()
	return map[string]any{"name": "general", "type": "channel"}, nil
}

func (c *relayCapture) SendInterrupt(context.Context, string, *string) error { return nil }

func (c *relayCapture) SendFollowUp(_ context.Context, action map[string]any, platform string) (map[string]any, error) {
	c.mu.Lock()
	c.followUps = append(c.followUps, action)
	c.platforms = append(c.platforms, platform)
	c.mu.Unlock()
	return map[string]any{"success": true, "message_id": "f1"}, nil
}

func (c *relayCapture) RelayIdentities() any {
	out := make([]any, len(c.identities))
	for i, id := range c.identities {
		out[i] = []any{id[0], id[1]}
	}
	return out
}

func (c *relayCapture) DescriptorForPlatform(platform string) any {
	if d, ok := c.byPlatform[platform]; ok {
		return d
	}
	return nil
}

// Last outbound action and its platform tag
func (c *relayCapture) last(t *testing.T) (map[string]any, string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sent) == 0 {
		t.Fatal("no outbound frame")
	}
	return c.sent[len(c.sent)-1], c.platforms[len(c.platforms)-1]
}

// Outbound actions of the op
func (c *relayCapture) ops(op string) []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, a := range c.sent {
		if a["op"] == op {
			out = append(out, a)
		}
	}
	return out
}

// Adapter over a capture transport with the descriptor overrides applied
func relayCaptureAdapter(t *testing.T, mod func(d *CapabilityDescriptor)) (*RelayAdapter, *relayCapture) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := &relayCapture{byPlatform: map[string]CapabilityDescriptor{}}
	return NewRelayAdapter(NewPlatformConfig(), relayTestDesc(mod), c), c
}

// Inbound text event of the platform and chat with the source fields set
func relayInbound(platform Platform, chatID, chatType string, set func(s *SessionSource)) *MessageEvent {
	src := &SessionSource{Platform: platform, ChatID: chatID, ChatType: chatType}
	if set != nil {
		set(src)
	}
	e := NewMessageEvent("hi")
	e.MessageType = MessageTypeText
	e.Source = src
	return e
}

func relayStrPtr(s string) *string { return &s }

// Metadata map equality on the Python value model
func relayMetaEqual(got any, want map[string]any) bool {
	m, ok := got.(map[string]any)
	return ok && maps.EqualFunc(m, want, func(a, b any) bool { return relayDeepEqual(a, b) })
}

func relayDeepEqual(a, b any) bool {
	switch x := a.(type) {
	case map[string]any:
		return relayMetaEqual(b, x)
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !relayDeepEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	}
	return a == b
}
