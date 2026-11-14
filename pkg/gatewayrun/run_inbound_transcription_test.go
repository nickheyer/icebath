package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/media"
)

// Plugin speech to text provider answering through a test supplied function
type fakeSTTProvider struct {
	media.TranscriptionProviderDefaults
	mu    sync.Mutex
	calls []string
	fn    func(path string) map[string]any
}

func (p *fakeSTTProvider) Name() string      { return "mock" }
func (p *fakeSTTProvider) IsAvailable() bool { return true }
func (p *fakeSTTProvider) Transcribe(_ context.Context, path string, _ media.TranscribeOptions) (any, error) {
	p.mu.Lock()
	p.calls = append(p.calls, path)
	p.mu.Unlock()
	return p.fn(path), nil
}

// Paths the provider was asked to transcribe
func (p *fakeSTTProvider) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

// Home whose speech to text provider is the registered mock plugin answering through fn
func installMockSTT(t *testing.T, fn func(path string) map[string]any) *fakeSTTProvider {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("stt:\n  enabled: true\n  provider: mock\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &fakeSTTProvider{fn: fn}
	p.TranscriptionProviderDefaults = media.NewTranscriptionProviderDefaults(p)
	if err := media.RegisterTranscriptionProvider(p, ""); err != nil {
		t.Fatal(err)
	}
	return p
}

// Voice clip file under a temp directory
func voiceClip(t *testing.T, dir, name string) string {
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("OggS voice"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Bare runner with speech to text on and the Telegram adapter when given
func voiceRunner(adapter gateway.PlatformAdapter) *GatewayRunner {
	r := allocGatewayRunner()
	r.config = gateway.DefaultGatewayConfig(context.Background())
	r.config.STTEnabled = true
	r.adapters = adapterDict()
	if adapter != nil {
		r.adapters.Set(gateway.PlatformTelegram, adapter)
	}
	return r
}

func voiceSource() *gateway.SessionSource {
	return &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "12345", ChatType: "dm"}
}

func voiceEvent(source *gateway.SessionSource, urls []string, text string) *gateway.MessageEvent {
	types := make([]string, len(urls))
	for i := range types {
		types[i] = "audio/ogg"
	}
	return &gateway.MessageEvent{Text: text, MessageType: gateway.MessageTypeVoice, Source: source,
		MediaURLs: append([]string(nil), urls...), MediaTypes: types, Metadata: map[string]any{}}
}

// Transcript naming the clip file, the shape the Python fakes return
func clipNameTranscript(path string) map[string]any {
	return map[string]any{"success": true, "transcript": "<" + filepath.Base(path) + ">", "provider": "mock"}
}

func TestEnrichMessageWithTranscriptionEmptyContent(t *testing.T) {
	t.Run("test_enrich_message_with_transcription_returns_tuple_for_empty_content_placeholder", func(t *testing.T) {
		installMockSTT(t, func(string) map[string]any {
			return map[string]any{"success": true, "transcript": "hello from a captionless voice note", "provider": "local_command"}
		})
		runner := voiceRunner(nil)
		clip := voiceClip(t, t.TempDir(), "voice.ogg")
		result, transcripts := runner.enrichMessageWithTranscription(context.Background(), "(The user sent a message with no text content)", []string{clip}, nil)
		if !strings.Contains(result, "hello from a captionless voice note") || strings.Contains(result, "(The user sent a message with no text content)") {
			t.Fatalf("result %q", result)
		}
		if !reflect.DeepEqual(transcripts, []string{"hello from a captionless voice note"}) {
			t.Fatalf("transcripts %v", transcripts)
		}
	})
}

func TestEnrichMessageWithTranscriptionGuardsEmptyTranscript(t *testing.T) {
	installMockSTT(t, func(string) map[string]any {
		return map[string]any{"success": true, "transcript": "   \n\t", "provider": "local_command"}
	})
	runner := voiceRunner(nil)
	clip := voiceClip(t, t.TempDir(), "voice.ogg")
	result, transcripts := runner.enrichMessageWithTranscription(context.Background(), "caption", []string{clip}, nil)
	if !strings.Contains(result, "empty or inaudible") || strings.Contains(result, `""`) {
		t.Fatalf("result %q", result)
	}
	if len(transcripts) != 0 {
		t.Fatalf("transcripts %v", transcripts)
	}
}

func TestPendingVoiceInterruptReusesTranscriptAndEcho(t *testing.T) {
	stt := installMockSTT(t, func(string) map[string]any {
		return map[string]any{"success": true, "transcript": "hello once", "provider": "mock"}
	})
	ctx := context.Background()
	adapter := newRestartTestAdapter()
	runner := voiceRunner(adapter)
	source := voiceSource()
	clip := voiceClip(t, t.TempDir(), "telegram-voice.ogg")
	event := voiceEvent(source, []string{clip}, "")

	caption := event.Text
	interruptText, interruptTranscripts := runner.transcribePendingAudioEventOnce(ctx, event, &caption)
	runner.echoPendingSTTTranscriptsOnce(ctx, event, adapter, source, interruptTranscripts, nil, "Transcript")
	caption = event.Text
	drainText, drainTranscripts := runner.transcribePendingAudioEventOnce(ctx, event, &caption)
	runner.echoPendingSTTTranscriptsOnce(ctx, event, adapter, source, drainTranscripts, nil, "Transcript")

	if interruptText == nil || *interruptText != `"hello once"` || drainText == nil || *drainText != *interruptText {
		t.Fatalf("texts %v %v", interruptText, drainText)
	}
	if !reflect.DeepEqual(drainTranscripts, []string{"hello once"}) || !reflect.DeepEqual(interruptTranscripts, drainTranscripts) {
		t.Fatalf("transcripts %v %v", interruptTranscripts, drainTranscripts)
	}
	if calls := stt.seen(); !reflect.DeepEqual(calls, []string{clip}) {
		t.Fatalf("transcribe calls %v", calls)
	}
	calls := adapter.calls()
	if len(calls) != 1 || calls[0].chatID != "12345" || calls[0].content != "\U0001f399️ \"hello once\"" || calls[0].metadata != nil {
		t.Fatalf("sends %+v", calls)
	}
}

func TestVoiceNoteMergedDuringPendingSttIsTranscribed(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	installMockSTT(t, func(path string) map[string]any {
		once.Do(func() { close(started); <-release })
		return clipNameTranscript(path)
	})
	ctx := context.Background()
	runner := voiceRunner(nil)
	source := voiceSource()
	dir := t.TempDir()
	a, b := voiceClip(t, dir, "A.ogg"), voiceClip(t, dir, "B.ogg")
	pending := map[string]*gateway.MessageEvent{}
	gateway.MergePendingMessageEvent(pending, "k", voiceEvent(source, []string{a}, ""), false)
	event := pending["k"]

	empty := ""
	done := make(chan struct{})
	go func() {
		runner.transcribePendingAudioEventOnce(ctx, event, &empty)
		close(done)
	}()
	<-started
	gateway.MergePendingMessageEvent(pending, "k", voiceEvent(source, []string{b}, ""), false)
	close(release)
	<-done

	drainText, drainTranscripts := runner.transcribePendingAudioEventOnce(ctx, event, &empty)
	if !reflect.DeepEqual(drainTranscripts, []string{"<A.ogg>", "<B.ogg>"}) {
		t.Fatalf("transcripts %v", drainTranscripts)
	}
	if drainText == nil || !strings.Contains(*drainText, "<B.ogg>") {
		t.Fatalf("text %v", drainText)
	}
}

func TestCaptionOfVoiceNoteMergedDuringPendingSttIsKept(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	installMockSTT(t, func(path string) map[string]any {
		once.Do(func() { close(started); <-release })
		return clipNameTranscript(path)
	})
	ctx := context.Background()
	runner := voiceRunner(nil)
	source := voiceSource()
	dir := t.TempDir()
	a, b := voiceClip(t, dir, "A.ogg"), voiceClip(t, dir, "B.ogg")
	pending := map[string]*gateway.MessageEvent{}
	gateway.MergePendingMessageEvent(pending, "k", voiceEvent(source, []string{a}, "first caption"), false)
	event := pending["k"]

	var interruptText string
	done := make(chan struct{})
	fallback := event.Text
	go func() {
		interruptText, _ = runner.transcribeAndEchoPendingVoice(ctx, event, nil, source, fallback, "Voice-priority-interrupt", nil, false)
		close(done)
	}()
	<-started
	gateway.MergePendingMessageEvent(pending, "k", voiceEvent(source, []string{b}, "second caption"), false)
	close(release)
	<-done

	drainText, _ := runner.transcribeAndEchoPendingVoice(ctx, event, nil, source, event.Text, "Voice-drain", nil, false)
	for _, text := range []string{interruptText, drainText} {
		for _, want := range []string{"<B.ogg>", "first caption", "second caption"} {
			if !strings.Contains(text, want) {
				t.Fatalf("%q missing from %q", want, text)
			}
		}
	}
}

func TestBurstOfMergedVoiceNotesTranscribesEachClipOnce(t *testing.T) {
	ctx := context.Background()
	runner := voiceRunner(nil)
	source := voiceSource()
	dir := t.TempDir()
	clips := map[string]string{}
	for _, name := range []string{"A.ogg", "B.ogg", "C.ogg", "D.ogg", "E.ogg"} {
		clips[name] = voiceClip(t, dir, name)
	}
	pending := map[string]*gateway.MessageEvent{}
	burst := []string{clips["B.ogg"], clips["C.ogg"], clips["D.ogg"]}
	var calls []string
	stt := installMockSTT(t, func(path string) map[string]any {
		calls = append(calls, filepath.Base(path))
		if len(burst) > 0 {
			next := burst[0]
			burst = burst[1:]
			gateway.MergePendingMessageEvent(pending, "k", voiceEvent(source, []string{next}, ""), false)
		}
		return clipNameTranscript(path)
	})
	gateway.MergePendingMessageEvent(pending, "k", voiceEvent(source, []string{clips["A.ogg"]}, ""), false)
	event := pending["k"]

	_, transcripts := runner.transcribePendingAudioEventOnce(ctx, event, nil)
	if !reflect.DeepEqual(transcripts, []string{"<A.ogg>", "<B.ogg>", "<C.ogg>", "<D.ogg>"}) {
		t.Fatalf("transcripts %v", transcripts)
	}
	gateway.MergePendingMessageEvent(pending, "k", voiceEvent(source, []string{clips["E.ogg"]}, ""), false)
	_, drainTranscripts := runner.transcribePendingAudioEventOnce(ctx, event, nil)
	if !reflect.DeepEqual(drainTranscripts, []string{"<A.ogg>", "<B.ogg>", "<C.ogg>", "<D.ogg>", "<E.ogg>"}) {
		t.Fatalf("drain transcripts %v", drainTranscripts)
	}
	if !reflect.DeepEqual(calls, []string{"A.ogg", "B.ogg", "C.ogg", "D.ogg", "E.ogg"}) || len(stt.seen()) != 5 {
		t.Fatalf("calls %v", calls)
	}
}
