package tools

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Resets recorded profile policy and records the launch profile's switch for one test
func pinRedactPolicy(t *testing.T, launch bool) {
	t.Helper()
	redactPolicy.Lock()
	orig := redactPolicy.cfg
	redactPolicy.cfg = map[string]bool{processHomeKey(): launch}
	redactPolicy.Unlock()
	t.Cleanup(func() {
		redactPolicy.Lock()
		redactPolicy.cfg = orig
		redactPolicy.Unlock()
	})
}

func TestRedactingHandlerRedactsMessageAndAttributes(t *testing.T) {
	pinRedactPolicy(t, true)
	var buf bytes.Buffer
	logger := slog.New(NewRedactingHandler(slog.NewTextHandler(&buf, nil)))
	secret := "sk-proj-" + strings.Repeat("a", 30)
	logger.With("bound", secret).Warn("key "+secret, "attr", secret)
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("secret logged: %s", buf.String())
	}
}

func TestRoutedRecordsFollowTheirOwnProfilesRedactionPolicy(t *testing.T) {
	secret := "sk-proj-ROUTEDPROFILE" + strings.Repeat("b", 24)
	routed := t.TempDir()
	cases := []struct {
		name     string
		launch   bool
		record   func()
		redacted bool
	}{
		{name: "unrecorded profile redacts", launch: false, redacted: true},
		{name: "config opt out", launch: true, record: func() { NoteConfigPolicy(home.Key(routed), false) }, redacted: false},
		{name: "config no", launch: true, record: func() { NoteConfigPolicy(home.Key(routed), "no") }, redacted: false},
		{name: "unreadable value keeps redacting", launch: false, record: func() { NoteConfigPolicy(home.Key(routed), "maybe") }, redacted: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pinRedactPolicy(t, tc.launch)
			if tc.record != nil {
				tc.record()
			}
			var buf bytes.Buffer
			logger := slog.New(NewRedactingHandler(slog.NewTextHandler(&buf, nil)))
			logger.WarnContext(home.WithOverride(context.Background(), routed), "provider rejected key "+secret)
			if got := !strings.Contains(buf.String(), secret); got != tc.redacted {
				t.Fatalf("redacted = %v, log %s", got, buf.String())
			}
		})
	}
}

func TestLaunchRecordsFollowTheLaunchPolicy(t *testing.T) {
	secret := "sk-proj-LAUNCHPROFILE" + strings.Repeat("a", 24)
	for _, launch := range []bool{true, false} {
		pinRedactPolicy(t, launch)
		var buf bytes.Buffer
		slog.New(NewRedactingHandler(slog.NewTextHandler(&buf, nil))).Warn("launch key " + secret)
		if got := !strings.Contains(buf.String(), secret); got != launch {
			t.Fatalf("launch %v redacted = %v", launch, got)
		}
	}
}
