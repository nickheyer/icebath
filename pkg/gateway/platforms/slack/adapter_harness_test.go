package slack

import (
	"context"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Messages the adapter handed to the gateway during a test
type capturedEvents struct {
	mu     sync.Mutex
	events []*gateway.MessageEvent
}

func (c *capturedEvents) all() []*gateway.MessageEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*gateway.MessageEvent(nil), c.events...)
}

// Connected adapter on the fake Slack API with bot U_BOT and handle_message captured
func newHarnessAdapter(t *testing.T, extra map[string]any) (*SlackAdapter, *fakeSlackAPI, *capturedEvents) {
	t.Helper()
	f := newFakeSlackAPI(t)
	if extra == nil {
		extra = map[string]any{}
	}
	a := NewSlackAdapter(context.Background(), &gateway.PlatformConfig{Enabled: true, Token: "xoxb-fake-token", Extra: extra})
	bot := "U_BOT"
	a.app = &slackApp{client: newWebClient("xoxb-fake-token", "")}
	a.botUserID = &bot
	a.SetRunning(true)
	captured := &capturedEvents{}
	prev := handleMessageFn
	handleMessageFn = func(_ *SlackAdapter, _ context.Context, event *gateway.MessageEvent) error {
		captured.mu.Lock()
		captured.events = append(captured.events, event)
		captured.mu.Unlock()
		return nil
	}
	t.Cleanup(func() { handleMessageFn = prev })
	return a, f, captured
}

// Unwrapped string of an optional value
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
