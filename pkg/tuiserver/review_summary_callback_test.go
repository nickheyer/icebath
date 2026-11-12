package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

func TestInitSessionAttachesBackgroundReviewCallback(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := testAgent(t, agent.AIAgentOptions{Model: "fake/model"})
	recorder := &frameRecorder{}
	ctx := tuigateway.BindTransport(context.Background(), recorder)
	initSession(ctx, "sid-abc", "session-key", a, []any{}, 80, t.TempDir(), nil, "", "", false)
	t.Cleanup(func() { closeSessionByID(context.Background(), "sid-abc", "test_cleanup", nil) })
	cb, isFunc := a.BackgroundReviewCallback().(func(string))
	if !isFunc || cb == nil {
		t.Fatalf("initSession attached no background review callback: %T", a.BackgroundReviewCallback())
	}
	recorder.mu.Lock()
	recorder.frames = nil
	recorder.mu.Unlock()
	message := "💾 Self-improvement review: Skill 'hermes-release' patched"
	cb(message)
	var matched [][2]any
	recorder.mu.Lock()
	for _, frame := range recorder.frames {
		params, _ := frame.GetOr("params", nil).(interface{ GetOr(string, any) any })
		if params == nil || params.GetOr("type", nil) != "review.summary" {
			continue
		}
		matched = append(matched, [2]any{params.GetOr("session_id", nil), params.GetOr("payload", nil)})
	}
	recorder.mu.Unlock()
	if len(matched) != 1 {
		t.Fatalf("review.summary frames %v", matched)
	}
	if matched[0][0] != "sid-abc" {
		t.Fatalf("review.summary for %v", matched[0][0])
	}
	payload := map[string]any{}
	switch p := matched[0][1].(type) {
	case map[string]any:
		payload = p
	case interface{ GetOr(string, any) any }:
		payload["text"] = p.GetOr("text", nil)
	}
	if !reflect.DeepEqual(payload, map[string]any{"text": message}) {
		t.Fatalf("payload %v", matched[0][1])
	}
}

func TestLoadMemoryNotificationsNormalization(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, expected string
	}{
		{"None-on", "", "on"},
		{"on-on", "display:\n  memory_notifications: \"on\"\n", "on"},
		{"off-off", "display:\n  memory_notifications: \"off\"\n", "off"},
		{"verbose-verbose", "display:\n  memory_notifications: verbose\n", "verbose"},
		{"VERBOSE-verbose", "display:\n  memory_notifications: VERBOSE\n", "verbose"},
		{"True-on", "display:\n  memory_notifications: true\n", "on"},
		{"False-off", "display:\n  memory_notifications: false\n", "off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := t.TempDir()
			t.Setenv("ICEBATH_HOME", h)
			if tc.yaml != "" {
				if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(tc.yaml), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := loadMemoryNotifications(context.Background()); got != tc.expected {
				t.Fatalf("got %q want %q", got, tc.expected)
			}
		})
	}
}
