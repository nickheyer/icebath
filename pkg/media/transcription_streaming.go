package media

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	streamingInputRate          = 16000
	defaultOpenAIStreamingModel = "gpt-live-transcribe"
	elevenLabsRealtimeModel     = "scribe_v2_realtime"
	streamingFinalizeTimeout    = 20.0
	streamingConnectTimeout     = 10 * time.Second
)

var (
	streamingProviders = map[string]bool{"openai": true, "xai": true, "elevenlabs": true}
	streamMonotonic    = pythread.Monotonic
)

func streamLog() *slog.Logger { return slog.Default().With("logger", "tools.transcription_streaming") }

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

// Resample converts one chunk, carrying the unconsumed tail into the next call
func (r *Resampler) Resample(pcm []byte) []byte {
	if r.src == r.dst || len(pcm) == 0 {
		return pcm
	}
	data := append(append([]int16(nil), r.carry...), pcmInt16(pcm[:len(pcm)-len(pcm)%2])...)
	if r.src%r.dst == 0 {
		step := r.src / r.dst
		usable := len(data) - len(data)%step
		out := make([]int16, 0, usable/step)
		for i := 0; i < usable; i += step {
			sum := 0
			for _, v := range data[i : i+step] {
				sum += int(v)
			}
			out = append(out, int16(pyFloorDiv(sum, step)))
		}
		r.carry = data[usable:]
		return int16Bytes(out)
	}
	ratio := float64(r.src) / float64(r.dst)
	pos, last := r.pos, len(data)-1
	var out []int16
	for pos < float64(last) {
		i := int(pos)
		frac := pos - float64(i)
		out = append(out, int16(float64(data[i])+float64(float64(int(data[i+1])-int(data[i]))*frac)))
		pos += ratio
	}
	keep := int(pos)
	r.carry = data[min(keep, len(data)):]
	r.pos = pos - float64(keep)
	return int16Bytes(out)
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
	done      *pythread.Event
	cancelled *pythread.Event
	mu        sync.Mutex
	result    map[string]any
	partial   string
	to16k     *Resampler
	driver    streamingDriver
	ctx       context.Context
}

func newStreamingSession(ctx context.Context, provider string, onPartial func(string), driver streamingDriver) *StreamingSession {
	return &StreamingSession{provider: provider, onPartial: onPartial, audio: newAudioQueue(), done: &pythread.Event{},
		cancelled: &pythread.Event{}, result: map[string]any{}, driver: driver, ctx: ctx}
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

// Next queued chunk resampled to 16 kHz, end set for the sentinel, an empty chunk when nothing is queued
func (s *StreamingSession) takeAudio(block bool, timeout float64) ([]byte, bool) {
	chunk, end, ok := s.audio.get(block, timeout)
	if !ok {
		return []byte{}, false
	}
	if end {
		return nil, true
	}
	s.mu.Lock()
	resampler := s.to16k
	s.mu.Unlock()
	if len(chunk) > 0 && resampler != nil {
		chunk = resampler.Resample(chunk)
	}
	return chunk, false
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
	if !s.done.Wait(timeout) {
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
	text = pystr.Strip(text)
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
					streamLog().Debug(fmt.Sprintf("on_partial callback raised: %v", r))
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
		streamLog().Warn(fmt.Sprintf("Live STT (%s) failed: %s", s.provider, err))
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
	event(event *pyjson.Object) (map[string]any, error)
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
	text, err := pyjson.Dumps(v)
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

// Ordered JSON object from alternating keys and values
func jsonObject(pairs ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i < len(pairs); i += 2 {
		o.Set(pairs[i].(string), pairs[i+1])
	}
	return o
}

// Connects, runs the loop, then closes the socket in the background
func (d *wsDriver) session(s *StreamingSession) (map[string]any, error) {
	dialer := websocket.Dialer{HandshakeTimeout: streamingConnectTimeout, Proxy: http.ProxyFromEnvironment}
	conn, resp, err := dialer.DialContext(s.ctx, d.url, d.headers)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	ws := &wsConn{conn: conn, messages: make(chan wsMessage, 64)}
	stopped := make(chan struct{})
	go func() {
		for {
			kind, data, err := conn.ReadMessage()
			select {
			case ws.messages <- wsMessage{binary: kind == websocket.BinaryMessage, data: data, err: err}:
			case <-stopped:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() {
		close(stopped)
		go func() {
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
			conn.Close()
		}()
	}()
	return d.loop(s, ws)
}

// Streams audio while reading events until one yields the envelope or the provider stalls past the deadline
func (d *wsDriver) loop(s *StreamingSession, ws *wsConn) (map[string]any, error) {
	d.backlog = nil
	if err := d.wire.open(ws); err != nil {
		return nil, err
	}
	endingSince, ending := 0.0, false
	for {
		if s.cancelled.IsSet() {
			return s.errorEnvelope("cancelled"), nil
		}
		if !ending {
			since, ended, err := d.drainAudio(s, ws)
			if err != nil {
				return nil, err
			}
			endingSince, ending = since, ended
		} else if streamMonotonic()-endingSince > streamingFinalizeTimeout {
			return s.errorEnvelope("provider never finalized the transcript"), nil
		}
		msg, ok := ws.recv(20 * time.Millisecond)
		if !ok {
			continue
		}
		if msg.err != nil {
			return nil, msg.err
		}
		if msg.binary {
			continue
		}
		decoded, err := pyjson.Loads(string(msg.data))
		if err != nil {
			return nil, err
		}
		event, isObject := decoded.(*pyjson.Object)
		if !isObject {
			return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(decoded))
		}
		result, err := d.wire.event(event)
		if err != nil {
			return nil, err
		}
		if result != nil {
			s.finish(result)
			return result, nil
		}
		if len(d.backlog) > 0 && d.wire.ready() {
			if err := d.flushBacklog(ws); err != nil {
				return nil, err
			}
		}
	}
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

// Sends or holds every queued chunk, reporting the end of audio time once the sentinel is sent on
func (d *wsDriver) drainAudio(s *StreamingSession, ws *wsConn) (float64, bool, error) {
	for {
		chunk, end := s.takeAudio(false, 0)
		if end {
			if err := d.wire.end(ws); err != nil {
				return 0, false, err
			}
			return streamMonotonic(), true, nil
		}
		if len(chunk) == 0 {
			if s.audio.empty() {
				return 0, false, nil
			}
			continue
		}
		if d.wire.ready() {
			if err := d.wire.sendAudio(ws, chunk); err != nil {
				return 0, false, err
			}
		} else {
			d.backlog = append(d.backlog, chunk)
		}
	}
}

// Websocket URL of a base URL with the path suffix and query, wss for https bases
func streamingURL(baseURL, suffix, query string) (string, error) {
	parts, err := pyurl.Split(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return "", err
	}
	scheme := "ws"
	if parts.Scheme == "https" {
		scheme = "wss"
	}
	return pyurl.Unsplit(pyurl.URL{Scheme: scheme, Netloc: parts.Netloc, Path: parts.Path + suffix, Query: query}), nil
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

func (w *openAIRealtimeWire) sendAudio(ws *wsConn, chunk []byte) error {
	pcm := w.resample.Resample(chunk)
	if len(pcm) == 0 {
		return nil
	}
	w.sentAudio = true
	return ws.sendJSON(jsonObject("type", "input_audio_buffer.append", "audio", base64.StdEncoding.EncodeToString(pcm)))
}

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
func eventText(event *pyjson.Object, key string) string {
	v, _ := event.Get(key)
	if !pyTruthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

func (w *openAIRealtimeWire) event(event *pyjson.Object) (map[string]any, error) {
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
		return map[string]any{"success": true, "transcript": pystr.Strip(transcript), "provider": "openai"}, nil
	case kind == "error":
		message := "provider error"
		if errObj, ok := event.Get("error"); ok && pyTruthy(errObj) {
			o, isObj := errObj.(*pyjson.Object)
			if !isObj {
				return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(errObj))
			}
			if m := eventText(o, "message"); m != "" {
				message = m
			}
		}
		if strings.Contains(pystr.Lower(message), "buffer too small") {
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
	u, err := streamingURL(baseURL, "/stt", pyurl.URLEncode(query))
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
	return pystr.Strip(strings.Join(kept, " "))
}

func (w *xaiStreamingWire) text() string {
	return joinNonEmpty(append(append(append([]string(nil), w.utterances...), w.chunks...), w.interim)...)
}

func (w *xaiStreamingWire) event(event *pyjson.Object) (map[string]any, error) {
	kind, _ := event.Get("type")
	switch kind {
	case "transcript.created":
		w.created = true
	case "transcript.partial":
		text := pystr.Strip(eventText(event, "text"))
		speechFinal, _ := event.Get("speech_final")
		isFinal, _ := event.Get("is_final")
		switch {
		case pyTruthy(speechFinal):
			w.utterances = append(w.utterances, text)
			w.chunks, w.interim = nil, ""
		case pyTruthy(isFinal):
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
	u, err := streamingURL(baseURL, "/speech-to-text/realtime", pyurl.URLEncode(query))
	if err != nil {
		return nil, err
	}
	w := &elevenLabsRealtimeWire{}
	d := &wsDriver{url: u, headers: http.Header{"Xi-Api-Key": {apiKey}}, wire: w}
	w.s = newStreamingSession(ctx, "elevenlabs", onPartial, d)
	return w.s, nil
}

func (w *elevenLabsRealtimeWire) chunk(pcm []byte, commit bool) *pyjson.Object {
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

func (w *elevenLabsRealtimeWire) event(event *pyjson.Object) (map[string]any, error) {
	raw, _ := event.Get("message_type")
	kind := ""
	if pyTruthy(raw) {
		kind = pyrepr.Str(raw)
	}
	switch {
	case kind == "partial_transcript":
		w.interim = pystr.Strip(eventText(event, "text"))
		w.s.setPartial(w.text())
	case kind == "committed_transcript":
		if text := pystr.Strip(eventText(event, "text")); text != "" {
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
			return w.s.errorEnvelope(fmt.Sprintf("%s: %s", kind, pyrepr.Str(errValue))), nil
		}
	}
	return nil, nil
}

type pluginStreamDriver struct {
	inner TranscriptionStreamSession
}

// Pumps audio into a plugin stream session and polls its partial transcript
func (d *pluginStreamDriver) session(s *StreamingSession) (map[string]any, error) {
	for {
		chunk, end := s.takeAudio(true, 0.25)
		if s.cancelled.IsSet() {
			return s.errorEnvelope("cancelled"), nil
		}
		if end {
			result, err := d.inner.Finalize()
			if err != nil {
				return nil, err
			}
			out := make(map[string]any, len(result))
			for k, v := range result {
				out[k] = v
			}
			return out, nil
		}
		if len(chunk) > 0 {
			if err := d.inner.PushAudio(chunk); err != nil {
				return nil, err
			}
		}
		partial, err := d.inner.PartialTranscript()
		if err != nil {
			return nil, err
		}
		s.setPartial(partial)
	}
}

func streamingEnabled(sttConfig map[string]any) bool {
	v, ok := sttConfig["streaming"]
	if !ok || v == nil {
		return false
	}
	return provider.IsTruthyValue(v)
}

// OpenAI key and base URL for live STT, absent under the managed Nous selection or when unresolvable
func openAIStreamingCredentials(ctx context.Context) (string, string, bool, error) {
	if sttReadSelection(ctx) == NousManagedProvider {
		return "", "", false, nil
	}
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
func builtinStreamingSession(ctx context.Context, providerName string, sttConfig map[string]any, language, prompt string, onPartial func(string)) (*StreamingSession, error) {
	section, _ := asStrMap(sttConfig[providerName])
	if section == nil {
		section = map[string]any{}
	}
	sectionText := func(key string) string {
		if v := section[key]; pyTruthy(v) {
			return pyrepr.Str(v)
		}
		return ""
	}
	switch providerName {
	case "openai":
		key, base, ok, err := openAIStreamingCredentials(ctx)
		if err != nil || !ok {
			return nil, err
		}
		model := sectionText("streaming_model")
		if model == "" {
			model = defaultOpenAIStreamingModel
		}
		return NewOpenAIRealtimeSession(ctx, key, base, model, language, prompt, onPartial)
	case "xai":
		apiKey := pystr.Strip(sttEnvValue(ctx, "XAI_API_KEY"))
		if apiKey == "" {
			return nil, nil
		}
		base := sectionText("base_url")
		if base == "" {
			base = sttEnvValue(ctx, "XAI_STT_BASE_URL")
		}
		if base == "" {
			base = xaiSTTBaseURL
		}
		model := ""
		if v := section["model"]; pyTruthy(v) {
			model = pyrepr.Str(v)
		}
		return NewXAIStreamingSession(ctx, apiKey, base, normalizeXAISTTModel(model), language, onPartial)
	case "elevenlabs":
		apiKey := resolveSTTProviderKey(ctx, "ELEVENLABS_API_KEY", "elevenlabs")
		if apiKey == "" {
			return nil, nil
		}
		base := sectionText("base_url")
		if base == "" {
			base = elevenLabsSTTBaseURL
		}
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

// StreamingAvailable reports whether live STT is enabled and the active provider can open a session
func StreamingAvailable(ctx context.Context, sttConfig map[string]any) (bool, error) {
	if sttConfig == nil {
		sttConfig = loadSTTConfig(ctx)
	}
	if !streamingEnabled(sttConfig) {
		return false, nil
	}
	providerName, err := getProvider(ctx, sttConfig)
	if err != nil {
		return false, err
	}
	if streamingProviders[providerName] {
		session, err := builtinStreamingSession(ctx, providerName, sttConfig, "", "", nil)
		return session != nil, err
	}
	registered := GetTranscriptionProvider(ctx, providerName, "")
	return registered != nil && registered.StreamingCapable(), nil
}

// OpenStreamingSession starts a live session for the active provider, nil when streaming is off or none opens
func OpenStreamingSession(ctx context.Context, onPartial func(string), sttConfig map[string]any) (*StreamingSession, error) {
	if sttConfig == nil {
		sttConfig = loadSTTConfig(ctx)
	}
	if !streamingEnabled(sttConfig) {
		return nil, nil
	}
	providerName, err := getProvider(ctx, sttConfig)
	if err != nil {
		return nil, err
	}
	if providerName == "none" || providerName == "local" || providerName == "local_command" {
		return nil, nil
	}
	var extra []string
	if providerName == "elevenlabs" {
		extra = []string{"language_code"}
	}
	language := resolveSTTLanguage(ctx, providerName, sttConfig, extra...)
	prompt := ""
	if v := sttConfig["prompt"]; pyTruthy(v) {
		prompt = pystr.Strip(pyrepr.Str(v))
	}
	session, err := func() (session *StreamingSession, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		if streamingProviders[providerName] {
			return builtinStreamingSession(ctx, providerName, sttConfig, language, prompt, onPartial)
		}
		return pluginStreamingSession(ctx, providerName, language, prompt, onPartial)
	}()
	if err != nil {
		streamLog().Warn(fmt.Sprintf("Live STT session for %s could not open: %s", providerName, err))
		return nil, nil
	}
	if session == nil {
		return nil, nil
	}
	return session.Start(), nil
}
