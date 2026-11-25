//go:build !icebath_core && !icebath_noextras

package media

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	syncutil "github.com/nickheyer/icebath/pkg/utils/sync"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
)

var (
	streamingProviders = map[string]bool{"openai": true, "xai": true, "elevenlabs": true}
	streamMonotonic    = timeutil.Monotonic
)

// Resampler converts s16le mono PCM between rates across chunk boundaries, averaging integer ratios
type Resampler struct {
	src, dst int
	pos      float64
	carry    []int16
}

// NewResampler resamples from src to dst Hz
func NewResampler(src, dst int) *Resampler { return &Resampler{src: src, dst: dst} }

func pcmInt16(pcm []byte) []int16 {
	out := make([]int16, len(pcm)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(pcm[2*i:]))
	}
	return out
}

// Unbounded queue of PCM chunks where a nil entry is the end of audio sentinel
type audioQueue struct {
	mu    sync.Mutex
	items [][]byte
	ends  []bool
	wake  chan struct{}
}

func newAudioQueue() *audioQueue { return &audioQueue{wake: make(chan struct{}, 1)} }

func (q *audioQueue) put(chunk []byte, end bool) {
	q.mu.Lock()
	q.items = append(q.items, chunk)
	q.ends = append(q.ends, end)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Next chunk, waiting up to timeout when block is set, ok false when the queue stayed empty
func (q *audioQueue) get(block bool, timeout float64) (chunk []byte, end, ok bool) {
	deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			chunk, end = q.items[0], q.ends[0]
			q.items, q.ends = q.items[1:], q.ends[1:]
			q.mu.Unlock()
			return chunk, end, true
		}
		q.mu.Unlock()
		remaining := time.Until(deadline)
		if !block || remaining <= 0 {
			return nil, false, false
		}
		select {
		case <-q.wake:
		case <-time.After(remaining):
		}
	}
}

func (q *audioQueue) empty() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items) == 0
}

// The provider specific half of a live session, run on the session worker
type streamingDriver interface {
	session(s *StreamingSession) (map[string]any, error)
}

// StreamingSession feeds 16 kHz PCM to a live STT provider on one worker and reports partial and final text
type StreamingSession struct {
	provider  string
	onPartial func(string)
	audio     *audioQueue
	done      *syncutil.Event
	cancelled *syncutil.Event
	mu        sync.Mutex
	result    map[string]any
	partial   string
	to16k     *Resampler
	driver    streamingDriver
	ctx       context.Context
}

func newStreamingSession(ctx context.Context, provider string, onPartial func(string), driver streamingDriver) *StreamingSession {
	return &StreamingSession{provider: provider, onPartial: onPartial, audio: newAudioQueue(), done: &syncutil.Event{},
		cancelled: &syncutil.Event{}, result: map[string]any{}, driver: driver, ctx: ctx}
}

// Provider name of the session
func (s *StreamingSession) Provider() string { return s.provider }

// SetInputRate resamples pushed audio to 16 kHz when the capture rate differs
func (s *StreamingSession) SetInputRate(rate int) {
	if rate != streamingInputRate {
		s.mu.Lock()
		s.to16k = NewResampler(rate, streamingInputRate)
		s.mu.Unlock()
	}
}

// Start runs the provider worker
func (s *StreamingSession) Start() *StreamingSession {
	go s.run()
	return s
}

// PushAudio queues PCM until the session finishes
func (s *StreamingSession) PushAudio(pcm []byte) {
	if len(pcm) > 0 && !s.done.IsSet() {
		s.audio.put(append([]byte(nil), pcm...), false)
	}
}

// EndAudio marks the end of speech so the provider flushes
func (s *StreamingSession) EndAudio() { s.audio.put(nil, true) }

// Finalize waits for the transcript envelope, cancelling with an error envelope when the timeout passes
func (s *StreamingSession) Finalize(timeout float64) map[string]any {
	s.EndAudio()
	if !s.done.WaitTimeout(time.Duration(timeout * float64(time.Second))) {
		s.Cancel()
		return s.errorEnvelope("live transcription timed out")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]any, len(s.result))
	for k, v := range s.result {
		out[k] = v
	}
	return out
}

// Cancel stops the worker and finishes with a cancelled envelope
func (s *StreamingSession) Cancel() {
	s.cancelled.Set()
	s.audio.put(nil, true)
	s.finish(s.errorEnvelope("cancelled"))
}

// PartialTranscript is the latest non final text
func (s *StreamingSession) PartialTranscript() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.partial
}

// Records a changed partial and hands it to the partial callback, a failing callback logged
func (s *StreamingSession) setPartial(text string) {
	text = strings.TrimSpace(text)
	s.mu.Lock()
	changed := text != "" && text != s.partial
	if changed {
		s.partial = text
	}
	s.mu.Unlock()
	if changed && s.onPartial != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					pkgLog().Debug(fmt.Sprintf("on_partial callback raised: %v", r))
				}
			}()
			s.onPartial(text)
		}()
	}
}

func (s *StreamingSession) errorEnvelope(message string) map[string]any {
	return map[string]any{"success": false, "transcript": "", "provider": s.provider, "error": message}
}

// Stores the first envelope and wakes Finalize
func (s *StreamingSession) finish(result map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.done.IsSet() {
		s.result = result
		s.done.Set()
	}
}

// Worker body turning every failure into the error envelope
func (s *StreamingSession) run() {
	result, err := func() (result map[string]any, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		return s.driver.session(s)
	}()
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("Live STT (%s) failed: %s", s.provider, err))
		s.finish(s.errorEnvelope(fmt.Sprintf("live transcription failed: %s", err)))
		return
	}
	s.finish(result)
}

// One websocket wire, the hooks the shared send and receive loop calls
type wsWire interface {
	open(ws *wsConn) error
	ready() bool
	sendAudio(ws *wsConn, chunk []byte) error
	end(ws *wsConn) error
	event(event *jsonutil.Object) (map[string]any, error)
}

// Websocket with a reader goroutine so receives can time out without breaking the connection
type wsConn struct {
	conn     *websocket.Conn
	messages chan wsMessage
}

type wsMessage struct {
	binary bool
	data   []byte
	err    error
}

func (c *wsConn) sendText(text string) error {
	return c.conn.WriteMessage(websocket.TextMessage, []byte(text))
}

func (c *wsConn) sendJSON(v any) error {
	text, err := jsonutil.Encode(v, jsonutil.Spaced(), jsonutil.ASCII())
	if err != nil {
		return err
	}
	return c.sendText(text)
}

// Next message within the timeout, ok false when none arrived in time
func (c *wsConn) recv(timeout time.Duration) (wsMessage, bool) {
	select {
	case m := <-c.messages:
		return m, true
	case <-time.After(timeout):
		return wsMessage{}, false
	}
}

type wsDriver struct {
	url     string
	headers http.Header
	wire    wsWire
	backlog [][]byte
}

// Sends the audio held back until the provider was ready
func (d *wsDriver) flushBacklog(ws *wsConn) error {
	backlog := d.backlog
	d.backlog = nil
	for _, chunk := range backlog {
		if err := d.wire.sendAudio(ws, chunk); err != nil {
			return err
		}
	}
	return nil
}

// Websocket URL of a base URL with the path suffix and query, wss for https bases
func streamingURL(baseURL, suffix, query string) (string, error) {
	parts, err := urlutil.Split(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return "", err
	}
	scheme := "ws"
	if parts.Scheme == "https" {
		scheme = "wss"
	}
	return urlutil.Parts{Scheme: scheme, Authority: parts.Authority, Path: parts.Path + suffix, Query: query}.String(), nil
}

type openAIRealtimeWire struct {
	s         *StreamingSession
	model     string
	language  string
	prompt    string
	resample  *Resampler
	text      string
	sentAudio bool
}

// NewOpenAIRealtimeSession is a Realtime transcription session resampling 16 kHz input to 24 kHz
func NewOpenAIRealtimeSession(ctx context.Context, apiKey, baseURL, model, language, prompt string, onPartial func(string)) (*StreamingSession, error) {
	u, err := streamingURL(baseURL, "/realtime", "intent=transcription")
	if err != nil {
		return nil, err
	}
	w := &openAIRealtimeWire{model: model, language: language, prompt: prompt, resample: NewResampler(streamingInputRate, 24000)}
	d := &wsDriver{url: u, headers: http.Header{"Authorization": {"Bearer " + apiKey}}, wire: w}
	w.s = newStreamingSession(ctx, "openai", onPartial, d)
	return w.s, nil
}

func (w *openAIRealtimeWire) open(ws *wsConn) error {
	transcription := jsonObject("model", w.model)
	if w.language != "" {
		if w.model == defaultOpenAIStreamingModel {
			transcription.Set("languages", []any{w.language})
		} else {
			transcription.Set("language", w.language)
		}
	}
	if w.prompt != "" {
		transcription.Set("prompt", w.prompt)
	}
	return ws.sendJSON(jsonObject("type", "session.update", "session", jsonObject("type", "transcription", "audio",
		jsonObject("input", jsonObject("format", jsonObject("type", "audio/pcm", "rate", 24000),
			"transcription", transcription, "turn_detection", nil)))))
}

func (w *openAIRealtimeWire) ready() bool { return true }

// Commits the buffer, or finishes empty at once when no audio was sent
func (w *openAIRealtimeWire) end(ws *wsConn) error {
	if !w.sentAudio {
		w.s.finish(map[string]any{"success": true, "transcript": "", "provider": "openai"})
		w.s.cancelled.Set()
		return nil
	}
	return ws.sendJSON(jsonObject("type", "input_audio_buffer.commit"))
}

// Python str of an event field, empty when missing, None or falsy
func eventText(event *jsonutil.Object, key string) string {
	v, _ := event.Get(key)
	if !anyutil.IsTrue(v) {
		return ""
	}
	return anyutil.String(v)
}

func (w *openAIRealtimeWire) event(event *jsonutil.Object) (map[string]any, error) {
	kind := eventText(event, "type")
	switch {
	case strings.HasSuffix(kind, "input_audio_transcription.delta"):
		w.text += eventText(event, "delta")
		w.s.setPartial(w.text)
	case strings.HasSuffix(kind, "input_audio_transcription.completed"):
		transcript := eventText(event, "transcript")
		if transcript == "" {
			transcript = w.text
		}
		return map[string]any{"success": true, "transcript": strings.TrimSpace(transcript), "provider": "openai"}, nil
	case kind == "error":
		message := "provider error"
		if errObj, ok := event.Get("error"); ok && anyutil.IsTrue(errObj) {
			o, isObj := errObj.(*jsonutil.Object)
			if !isObj {
				return nil, fmt.Errorf("OpenAI realtime error is %s where a mapping was expected", errchain.Kind(errObj))
			}
			if m := eventText(o, "message"); m != "" {
				message = m
			}
		}
		if strings.Contains(strings.ToLower(message), "buffer too small") {
			return map[string]any{"success": true, "transcript": "", "provider": "openai"}, nil
		}
		return w.s.errorEnvelope(message), nil
	}
	return nil, nil
}

type xaiStreamingWire struct {
	s          *StreamingSession
	created    bool
	utterances []string
	chunks     []string
	interim    string
	driver     *wsDriver
}

// NewXAIStreamingSession is an xAI STT websocket session with interim results
func NewXAIStreamingSession(ctx context.Context, apiKey, baseURL, model, language string, onPartial func(string)) (*StreamingSession, error) {
	query := [][2]string{{"sample_rate", strconv.Itoa(streamingInputRate)}, {"encoding", "pcm"}, {"interim_results", "true"}, {"model", model}}
	if language != "" {
		query = append(query, [2]string{"language", language})
	}
	u, err := streamingURL(baseURL, "/stt", urlutil.EncodeQuery(query))
	if err != nil {
		return nil, err
	}
	w := &xaiStreamingWire{}
	w.driver = &wsDriver{url: u, headers: http.Header{"Authorization": {"Bearer " + apiKey}}, wire: w}
	w.s = newStreamingSession(ctx, "xai", onPartial, w.driver)
	return w.s, nil
}

func (w *xaiStreamingWire) open(*wsConn) error { return nil }

// The server drops audio sent before transcript.created
func (w *xaiStreamingWire) ready() bool { return w.created }

func (w *xaiStreamingWire) sendAudio(ws *wsConn, chunk []byte) error {
	return ws.conn.WriteMessage(websocket.BinaryMessage, chunk)
}

func (w *xaiStreamingWire) end(ws *wsConn) error {
	if !w.created {
		w.driver.backlog = nil
	}
	return ws.sendJSON(jsonObject("type", "audio.done"))
}

func joinNonEmpty(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.TrimSpace(strings.Join(kept, " "))
}

func (w *xaiStreamingWire) text() string {
	return joinNonEmpty(append(append(append([]string(nil), w.utterances...), w.chunks...), w.interim)...)
}

func (w *xaiStreamingWire) event(event *jsonutil.Object) (map[string]any, error) {
	kind, _ := event.Get("type")
	switch kind {
	case "transcript.created":
		w.created = true
	case "transcript.partial":
		text := strings.TrimSpace(eventText(event, "text"))
		speechFinal, _ := event.Get("speech_final")
		isFinal, _ := event.Get("is_final")
		switch {
		case anyutil.IsTrue(speechFinal):
			w.utterances = append(w.utterances, text)
			w.chunks, w.interim = nil, ""
		case anyutil.IsTrue(isFinal):
			w.chunks = append(w.chunks, text)
			w.interim = ""
		default:
			w.interim = text
		}
		w.s.setPartial(w.text())
	case "transcript.done":
		return map[string]any{"success": true, "transcript": w.text(), "provider": "xai"}, nil
	case "error":
		message := eventText(event, "message")
		if message == "" {
			message = "provider error"
		}
		return w.s.errorEnvelope(message), nil
	}
	return nil, nil
}

type elevenLabsRealtimeWire struct {
	s         *StreamingSession
	committed []string
	interim   string
	ending    bool
}

// NewElevenLabsRealtimeSession is an ElevenLabs Scribe realtime session committing on VAD
func NewElevenLabsRealtimeSession(ctx context.Context, apiKey, baseURL, language string, onPartial func(string)) (*StreamingSession, error) {
	query := [][2]string{{"model_id", elevenLabsRealtimeModel}, {"audio_format", "pcm_" + strconv.Itoa(streamingInputRate)}, {"commit_strategy", "vad"}}
	if language != "" {
		query = append(query, [2]string{"language_code", language})
	}
	u, err := streamingURL(baseURL, "/speech-to-text/realtime", urlutil.EncodeQuery(query))
	if err != nil {
		return nil, err
	}
	w := &elevenLabsRealtimeWire{}
	d := &wsDriver{url: u, headers: http.Header{"Xi-Api-Key": {apiKey}}, wire: w}
	w.s = newStreamingSession(ctx, "elevenlabs", onPartial, d)
	return w.s, nil
}

func (w *elevenLabsRealtimeWire) chunk(pcm []byte, commit bool) *jsonutil.Object {
	return jsonObject("message_type", "input_audio_chunk", "audio_base_64", base64.StdEncoding.EncodeToString(pcm),
		"commit", commit, "sample_rate", streamingInputRate)
}

func (w *elevenLabsRealtimeWire) open(*wsConn) error { return nil }
func (w *elevenLabsRealtimeWire) ready() bool        { return true }
func (w *elevenLabsRealtimeWire) sendAudio(ws *wsConn, chunk []byte) error {
	return ws.sendJSON(w.chunk(chunk, false))
}

func (w *elevenLabsRealtimeWire) end(ws *wsConn) error {
	w.ending = true
	return ws.sendJSON(w.chunk(nil, true))
}

func (w *elevenLabsRealtimeWire) text() string {
	return joinNonEmpty(append(append([]string(nil), w.committed...), w.interim)...)
}

func (w *elevenLabsRealtimeWire) event(event *jsonutil.Object) (map[string]any, error) {
	raw, _ := event.Get("message_type")
	kind := ""
	if anyutil.IsTrue(raw) {
		kind = anyutil.String(raw)
	}
	switch {
	case kind == "partial_transcript":
		w.interim = strings.TrimSpace(eventText(event, "text"))
		w.s.setPartial(w.text())
	case kind == "committed_transcript":
		if text := strings.TrimSpace(eventText(event, "text")); text != "" {
			w.committed = append(w.committed, text)
		}
		w.interim = ""
		w.s.setPartial(w.text())
		if w.ending {
			return map[string]any{"success": true, "transcript": w.text(), "provider": "elevenlabs"}, nil
		}
	case (kind == "insufficient_audio_activity" || kind == "commit_throttled") && w.ending:
		return map[string]any{"success": true, "transcript": w.text(), "provider": "elevenlabs"}, nil
	case kind != "" && kind != "session_started" && kind != "committed_transcript_with_timestamps" && kind != "warning":
		if errValue, ok := event.Get("error"); ok {
			return w.s.errorEnvelope(fmt.Sprintf("%s: %s", kind, anyutil.String(errValue))), nil
		}
	}
	return nil, nil
}

type pluginStreamDriver struct {
	inner TranscriptionStreamSession
}

// OpenAI key and base URL for live STT, absent when unresolvable
func openAIStreamingCredentials(ctx context.Context) (string, string, bool, error) {
	key, base, err := resolveSTTOpenAIAudioClientConfig(ctx)
	var valueErr *sttValueError
	if errors.As(err, &valueErr) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return key, base, true, nil
}

// Built in live session for openai, xai or elevenlabs, nil when its credentials are missing
func builtinStreamingSession(ctx context.Context, providerName string, req *sttRequest, language, prompt string, onPartial func(string)) (*StreamingSession, error) {
	switch providerName {
	case "openai":
		key, base, ok, err := openAIStreamingCredentials(ctx)
		if err != nil || !ok {
			return nil, err
		}
		model := textOr(req.OpenAI.StreamingModel, defaultOpenAIStreamingModel)
		return NewOpenAIRealtimeSession(ctx, key, base, model, language, prompt, onPartial)
	case "xai":
		apiKey := strings.TrimSpace(sttEnvValue(ctx, "XAI_API_KEY"))
		if apiKey == "" {
			return nil, nil
		}
		base := textOr(req.XAI.BaseURL, xaiSTTBaseURL)
		return NewXAIStreamingSession(ctx, apiKey, base, normalizeXAISTTModel(strings.TrimSpace(req.XAI.Model)), language, onPartial)
	case "elevenlabs":
		apiKey := resolveSTTProviderKey(ctx, "ELEVENLABS_API_KEY", "elevenlabs")
		if apiKey == "" {
			return nil, nil
		}
		base := textOr(req.ElevenLabs.BaseURL, elevenLabsSTTBaseURL)
		return NewElevenLabsRealtimeSession(ctx, apiKey, base, language, onPartial)
	}
	return nil, nil
}

// Live session of a streaming capable plugin provider, nil otherwise
func pluginStreamingSession(ctx context.Context, providerName, language, prompt string, onPartial func(string)) (*StreamingSession, error) {
	registered := GetTranscriptionProvider(ctx, providerName, "")
	if registered == nil || !registered.StreamingCapable() {
		return nil, nil
	}
	var lang, pr any
	if language != "" {
		lang = language
	}
	if prompt != "" {
		pr = prompt
	}
	inner, err := registered.OpenStreamSession(ctx, lang, pr)
	if err != nil {
		return nil, err
	}
	return newStreamingSession(ctx, providerName, onPartial, &pluginStreamDriver{inner: inner}), nil
}

// OpenStreamingSession starts a live session for the active provider, nil when streaming is off or none opens
func OpenStreamingSession(ctx context.Context, onPartial func(string)) (*StreamingSession, error) {
	req := loadSTTRequest(ctx)
	if !req.Streaming {
		return nil, nil
	}
	providerName, err := getProvider(ctx, req)
	if err != nil {
		return nil, err
	}
	if providerName == "none" || providerName == "local" || providerName == "local_command" {
		return nil, nil
	}
	language := resolveSTTLanguage(ctx, providerName, req)
	prompt := strings.TrimSpace(req.Prompt)
	session, err := func() (session *StreamingSession, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		if streamingProviders[providerName] {
			return builtinStreamingSession(ctx, providerName, req, language, prompt, onPartial)
		}
		return pluginStreamingSession(ctx, providerName, language, prompt, onPartial)
	}()
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("Live STT session for %s could not open: %s", providerName, err))
		return nil, nil
	}
	if session == nil {
		return nil, nil
	}
	return session.Start(), nil
}
