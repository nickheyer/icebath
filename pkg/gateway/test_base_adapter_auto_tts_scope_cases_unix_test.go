//go:build !windows

package gateway

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
)

func TestAutoTTSUnderMultiplexSpeaksThroughTheServedProfilesCommandProviderToken(t *testing.T) {
	served := profileEnvServedHome(t, profileEnvLaunchHome(t))
	script := filepath.Join(t.TempDir(), "speak.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s' \"$SKILL_TOKEN\" > \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	envRefsWrite(t, served, "config.yaml", "tts:\n  provider: srv\n  providers:\n    srv:\n      type: command\n"+
		"      command: \"sh "+pyshlex.Quote(script)+" {output_path}\"\n      env_passthrough: [SKILL_TOKEN]\n")
	a := newTurnAdapter(PlatformSlack)
	paths, requested := a.synthesizeAutoTTS(ProfileRuntimeScope(context.Background(), served, true), "hello there")
	if requested == nil || len(paths) != 1 || paths[0] != *requested {
		t.Fatalf("auto tts paths %v requested %v", paths, requested)
	}
	t.Cleanup(func() { os.Remove(*requested) })
	if spoken, err := os.ReadFile(paths[0]); err != nil || string(spoken) != "skill-secret" {
		t.Fatalf("command provider saw %q %v", spoken, err)
	}
}
