package tools

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Resets recorded profile policy and pins the launch switch for one test
func pinRedactPolicy(t *testing.T, launch bool) {
	t.Helper()
	origLaunch := launchRedactEnabled
	launchRedactEnabled = func() bool { return launch }
	redactPolicy.Lock()
	origEnv, origCfg := redactPolicy.env, redactPolicy.cfg
	redactPolicy.env, redactPolicy.cfg = map[string]*string{}, map[string]*string{}
	redactPolicy.Unlock()
	t.Cleanup(func() {
		launchRedactEnabled = origLaunch
		redactPolicy.Lock()
		redactPolicy.env, redactPolicy.cfg = origEnv, origCfg
		redactPolicy.Unlock()
	})
}

func TestRedactingHandlerRedactsMessageAndAttributes(t *testing.T) {
	pinRedactPolicy(t, true)
	var buf bytes.Buffer
	logger := slog.New(NewRedactingHandler(slog.NewTextHandler(&buf, nil), nil))
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
		scope    map[string]string
		redacted bool
	}{
		{name: "unrecorded profile redacts", launch: false, redacted: true},
		{name: "config opt out", launch: true, record: func() {
			NoteEnvPolicy(home.Key(routed), nil)
			NoteConfigPolicy(home.Key(routed), false)
		}, redacted: false},
		{name: "config opt out before the env is read still redacts", launch: false, record: func() { NoteConfigPolicy(home.Key(routed), false) }, redacted: true},
		{name: "env opt out", launch: true, record: func() { NoteEnvPolicy(home.Key(routed), "false") }, redacted: false},
		{name: "env unset falls to config", launch: true, record: func() {
			NoteEnvPolicy(home.Key(routed), nil)
			NoteConfigPolicy(home.Key(routed), "no")
		}, redacted: false},
		{name: "live scope opt out", launch: true, scope: map[string]string{"ICEBATH_REDACT_SECRETS": "false"}, redacted: false},
		{name: "live scope without the key reads config", launch: false, scope: map[string]string{}, record: func() {
			NoteEnvPolicy(home.Key(routed), "false")
		}, redacted: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pinRedactPolicy(t, tc.launch)
			if tc.record != nil {
				tc.record()
			}
			var buf bytes.Buffer
			scopeOf := func(context.Context) map[string]string { return tc.scope }
			logger := slog.New(NewRedactingHandler(slog.NewTextHandler(&buf, nil), scopeOf))
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
		slog.New(NewRedactingHandler(slog.NewTextHandler(&buf, nil), nil)).Warn("launch key " + secret)
		if got := !strings.Contains(buf.String(), secret); got != launch {
			t.Fatalf("launch %v redacted = %v", launch, got)
		}
	}
}
