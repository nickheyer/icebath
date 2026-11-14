package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Every chunk of a streamer reply and the error that ended it
func collectChunks(seq iter.Seq2[[]byte, error]) ([][]byte, error) {
	var out [][]byte
	for chunk, err := range seq {
		if err != nil {
			return out, err
		}
		out = append(out, chunk)
	}
	return out, nil
}

func streamChunks(t *testing.T, s StreamingTTSProvider, text string) ([][]byte, error) {
	t.Helper()
	seq, err := s.Stream(context.Background(), text)
	if err != nil {
		t.Fatal(err)
	}
	return collectChunks(seq)
}

func TestSpeechInterruptedLatchTakePopsAndReportsRecentBarge(t *testing.T) {
	MarkSpeechInterrupted()
	if !TakeSpeechInterrupted() {
		t.Fatal("recent barge not reported")
	}
	if TakeSpeechInterrupted() {
		t.Fatal("latch must be one shot")
	}
}

func TestSpeechInterruptedLatchStaleBargeExpires(t *testing.T) {
	MarkSpeechInterrupted()
	speechInterruptedMu.Lock()
	stale := *speechInterruptedAt - speechInterruptTTLSeconds - 1
	speechInterruptedAt = &stale
	speechInterruptedMu.Unlock()
	if TakeSpeechInterrupted() {
		t.Fatal("stale barge must expire")
	}
}

// Registers a fake streamer class under a name for the test duration
func registerFakeStreamer(t *testing.T, name string, available bool) {
	t.Helper()
	streamingRegistryMu.Lock()
	old, had := streamingRegistry[name]
	streamingRegistryMu.Unlock()
	RegisterStreamingTTSProvider(name, StreamingTTSClass{
		Available: func(context.Context, *tools.Env) bool { return available },
		New: func(*tools.Env, map[string]any, any) (StreamingTTSProvider, error) {
			return newFakeStreamer(func(*fakeStreamer, string) iter.Seq2[[]byte, error] { return pcmGenerator(nil, []byte("\x00\x00")) }), nil
		},
	})
	t.Cleanup(func() {
		streamingRegistryMu.Lock()
		defer streamingRegistryMu.Unlock()
		if had {
			streamingRegistry[name] = old
		} else {
			delete(streamingRegistry, name)
		}
	})
}

func TestNeverSwapsProviderForStreaming(t *testing.T) {
	registerFakeStreamer(t, "elevenlabs", true)
	got, err := ResolveStreamingProvider(context.Background(), nil, map[string]any{"provider": "edge"}, "")
	if err != nil || got != nil {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestElevenlabsAvailableReflectsKey(t *testing.T) {
	swap(t, &streamResolveKey, func(_ context.Context, _ *tools.Env, envVar, _ string) string {
		if envVar == "ELEVENLABS_API_KEY" {
			return "key"
		}
		return ""
	})
	if !elevenLabsStreamerAvailable(context.Background(), nil) {
		t.Fatal("key present but streamer unavailable")
	}
	streamResolveKey = func(context.Context, *tools.Env, string, string) string { return "" }
	if elevenLabsStreamerAvailable(context.Background(), nil) {
		t.Fatal("no key but streamer available")
	}
}

func TestOpenaiAvailableReflectsAudioKeyResolution(t *testing.T) {
	swap(t, &streamOpenAIConfigAPIKey, func(context.Context, *tools.Env) string { return "" })
	swap(t, &streamResolveOpenAIAudioAPIKey, func(*tools.Env) string { return "voice-key" })
	if !openAIStreamerAvailable(context.Background(), nil) {
		t.Fatal("voice key must make the streamer available")
	}
	streamResolveOpenAIAudioAPIKey = func(*tools.Env) string { return "" }
	if openAIStreamerAvailable(context.Background(), nil) {
		t.Fatal("no key but streamer available")
	}
	streamOpenAIConfigAPIKey = func(context.Context, *tools.Env) string { return "cfg-key" }
	if !openAIStreamerAvailable(context.Background(), nil) {
		t.Fatal("tts.openai.api_key must make the streamer available")
	}
}

type openAIClientArgs struct{ apiKey, baseURL string }

type openAICapture struct {
	mu      sync.Mutex
	clients []openAIClientArgs
	bodies  []map[string]any
	extras  []map[string]any
}

type fakeSpeechStream struct {
	headers http.Header
	chunks  [][]byte
}

func (f *fakeSpeechStream) Headers() http.Header { return f.headers }
func (f *fakeSpeechStream) Close() error         { return nil }
func (f *fakeSpeechStream) IterBytes() iter.Seq2[[]byte, error] {
	return pcmGenerator(nil, f.chunks...)
}

type fakeOpenAIStreamClient struct {
	capture *openAICapture
	headers http.Header
	chunks  [][]byte
}

func (c *fakeOpenAIStreamClient) StreamSpeech(_ context.Context, body, extraBody map[string]any) (openAISpeechStream, error) {
	c.capture.mu.Lock()
	c.capture.bodies = append(c.capture.bodies, body)
	c.capture.extras = append(c.capture.extras, extraBody)
	c.capture.mu.Unlock()
	return &fakeSpeechStream{headers: c.headers, chunks: c.chunks}, nil
}

// Replaces the OpenAI client with one answering the given headers and chunks, a nil chunk list meaning one short frame
func patchOpenAISpeech(t *testing.T, headers map[string]string, chunks [][]byte) *openAICapture {
	t.Helper()
	speakerHome(t)
	t.Setenv("OPENAI_BASE_URL", "")
	var h http.Header
	if headers != nil {
		h = http.Header{}
		for k, v := range headers {
			h.Set(k, v)
		}
	}
	if chunks == nil {
		chunks = [][]byte{pcm("\x01\x00", 100)}
	}
	capture := &openAICapture{}
	swap(t, &newOpenAIStreamClient, func(apiKey, baseURL string) openAIStreamClient {
		capture.mu.Lock()
		capture.clients = append(capture.clients, openAIClientArgs{apiKey, baseURL})
		capture.mu.Unlock()
		return &fakeOpenAIStreamClient{capture: capture, headers: h, chunks: chunks}
	})
	return capture
}

func TestOpenaiStreamerForwardsConsentAttestation(t *testing.T) {
	capture := patchOpenAISpeech(t, nil, [][]byte{[]byte("\x01\x00")})
	swap(t, &streamResolveOpenAIAudioAPIKey, func(*tools.Env) string { return "env-key" })
	section := map[string]any{"api_key": "k", "consent_attestation": "I have consent"}
	s, err := newOpenAIStreamer(nil, map[string]any{"openai": section}, section)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := streamChunks(t, s, "hi"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(capture.extras[0], map[string]any{"consent_attestation": "I have consent"}) {
		t.Fatalf("extra_body = %v", capture.extras[0])
	}
	bare := map[string]any{"api_key": "k"}
	s, err = newOpenAIStreamer(nil, map[string]any{"openai": bare}, bare)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := streamChunks(t, s, "hi"); err != nil {
		t.Fatal(err)
	}
	if capture.extras[1] != nil {
		t.Fatalf("an unset key must add no extra_body, got %v", capture.extras[1])
	}
}

func TestOpenaiStreamerPrefersConfiguredApiKey(t *testing.T) {
	capture := patchOpenAISpeech(t, nil, [][]byte{[]byte("\x01\x00")})
	swap(t, &streamResolveOpenAIAudioAPIKey, func(*tools.Env) string { return "env-key" })
	config := map[string]any{"provider": "openai", "openai": map[string]any{"api_key": "cfg-key", "base_url": "http://local-tts.example/v1"}}
	streamer, err := ResolveStreamingProvider(context.Background(), nil, config, "")
	if err != nil || streamer == nil {
		t.Fatalf("streamer = %v, %v", streamer, err)
	}
	chunks, err := streamChunks(t, streamer, "Streaming test.")
	if err != nil || len(chunks) != 1 || string(chunks[0]) != "\x01\x00" {
		t.Fatalf("chunks = %q, %v", chunks, err)
	}
	if capture.clients[0].apiKey != "cfg-key" {
		t.Fatalf("client api key = %q", capture.clients[0].apiKey)
	}
}

func TestXaiAvailableUsesOauthCredentialResolver(t *testing.T) {
	key := "xai-key"
	swap(t, &ttsResolveXAICredentials, func(*tools.Env) (string, string, string) { return "xai-oauth", key, "" })
	if !xaiStreamerAvailable(context.Background(), nil) {
		t.Fatal("resolved key must make the streamer available")
	}
	key = ""
	if xaiStreamerAvailable(context.Background(), nil) {
		t.Fatal("no key but streamer available")
	}
}

type recordingXAIPool struct {
	mu    sync.Mutex
	token string
	calls int
}

func (p *recordingXAIPool) SelectPooled(*tools.Env, string) (string, string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.token, "", p.token != ""
}

func (p *recordingXAIPool) RefreshMatching(*tools.Env, string, string) (string, string, bool) {
	return "", "", false
}

func TestXaiStreamingPrefersExplicitApiKey(t *testing.T) {
	speakerHome(t)
	pool := &recordingXAIPool{token: "oauth-bearer"}
	swap(t, &tools.CredentialPools, tools.CredentialPoolSource(pool))
	t.Setenv("XAI_API_KEY", "k")
	if !xaiStreamerAvailable(context.Background(), nil) {
		t.Fatal("explicit key must make the streamer available")
	}
	if key, err := xaiStreamAPIKey(nil); err != nil || key != "k" {
		t.Fatalf("key = %q, %v", key, err)
	}
	if !xaiRequirements(nil) {
		t.Fatal("availability probe must resolve the explicit key")
	}
	if pool.calls != 0 {
		t.Fatalf("an explicit key must win before the OAuth pool is spent, pool calls = %d", pool.calls)
	}
	t.Setenv("XAI_API_KEY", "")
	pool.token = ""
	streamer, err := newXAIStreamer(nil, map[string]any{}, map[string]any{"voice_id": "v"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = streamChunks(t, streamer, "hi")
	if err == nil || !strings.Contains(err.Error(), "No xAI credentials") {
		t.Fatalf("err = %v", err)
	}
	if pool.calls == 0 {
		t.Fatal("the pool is consulted only after the explicit key lookup came back empty")
	}
}

func TestGeminiStreamerDecodesSsePcmChunks(t *testing.T) {
	speakerHome(t)
	t.Setenv("GEMINI_BASE_URL", "")
	pcm1, pcm2 := pcm("\x01\x00", 40), pcm("\x02\x00", 40)
	event := func(audio []byte) string {
		raw, _ := json.Marshal(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{
			map[string]any{"inlineData": map[string]any{"data": base64.StdEncoding.EncodeToString(audio)}}}}}}})
		return "data: " + string(raw)
	}
	var gotURL string
	var gotHeaders map[string]string
	swap(t, &ttsPostJSON, func(_ context.Context, rawURL string, _ any, headers map[string]string) (*http.Response, error) {
		gotURL, gotHeaders = rawURL, headers
		body := event(pcm1) + "\n\n: heartbeat\n" + event(pcm2) + "\n"
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: http.Header{"Content-Type": {"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	swap(t, &streamResolveKey, func(context.Context, *tools.Env, string, string) string { return "g-key" })
	streamer, err := newGeminiStreamer(nil, map[string]any{}, map[string]any{"voice": "Kore"})
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := streamChunks(t, streamer, "Hello there.")
	if err != nil || len(chunks) != 2 || !bytes.Equal(chunks[0], pcm1) || !bytes.Equal(chunks[1], pcm2) {
		t.Fatalf("chunks = %d, %v", len(chunks), err)
	}
	u, err := url.Parse(gotURL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("alt") != "sse" {
		t.Fatalf("url = %s", gotURL)
	}
	if u.Query().Has("key") {
		t.Fatal("the key must ride in the header, never the query string")
	}
	if gotHeaders["x-goog-api-key"] != "g-key" {
		t.Fatalf("headers = %v", gotHeaders)
	}
	if !strings.HasSuffix(u.Path, ":streamGenerateContent") {
		t.Fatalf("Gemini SSE must use the streamed endpoint, path = %s", u.Path)
	}
}

// Serves the handler on a loopback WebSocket server and returns its ws URL
func fakeXAIServer(t *testing.T, handler func(ws *websocket.Conn, r *http.Request)) string {
	t.Helper()
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		handler(ws, r)
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/tts"
}

func audioDelta(audio []byte) []byte {
	raw, _ := json.Marshal(map[string]any{"type": "audio.delta", "delta": base64.StdEncoding.EncodeToString(audio)})
	return raw
}

func TestXaiStreamPreservesProfileAndDeliversBeforeCompletion(t *testing.T) {
	for _, profile := range []string{"alpha", "beta"} {
		t.Run(profile, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), profile)
			if err := os.Mkdir(home, 0o755); err != nil {
				t.Fatal(err)
			}
			key := "test-" + profile + "-key"
			if err := os.WriteFile(filepath.Join(home, ".env"), []byte("XAI_API_KEY="+key+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ICEBATH_HOME", home)
			t.Setenv("XAI_API_KEY", "test-process-key")
			firstDelivered := &pythread.Event{}
			defer firstDelivered.Set()
			var mu sync.Mutex
			var seenPath, seenAuth string
			var seenMessages []map[string]any
			audio := pcm("\x01\x00", 30)
			wsURL := fakeXAIServer(t, func(ws *websocket.Conn, r *http.Request) {
				mu.Lock()
				seenPath, seenAuth = r.URL.RequestURI(), r.Header.Get("Authorization")
				mu.Unlock()
				for range 2 {
					_, data, err := ws.ReadMessage()
					if err != nil {
						return
					}
					var msg map[string]any
					_ = json.Unmarshal(data, &msg)
					mu.Lock()
					seenMessages = append(seenMessages, msg)
					mu.Unlock()
				}
				_ = ws.WriteMessage(websocket.TextMessage, audioDelta(audio))
				if !firstDelivered.Wait(10) {
					return
				}
				_ = ws.WriteMessage(websocket.TextMessage, audioDelta(audio))
				_ = ws.WriteMessage(websocket.TextMessage, []byte(`{"type": "audio.done"}`))
			})
			env := tools.EnvForProfileHome(context.Background(), nil, home)
			section := map[string]any{"streaming_url": wsURL, "voice_id": "test-voice", "language": "test-language"}
			streamer, err := newXAIStreamer(env, map[string]any{}, section)
			if err != nil {
				t.Fatal(err)
			}
			seq, err := streamer.Stream(context.Background(), "A sentence.")
			if err != nil {
				t.Fatal(err)
			}
			next, stopPull := iter.Pull2(seq)
			defer stopPull()
			first, err, ok := next()
			if !ok || err != nil || !bytes.Equal(first, audio) {
				t.Fatalf("first chunk = %q, %v, %v", first, err, ok)
			}
			mu.Lock()
			auth := seenAuth
			mu.Unlock()
			if auth != "Bearer "+key {
				t.Fatalf("auth = %q", auth)
			}
			firstDelivered.Set()
			var rest [][]byte
			for {
				chunk, err, ok := next()
				if !ok {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				rest = append(rest, chunk)
			}
			if len(rest) != 1 || !bytes.Equal(rest[0], audio) {
				t.Fatalf("rest = %q", rest)
			}
			u, err := url.Parse(seenPath)
			if err != nil {
				t.Fatal(err)
			}
			params := u.Query()
			want := url.Values{"voice": {"test-voice"}, "language": {"test-language"}, "sample_rate": {"24000"}, "codec": {"pcm"}}
			if !reflect.DeepEqual(params, want) || streamer.SampleRate() != 24000 {
				t.Fatalf("params = %v", params)
			}
			wantMessages := []map[string]any{{"type": "text.delta", "delta": "A sentence."}, {"type": "text.done"}}
			if !reflect.DeepEqual(seenMessages, wantMessages) {
				t.Fatalf("messages = %v", seenMessages)
			}
		})
	}
}

func TestXaiStreamReportsErrorsAndBoundsReceivedAudio(t *testing.T) {
	for _, outcome := range []string{"error", "byte-cap"} {
		t.Run(outcome, func(t *testing.T) {
			swap(t, &ttsResolveXAICredentials, func(*tools.Env) (string, string, string) { return "xai", "test-key", "" })
			swap(t, &streamSentenceByteCap, 100)
			closed := &pythread.Event{}
			wsURL := fakeXAIServer(t, func(ws *websocket.Conn, _ *http.Request) {
				for range 2 {
					if _, _, err := ws.ReadMessage(); err != nil {
						return
					}
				}
				if outcome == "error" {
					_ = ws.WriteMessage(websocket.TextMessage, []byte(`{"type": "error", "message": "example failure"}`))
					return
				}
				go func() {
					for {
						if _, _, err := ws.ReadMessage(); err != nil {
							return
						}
					}
				}()
				for {
					if err := ws.WriteMessage(websocket.TextMessage, audioDelta(bytes.Repeat([]byte("x"), 64))); err != nil {
						closed.Set()
						return
					}
				}
			})
			streamer, err := newXAIStreamer(nil, map[string]any{}, map[string]any{"streaming_url": wsURL})
			if err != nil {
				t.Fatal(err)
			}
			chunks, err := streamChunks(t, streamer, "A sentence.")
			if outcome == "error" {
				if err == nil || !strings.Contains(err.Error(), "example failure") {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil || len(chunks) != 1 || !bytes.Equal(chunks[0], bytes.Repeat([]byte("x"), 64)) {
				t.Fatalf("chunks = %q, %v", chunks, err)
			}
			if !closed.Wait(10) {
				t.Fatal("upstream was not closed after the byte ceiling")
			}
		})
	}
}

func TestStreamCapTruncatesRunawayUpstream(t *testing.T) {
	swap(t, &streamSentenceByteCap, 100)
	endless := func(yield func([]byte, error) bool) {
		for yield(make([]byte, 64), nil) {
		}
	}
	out, err := collectChunks(cappedChunks(endless, "test"))
	if err != nil || len(out) != 1 {
		t.Fatalf("out = %d chunks, %v", len(out), err)
	}
	total := 0
	for _, c := range out {
		total += len(c)
	}
	if total > 100 {
		t.Fatalf("total = %d", total)
	}
}

func TestOpenaiPcmSampleRateResolution(t *testing.T) {
	cases := []struct {
		config   map[string]any
		headers  map[string]string
		expected int
		ok       bool
	}{
		{map[string]any{"pcm_sample_rate": "22050"}, nil, 22050, true},
		{map[string]any{"pcm_sample_rate": "bogus"}, nil, 24000, true},
		{nil, map[string]string{"content-type": "audio/pcm", "x-audio-sample-rate": "44100"}, 44100, true},
		{nil, map[string]string{"content-type": "audio/L16; rate=16000"}, 16000, true},
		{nil, map[string]string{"content-type": "audio/pcm"}, 0, false},
	}
	for _, c := range cases {
		if c.headers != nil {
			h := http.Header{}
			for k, v := range c.headers {
				h.Set(k, v)
			}
			if rate, ok := sampleRateFromHeaders(h); rate != c.expected || ok != c.ok {
				t.Fatalf("headers %v: rate = %d, %v", c.headers, rate, ok)
			}
			continue
		}
		section := map[string]any{"api_key": "sk-x"}
		for k, v := range c.config {
			section[k] = v
		}
		s, err := newOpenAIStreamer(nil, map[string]any{}, section)
		if err != nil || s.SampleRate() != c.expected {
			t.Fatalf("config %v: rate = %v, %v", c.config, s, err)
		}
	}
}

type pcmPlugin struct {
	*fakeTTSProvider
	streamsPCM bool
	rate       any
	available  bool
	mu         sync.Mutex
	calls      []TTSOptions
}

func newPCMPlugin(streamsPCM bool, rate any, available bool) *pcmPlugin {
	return &pcmPlugin{fakeTTSProvider: newFakeTTSProvider("fake-pcm"), streamsPCM: streamsPCM, rate: rate, available: available}
}

func (p *pcmPlugin) IsAvailable() bool     { return p.available }
func (p *pcmPlugin) StreamsPCM() bool      { return p.streamsPCM }
func (p *pcmPlugin) StreamSampleRate() any { return p.rate }
func (p *pcmPlugin) Synthesize(context.Context, string, string, TTSOptions) (string, error) {
	panic("synthesize() on the streaming path")
}
func (p *pcmPlugin) Stream(_ context.Context, _ string, opts TTSOptions) (iter.Seq2[[]byte, error], error) {
	return pcmGenerator(func() {
		p.mu.Lock()
		p.calls = append(p.calls, opts)
		p.mu.Unlock()
	}, pcm("\x01\x02", 4)), nil
}

func registerPCMPlugin(t *testing.T, p *pcmPlugin) {
	t.Helper()
	speakerHome(t)
	resetTTSRegistry(t)
	swap(t, &ttsEnsurePluginsDiscovered, func(context.Context, bool) {})
	if err := RegisterTTSProvider(p, ""); err != nil {
		t.Fatal(err)
	}
}

func TestOptedInPluginStreamsPcmInTheConfiguredVoice(t *testing.T) {
	for _, streaming := range []map[string]any{{}, {"provider": "auto"}, {"provider": "fake-pcm"}} {
		swap(t, &tryInstantiate, func(context.Context, *tools.Env, string, map[string]any) StreamingTTSProvider { return nil })
		plugin := newPCMPlugin(true, 22050, true)
		registerPCMPlugin(t, plugin)
		cfg := map[string]any{"provider": "fake-pcm", "voice": "v1", "speed": 1.25, "streaming": streaming}
		streamer, err := ResolveStreamingProvider(context.Background(), nil, cfg, "")
		if err != nil || streamer == nil {
			t.Fatalf("streaming %v: streamer = %v, %v", streaming, streamer, err)
		}
		if streamer.SampleRate() != 22050 {
			t.Fatalf("sample rate = %d", streamer.SampleRate())
		}
		chunks, err := streamChunks(t, streamer, "Hi.")
		if err != nil || len(chunks) != 1 || !bytes.Equal(chunks[0], pcm("\x01\x02", 4)) {
			t.Fatalf("chunks = %q, %v", chunks, err)
		}
		if len(plugin.calls) != 1 {
			t.Fatalf("calls = %d", len(plugin.calls))
		}
		call := plugin.calls[0]
		if call.Format != "pcm" || call.Voice == nil || *call.Voice != "v1" || call.Model != nil || call.Speed == nil || *call.Speed != 1.25 || call.Extra != nil {
			t.Fatalf("call = %+v", call)
		}
	}
}

func TestPluginWithoutFullOptInKeepsPerSentenceSynthesis(t *testing.T) {
	command := map[string]any{"providers": map[string]any{"fake-pcm": map[string]any{"type": "command", "command": "say {input_path}"}}}
	cases := []struct {
		plugin *pcmPlugin
		extra  map[string]any
	}{
		{newPCMPlugin(false, 22050, true), nil},
		{newPCMPlugin(true, nil, true), nil},
		{newPCMPlugin(true, 0, true), nil},
		{newPCMPlugin(true, 0.5, true), nil},
		{newPCMPlugin(true, math.NaN(), true), nil},
		{newPCMPlugin(true, 22050, false), nil},
		{newPCMPlugin(true, 22050, true), command},
	}
	for i, c := range cases {
		registerPCMPlugin(t, c.plugin)
		cfg := map[string]any{"provider": "fake-pcm"}
		for k, v := range c.extra {
			cfg[k] = v
		}
		streamer, err := ResolveStreamingProvider(context.Background(), nil, cfg, "")
		if err != nil || streamer != nil {
			t.Fatalf("case %d: streamer = %v, %v", i, streamer, err)
		}
		if len(c.plugin.calls) != 0 {
			t.Fatalf("case %d: calls = %v", i, c.plugin.calls)
		}
	}
}

func TestXaiStreamSocketClosures(t *testing.T) {
	oversizeClose := make(chan string, 1)
	cases := []struct {
		name    string
		serve   func(ws *websocket.Conn)
		wantErr string
	}{
		{"oversize frame", func(ws *websocket.Conn) {
			_ = ws.WriteMessage(websocket.TextMessage, bytes.Repeat([]byte("a"), 1<<20+10))
			_, _, err := ws.ReadMessage()
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				oversizeClose <- closeErr.Text
			}
		}, "xAI WS closed mid-stream: sent 1009 (message too big) frame with 1048586 bytes exceeds limit of 1048576 bytes; no close frame received"},
		{"dropped connection", func(ws *websocket.Conn) { _ = ws.NetConn().Close() },
			"xAI WS closed mid-stream: no close frame received or sent"},
		{"error close frame", func(ws *websocket.Conn) {
			_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "boom"), time.Now().Add(time.Second))
			_, _, _ = ws.ReadMessage()
		}, "xAI WS closed mid-stream: received 1011 (internal error) boom; then sent 1011 (internal error) boom"},
		{"clean close without audio.done", func(*websocket.Conn) {}, ""},
		{"invalid utf-8 text", func(ws *websocket.Conn) {
			_ = ws.WriteMessage(websocket.TextMessage, []byte("\xff{}"))
			_, _, _ = ws.ReadMessage()
		}, "xAI WS closed mid-stream: sent 1007 (invalid frame payload data) invalid start byte at position 0; no close frame received"},
		{"non mapping envelope", func(ws *websocket.Conn) {
			_ = ws.WriteMessage(websocket.TextMessage, []byte("[1]"))
			_, _, _ = ws.ReadMessage()
		}, "'list' object has no attribute 'get'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			swap(t, &ttsResolveXAICredentials, func(*tools.Env) (string, string, string) { return "xai", "test-key", "" })
			wsURL := fakeXAIServer(t, func(ws *websocket.Conn, _ *http.Request) {
				for range 2 {
					if _, _, err := ws.ReadMessage(); err != nil {
						return
					}
				}
				c.serve(ws)
			})
			streamer, err := newXAIStreamer(nil, map[string]any{}, map[string]any{"streaming_url": wsURL})
			if err != nil {
				t.Fatal(err)
			}
			chunks, err := streamChunks(t, streamer, "A sentence.")
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != c.wantErr || len(chunks) != 0 {
				t.Fatalf("err = %q, chunks = %d", got, len(chunks))
			}
			if c.name == "oversize frame" {
				select {
				case reason := <-oversizeClose:
					if reason != "frame with 1048586 bytes exceeds limit of 1048576 bytes" {
						t.Fatalf("close reason = %q", reason)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("the server never received the 1009 close frame")
				}
			}
		})
	}
}

func TestGeminiSseLineDecoding(t *testing.T) {
	split := strings.Repeat("a", 511) + "é\nnext"
	cases := []struct {
		contentType string
		body        string
		want        []string
		wantErr     string
	}{
		{"text/event-stream", "data: a\r\n\r\ndata: b", []string{"data: a", "", "data: b"}, ""},
		{"text/event-stream", "caf\xe9\x85tail\n", []string{"café", "tail"}, ""},
		{"application/json", split, []string{strings.Repeat("a", 511) + "é", "next"}, ""},
		{"application/octet-stream; charset=\"utf-8\"", "x y", []string{"x", "y"}, ""},
		{"", "\n\ndata: x\n", nil, "startswith first arg must be bytes or a tuple of bytes, not str"},
		{"", "\r\n\n", nil, ""},
		{"text/plain; charset=bogus", "x", nil, "unknown encoding: bogus"},
		{"text/plain; charset", "x", nil, "'bool' object has no attribute 'strip'"},
	}
	for i, c := range cases {
		resp := &http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(c.body))}
		if c.contentType != "" {
			resp.Header.Set("Content-Type", c.contentType)
		}
		var got []string
		gotErr := ""
		for line, err := range responseLines(resp) {
			if err != nil {
				gotErr = err.Error()
				break
			}
			got = append(got, line)
		}
		if !reflect.DeepEqual(got, c.want) || gotErr != c.wantErr {
			t.Fatalf("case %d: lines = %q, err = %q", i, got, gotErr)
		}
	}
}
