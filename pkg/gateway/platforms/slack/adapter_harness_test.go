package slack

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/settings"
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
	a := NewSlackAdapter(context.Background(), &gateway.PlatformConfig{Enabled: true, Token: "xoxb-fake-token", Extra: slackSettingsHome(t, extra)})
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

// Points ICEBATH_HOME at a fresh home whose config.yaml holds the slack settings of extra, returning the other keys
func slackSettingsHome(t *testing.T, extra map[string]any) map[string]any {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	block, rest := map[string]any{}, map[string]any{}
	for k, v := range extra {
		if settings.Lookup("slack."+k) != nil {
			block[k] = v
		} else {
			rest[k] = v
		}
	}
	doc, err := json.Marshal(map[string]any{"slack": block})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), doc, 0o600); err != nil {
		t.Fatal(err)
	}
	return rest
}

// Unwrapped string of an optional value
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
