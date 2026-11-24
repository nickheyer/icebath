//go:build !windows

package media

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Context of a multiplexed secondary profile under the launch home, each env file holding its own token value
func scopedProviderProfile(t *testing.T, name, servedConfig string) (context.Context, string) {
	t.Helper()
	launch := filepath.Join(t.TempDir(), "launch")
	served := filepath.Join(launch, "profiles", "beta")
	if err := os.MkdirAll(served, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", launch)
	for path, text := range map[string]string{filepath.Join(launch, ".env"): name + "=launch-token\n",
		filepath.Join(served, ".env"): name + "=served-token\n", filepath.Join(served, "config.yaml"): servedConfig} {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(name, "launch-token")
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	ctx := home.WithOverride(context.Background(), served)
	return provider.BindProfileSecretScope(ctx, served), served
}

// Script writing the value of the variable into the file named by its first argument
func tokenWriterScript(t *testing.T, name string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "write-token.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s' \"$"+name+"\" > \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestTextToSpeechToolUnderMultiplexRunsTheCommandProviderWithTheScopedProfilesToken(t *testing.T) {
	script := tokenWriterScript(t, "MY_TTS_TOKEN")
	ctx, _ := scopedProviderProfile(t, "MY_TTS_TOKEN", "tts:\n  provider: srv\n  providers:\n    srv:\n      type: command\n"+
		"      command: \"sh "+pyshlex.Quote(script)+" {output_path}\"\n      env_passthrough: [MY_TTS_TOKEN]\n")
	out := filepath.Join(t.TempDir(), "reply.mp3")
	raw, err := TextToSpeechTool(ctx, "hello there", out)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(raw), &result); err != nil || result["success"] != true {
		t.Fatalf("tts result %s %v", raw, err)
	}
	if spoken, err := os.ReadFile(out); err != nil || string(spoken) != "served-token" {
		t.Fatalf("command provider saw %q %v", spoken, err)
	}
}

func TestCheckTTSRequirementsUnderMultiplexSeesTheScopedProfilesProviderKey(t *testing.T) {
	ctx, _ := scopedProviderProfile(t, "ELEVENLABS_API_KEY", "tts:\n  provider: elevenlabs\n")
	t.Setenv("ELEVENLABS_API_KEY", "")
	os.Unsetenv("ELEVENLABS_API_KEY")
	if !CheckTTSRequirements(ctx) {
		t.Fatal("the served profile's own ElevenLabs key did not satisfy the TTS requirement check")
	}
}

func TestCommandSTTUnderMultiplexRunsWithTheScopedProfilesToken(t *testing.T) {
	keepSTTSeams(t)
	script := tokenWriterScript(t, "MY_STT_TOKEN")
	scoped, _ := scopedProviderProfile(t, "MY_STT_TOKEN", "stt:\n  provider: probe\n")
	audio := filepath.Join(t.TempDir(), "audio.wav")
	if err := os.WriteFile(audio, []byte("fixture audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := commandSTTDefaults
	p.Name, p.Command, p.Timeout, p.EnvPassthrough = "probe", "sh "+pyshlex.Quote(script)+" {output_path}", 5*time.Second, []string{"MY_STT_TOKEN"}
	r := transcribeCommandSTT(scoped, audio, &p, loadSTTRequest(scoped), "", "", "")
	if r["success"] != true || r["transcript"] != "served-token" {
		t.Fatalf("command stt result %v", r)
	}
}

func TestCommandProviderOfAToolEnvWithoutSecretsResolvesThroughTheContextScope(t *testing.T) {
	ctx, served := scopedProviderProfile(t, "MY_TTS_TOKEN", "")
	prev := tools.ProfileSecrets
	tools.ProfileSecrets = provider.ProfileSecretSource()
	t.Cleanup(func() { tools.ProfileSecrets = prev })
	res, err := runCommandProvider(ctx, &tools.Env{Home: served, Config: config.Tree{}}, `printf '%s' "$MY_TTS_TOKEN"`, 30, []string{"MY_TTS_TOKEN"})
	if err != nil {
		t.Fatalf("command provider under the served scope failed: %v", err)
	}
	if res.Stdout != "served-token" {
		t.Fatalf("command provider saw MY_TTS_TOKEN %q, want the served scope's served-token", res.Stdout)
	}
}
