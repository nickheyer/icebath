package media

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

type collisionSTT struct {
	provider.CatalogProviderDefaults
	name string
}

func (p *collisionSTT) Name() string { return p.name }
func (p *collisionSTT) Transcribe(context.Context, string, TranscribeOptions) (any, error) {
	return map[string]any{"success": true, "transcript": "plugin"}, nil
}
func (p *collisionSTT) StreamingCapable() bool { return false }
func (p *collisionSTT) OpenStreamSession(context.Context, any, any) (TranscriptionStreamSession, error) {
	return nil, nil
}

func TestTranscriptionPluginTakingABuiltinNameIsIgnoredWithAWarning(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	shadow := &collisionSTT{name: " OpenAI "}
	shadow.Self = shadow
	if err := RegisterTranscriptionProvider(shadow, ""); err != nil {
		t.Fatal(err)
	}
	want := "Transcription provider 'openai' shadows a built-in name; registration ignored. " +
		"Built-in STT providers (deepinfra, elevenlabs, groq, local, local_command, mistral, openai, xai) always win — pick a different name."
	if !strings.Contains(logs.String(), want) || !strings.Contains(logs.String(), "logger=agent.transcription_registry") {
		t.Fatalf("collision log %q", logs.String())
	}
	if got := GetTranscriptionProvider(context.Background(), "openai", ""); got != nil {
		t.Fatalf("shadowing provider was registered as %v", got.Name())
	}

	plugin := &collisionSTT{name: "Whisper-Plugin"}
	plugin.Self = plugin
	logs.Reset()
	if err := RegisterTranscriptionProvider(plugin, ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "shadows a built-in name") {
		t.Fatalf("a free name warned %q", logs.String())
	}
	if got := GetTranscriptionProvider(context.Background(), "whisper-plugin", ""); got != plugin {
		t.Fatalf("free name lookup gave %v", got)
	}
}
