package gateway

import (
	"context"
	"testing"
)

// Base adapter whose class flag and native probe the resolver reads, frames recorded
type nativeStreamFake struct {
	*baseFakeAdapter
	supports bool
	frames   []string
}

func (a *nativeStreamFake) SupportsNativeStreaming(chatType string, metadata map[string]any) bool {
	return a.supports
}

func (a *nativeStreamFake) SendStreamFrame(ctx context.Context, text string, finalize bool, chatID, replyTo, turnID string) (bool, error) {
	a.frames = append(a.frames, text)
	return true, nil
}

var _ NativeStreamAdapter = (*nativeStreamFake)(nil)

// Adapter of the hermes helper, the class flag set unless bare
func newNativeStreamFake(classFlag, supports bool) *nativeStreamFake {
	a := &nativeStreamFake{baseFakeAdapter: newBaseFake(Platform("wecom")), supports: supports}
	a.Traits.MaxMessageLength = 4096
	a.Traits.SupportsMessageEditing = false
	a.Traits.SupportsNativeStreaming = classFlag
	return a
}

func TestNativeStreamingResolverCapableAdapterResolvesToNative(t *testing.T) {
	adapter := newNativeStreamFake(true, true)
	consumer := NewGatewayStreamConsumer(adapter, "chat-1", StreamConsumerOptions{Config: &StreamConsumerConfig{ChatType: "dm"}})
	if !consumer.resolveNativeStreaming() {
		t.Fatal("capable adapter must resolve to native")
	}
}

func TestNativeStreamingResolverClassAttributeRequired(t *testing.T) {
	adapter := newNativeStreamFake(false, true)
	adapter.Traits.SupportsMessageEditing = true
	consumer := NewGatewayStreamConsumer(adapter, "chat-1", StreamConsumerOptions{Config: &StreamConsumerConfig{ChatType: "dm"}})
	if consumer.resolveNativeStreaming() {
		t.Fatal("an adapter without SUPPORTS_NATIVE_STREAMING must not resolve native")
	}
}

func TestNativeStreamingResolverProbeReturningFalseDisablesNative(t *testing.T) {
	adapter := newNativeStreamFake(true, false)
	consumer := NewGatewayStreamConsumer(adapter, "chat-1", StreamConsumerOptions{Config: &StreamConsumerConfig{ChatType: "dm"}})
	if consumer.resolveNativeStreaming() {
		t.Fatal("a false probe must disable native")
	}
}
