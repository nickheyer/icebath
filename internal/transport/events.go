package transport

import "github.com/nickheyer/icebath/internal/provider"

// Sends one event on to the consumer of a stream, when there is one
func Say(fn provider.StreamFunc, kind provider.EventKind, text string) {
	if fn != nil {
		_ = fn(provider.StreamEvent{Kind: kind, Text: text})
	}
}

// Sends a reply that came whole on as the events a stream would have given: its reasoning, its text and its tool calls
func Replay(response *provider.Response, fn provider.StreamFunc) error {
	if fn == nil || response == nil || response.Message == nil {
		return nil
	}
	if reasoning := response.Message.GetReasoning(); reasoning != "" {
		if err := fn(provider.StreamEvent{Kind: provider.EventReasoningDelta, Text: reasoning}); err != nil {
			return err
		}
	}
	if text := response.Message.GetText(); text != "" {
		if err := fn(provider.StreamEvent{Kind: provider.EventTextDelta, Text: text}); err != nil {
			return err
		}
	}
	for _, call := range response.Message.GetToolCalls() {
		if err := fn(provider.StreamEvent{Kind: provider.EventToolStart, Text: call.GetName()}); err != nil {
			return err
		}
	}
	return nil
}
